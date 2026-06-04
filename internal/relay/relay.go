package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/plugins"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// Handler returns a Gin handler that processes inbound requests and forwards them to the upstream service.
func Handler(inboundType llm.APIFormat) gin.HandlerFunc {
	inAdapter := newInbound(inboundType)
	return func(c *gin.Context) {
		run, err := newRelayRun(c, inboundType, inAdapter)
		if err != nil {
			return
		}
		run.run()
	}
}

func newRelayRun(c *gin.Context, inboundType llm.APIFormat, inAdapter transformer.Inbound) (*relayRun, error) {
	internalRequest, err := parseRequest(c, inboundType, inAdapter)
	if err != nil {
		return nil, err
	}

	if supportedModels := c.GetString("supported_models"); supportedModels != "" {
		// intersect with currently available models, filter out disabled model names
		availableModels, _ := op.GroupListModel(c.Request.Context())
		supportedModelsArray := lo.Map(strings.Split(supportedModels, ","), func(s string, _ int) string {
			return strings.TrimSpace(s)
		})
		effectiveModels := lo.Filter(supportedModelsArray, func(m string, _ int) bool {
			return lo.Contains(availableModels, m)
		})
		// empty intersection → all specified models have expired, treat as unlimited
		if len(effectiveModels) > 0 && !lo.Contains(effectiveModels, internalRequest.Model) {
			log.Debugf("unsupported model: model=%s supported_models=%s effective=%v", internalRequest.Model, supportedModels, effectiveModels)
			err := errors.New("unsupported model")
			resp.Error(c, http.StatusBadRequest, err.Error())
			return nil, err
		}
	}

	group, err := op.GroupGetEnabledMap(internalRequest.Model, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "model not found")
		return nil, err
	}

	apiKeyID := c.GetInt("api_key_id")
	iter := balancer.NewIterator(group, apiKeyID, internalRequest.Model)
	if iter.Len() == 0 {
		err := errors.New("no available channel")
		resp.Error(c, http.StatusServiceUnavailable, err.Error())
		return nil, err
	}

	return &relayRun{
		c:               c,
		inAdapter:       inAdapter,
		internalRequest: internalRequest,
		metrics: &RelayMetrics{
			APIKeyID:        apiKeyID,
			RequestModel:    internalRequest.Model,
			ActualModel:     internalRequest.Model,
			StartTime:       time.Now(),
			InternalRequest: internalRequest,
		},
		iter:  iter,
		group: group,
	}, nil
}

func (r *relayRun) run() {
	ctx := r.c.Request.Context()
	var lastErr error

	for r.iter.Next() {
		select {
		case <-ctx.Done():
			log.Debugf("request context canceled, stopping retry")
			r.metrics.Save(ctx, false, context.Canceled, r.iter.Attempts())
			return
		default:
		}

		attempt, err := r.prepareAttempt()
		if err != nil {
			lastErr = err
			continue
		}
		if attempt == nil {
			continue
		}

		written, err := attempt.run()
		if err == nil {
			r.metrics.Save(ctx, true, nil, r.iter.Attempts())
			return
		}
		if written {
			r.metrics.Save(ctx, false, err, r.iter.Attempts())
			return
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = errors.New("all channels failed")
	}
	r.metrics.Save(ctx, false, lastErr, r.iter.Attempts())
	resp.Error(r.c, http.StatusBadGateway, lastErr.Error())
}

func (r *relayRun) prepareAttempt() (*relayAttempt, error) {
	item := r.iter.Item()
	channel, err := op.ChannelGet(item.ChannelID, r.c.Request.Context())
	if err != nil {
		log.Debugf("failed to get channel %d: %v", item.ChannelID, err)
		r.iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
		return nil, err
	}
	if !channel.Enabled {
		r.iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
		return nil, nil
	}

	usedKey := channel.GetChannelKey()
	if usedKey.ChannelKey == "" {
		r.iter.Skip(channel.ID, 0, channel.Name, "no available key")
		return nil, nil
	}
	if r.iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
		return nil, nil
	}

	outAdapter, err := newOutbound(channel.Type, r.internalRequest, channel.GetBaseUrl(), usedKey.ChannelKey)
	if err != nil {
		r.iter.Skip(channel.ID, usedKey.ID, channel.Name, err.Error())
		return nil, nil
	}

	// set client model to the current candidate's actual upstream model on each attempt; retry will overwrite with next candidate.
	r.internalRequest.Model = item.ModelName
	r.metrics.ActualModel = item.ModelName
	r.metrics.ParamOverride = ""
	log.Debugf("forwarding to channel: model=%s mode=%d channel=%s upstream_model=%s key=%s (attempt %d/%d, sticky=%t)",
		r.metrics.RequestModel, r.group.Mode, channel.Name, item.ModelName,
		helper.MaskKeySuffix(usedKey.ChannelKey),
		r.iter.Index()+1, r.iter.Len(), r.iter.IsSticky())

	return &relayAttempt{
		relayRun:   r,
		outAdapter: outAdapter,
		channel:    channel,
		usedKey:    usedKey,
	}, nil
}

// run manages the complete lifecycle of a single channel attempt.
// Returns (written, error): written=true means the response has been written back to the client, caller should not retry.
func (ra *relayAttempt) run() (bool, error) {
	span := ra.iter.StartAttempt(ra.channel.ID, ra.usedKey.ID, ra.channel.Name)

	upstreamStatusCode, fwdErr := ra.forward()
	if fwdErr == nil && upstreamStatusCode == 0 {
		upstreamStatusCode = http.StatusOK
	}
	ra.usedKey.StatusCode = upstreamStatusCode
	ra.usedKey.LastUseTimeStamp = time.Now().Unix()

	// success path
	if fwdErr == nil {
		ra.usedKey.TotalCost += ra.metrics.Stats.InputCost + ra.metrics.Stats.OutputCost
		ra.usedKey.ConsecutiveAuthErrors = 0 // reset auth error count on success
		op.ChannelKeyUpdate(ra.usedKey)

		span.End(dbmodel.AttemptSuccess, "")
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:       span.Duration().Milliseconds(),
			RequestSuccess: 1,
		})
		balancer.RecordSuccess(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		balancer.SetSticky(ra.metrics.APIKeyID, ra.metrics.RequestModel, ra.channel.ID, ra.usedKey.ID)
		return false, nil
	}

	// error path: handle by status code
	statusCode := upstreamStatusCode
	if statusCode == 0 {
		statusCode = ra.statusCode
	}

	switch statusCode {
	case http.StatusBadRequest: // 400 — abort, no retry
		op.ChannelKeyUpdate(ra.usedKey)
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		resp.Error(ra.c, statusCode, fwdErr.Error())
		return true, fmt.Errorf("channel %s bad request (400): %v", ra.channel.Name, fwdErr)

	case http.StatusUnauthorized, http.StatusForbidden: // 401/403 — accumulate auth errors, disable key within 5min window
		ra.usedKey.ConsecutiveAuthErrors++
		ra.usedKey.LastAuthErrorTime = time.Now().Unix()
		if ra.usedKey.ConsecutiveAuthErrors >= 3 {
			ra.usedKey.Enabled = false
			log.Warnf("key %d disabled after %d consecutive auth errors (channel: %s)",
				ra.usedKey.ID, ra.usedKey.ConsecutiveAuthErrors, ra.channel.Name)
		}
		op.ChannelKeyUpdate(ra.usedKey)
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.c.Writer.Written(), fmt.Errorf("channel %s auth error (%d): %v", ra.channel.Name, statusCode, fwdErr)

	case http.StatusTooManyRequests: // 429 — cool down this key
		if ra.retryAfter > 0 {
			ra.usedKey.RetryAfter = int64(ra.retryAfter.Seconds())
		}
		op.ChannelKeyUpdate(ra.usedKey)
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		return ra.c.Writer.Written(), fmt.Errorf("channel %s rate limited (429): %v", ra.channel.Name, fwdErr)

	case http.StatusNotFound: // 404 — switch channel
		op.ChannelKeyUpdate(ra.usedKey)
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.c.Writer.Written(), fmt.Errorf("channel %s not found (404): %v", ra.channel.Name, fwdErr)

	default: // 5xx/network error → circuit breaker
		op.ChannelKeyUpdate(ra.usedKey)
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.c.Writer.Written(), fmt.Errorf("channel %s failed: %v", ra.channel.Name, fwdErr)
	}
}

// parseRequest parses and validates the incoming request
func parseRequest(c *gin.Context, inboundType llm.APIFormat, inAdapter transformer.Inbound) (*llm.Request, error) {
	if inAdapter == nil {
		err := fmt.Errorf("unsupported inbound type: %s", inboundType)
		resp.Error(c, http.StatusBadRequest, err.Error())
		return nil, err
	}

	httpRequest, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, err
	}

	internalRequest, err := inAdapter.TransformRequest(c.Request.Context(), httpRequest)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if errors.Is(err, transformer.ErrInvalidRequest) {
			statusCode = http.StatusBadRequest
		}
		resp.Error(c, statusCode, err.Error())
		return nil, err
	}
	if internalRequest.RawRequest == nil {
		internalRequest.RawRequest = httpRequest
	}

	return internalRequest, nil
}

// forward forwards the request to the upstream service
func (ra *relayAttempt) forward() (int, error) {
	ctx := ra.c.Request.Context()
	if ra.internalRequest.RawRequest == nil {
		return 0, fmt.Errorf("missing raw request")
	}

	httpClient, err := helper.KeyHttpClient(ra.channel, &ra.usedKey)
	if err != nil {
		log.Warnf("failed to get HTTP client: %v", err)
		return 0, err
	}
	relayMiddleware := &relayPipelineMiddleware{attempt: ra}

	// compose middleware using plugin architecture
	proxyDesc := helper.ProxyDesc(ra.channel, &ra.usedKey)
	logFields := plugins.LogFields{
		ChannelName: ra.channel.Name,
		KeyID:       ra.usedKey.ID,
		ProxyDesc:   proxyDesc,
	}
	middlewares := []pipeline.Middleware{
		plugins.NewRateLimiter(ra.channel, ra.usedKey.ID, ra.internalRequest.Model),
		relayMiddleware,
		stream.EnsureUsage(),
		plugins.NewStatusHandler(func(code int, retryAfter time.Duration) {
			ra.statusCode = code
			ra.retryAfter = retryAfter
		}),
		plugins.NewLogger(logFields),
	}
	result, err := pipeline.NewFactory(httpclient.NewHttpClientWithClient(httpClient)).
		Pipeline(
			&parsedRequestInbound{Inbound: ra.inAdapter, request: ra.internalRequest},
			ra.outAdapter,
			pipeline.WithMiddlewares(middlewares...),
			pipeline.WithEmptyResponseDetection(),
		).
		Process(ctx, ra.internalRequest.RawRequest)
	if err != nil {
		plugins.LogUpstreamError(logFields, err)
		var upstreamErr *httpclient.Error
		if errors.As(err, &upstreamErr) {
			ra.statusCode = upstreamErr.StatusCode
			retryAfter, ok := httpclient.ParseRetryAfter(err)
			if ok {
				ra.retryAfter = retryAfter
			}
			return upstreamErr.StatusCode, err
		}
		if ra.statusCode > 0 {
			return ra.statusCode, err
		}
		return 0, err
	}
	if result == nil {
		return 0, fmt.Errorf("empty pipeline result")
	}
	if result.Stream {
		if err := ra.writeStream(ctx, result.EventStream); err != nil {
			return http.StatusOK, err
		}
		return http.StatusOK, nil
	}
	if result.Response == nil {
		return 0, fmt.Errorf("empty pipeline response")
	}
	ra.metrics.InternalResponse = result.Response.Body
	statusCode := result.Response.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	contentType := "application/json"
	if result.Response.Headers != nil {
		for key, values := range result.Response.Headers {
			for _, value := range values {
				ra.c.Header(key, value)
			}
		}
		if result.Response.Headers.Get("Content-Type") != "" {
			contentType = result.Response.Headers.Get("Content-Type")
		}
	}
	ra.c.Data(statusCode, contentType, result.Response.Body)
	return statusCode, nil
}

func (ra *relayAttempt) applyChannelRequestOptions(outboundRequest *httpclient.Request) {
	// ParamOverride only works on JSON request body; multipart image editing etc. cannot merge by map.
	if ra.channel.ParamOverride != nil && *ra.channel.ParamOverride != "" && strings.Contains(strings.ToLower(outboundRequest.Headers.Get("Content-Type")+" "+outboundRequest.ContentType), "application/json") {
		var bodyMap map[string]any
		if err := json.Unmarshal(outboundRequest.Body, &bodyMap); err != nil {
			log.Warnf("failed to unmarshal request body: %v, skipping param_override", err)
		} else {
			var override map[string]any
			if err := json.Unmarshal([]byte(*ra.channel.ParamOverride), &override); err != nil {
				log.Warnf("failed to unmarshal param_override: %v, skipping", err)
			} else {
				maps.Copy(bodyMap, override)
				modifiedBody, err := json.Marshal(bodyMap)
				if err != nil {
					log.Warnf("failed to marshal modified body: %v, skipping param_override", err)
				} else {
					outboundRequest.Body = modifiedBody
					ra.metrics.ParamOverride = *ra.channel.ParamOverride
				}
			}
		}
	}
	for _, header := range ra.channel.CustomHeader {
		// pipeline already wrote Auth before raw request middleware; keep auth config priority for same-named sensitive headers, matching old BuildHttpRequest override order.
		if outboundRequest.Headers.Get(header.HeaderKey) != "" && httpclient.IsSensitiveHeader(header.HeaderKey) {
			continue
		}
		outboundRequest.Headers.Set(header.HeaderKey, header.HeaderValue)
	}
}

// writeStream writes pipeline output (client-format stream) back to the requester, preserving first-token timeout switch-channel behavior.
func (ra *relayAttempt) writeStream(ctx context.Context, clientStream streams.Stream[*httpclient.StreamEvent]) error {
	if clientStream == nil {
		return fmt.Errorf("empty pipeline stream")
	}

	// set SSE response headers
	ra.c.Header("Content-Type", "text/event-stream")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")

	firstToken := true
	responseEvents := make([]*httpclient.StreamEvent, 0, 8)
	type sseReadResult struct {
		event *httpclient.StreamEvent
		err   error
	}
	results := make(chan sseReadResult, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(results)
		defer clientStream.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Warnf("stream reader panic: %v", r)
				select {
				case results <- sseReadResult{err: fmt.Errorf("stream reader panic: %v", r)}:
				case <-done:
				case <-ctx.Done():
				}
			}
		}()
		// Next may block waiting for upstream token; run in goroutine so first-token timeout and client disconnect can interrupt this channel attempt.
		for clientStream.Next() {
			select {
			case results <- sseReadResult{event: clientStream.Current()}:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
		if err := clientStream.Err(); err != nil {
			select {
			case results <- sseReadResult{err: err}:
			case <-done:
			case <-ctx.Done():
			}
		}
	}()

	firstTokenTimeoutSec := ra.group.FirstTokenTimeOut
	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if firstTokenTimeoutSec > 0 {
		firstTokenTimer = time.NewTimer(time.Duration(firstTokenTimeoutSec) * time.Second)
		firstTokenC = firstTokenTimer.C
		defer func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			log.Debugf("client disconnected, stopping stream")
			_ = clientStream.Close()
			return nil
		case <-firstTokenC:
			log.Warnf("first token timeout (%ds), switching channel", firstTokenTimeoutSec)
			_ = clientStream.Close()
			return fmt.Errorf("first token timeout (%ds)", firstTokenTimeoutSec)
		case r, ok := <-results:
			if !ok {
				log.Infof("stream end")
				if len(responseEvents) == 0 {
					return nil
				}
				// when client requests streaming, pipeline handles conversion on the fly and does not auto-generate a complete response body.
				// reuse the same inbound aggregator to assemble already-written events into final body; log only the final response once.
				responseBody, meta, err := ra.inAdapter.AggregateStreamChunks(context.WithoutCancel(ctx), responseEvents)
				if err != nil {
					log.Warnf("failed to aggregate stream response for log: %v", err)
					return nil
				}
				ra.metrics.InternalResponse = responseBody
				ra.metrics.RecordUsage(meta.Usage)
				return nil
			}
			if r.err != nil {
				log.Warnf("failed to read event: %v", r.err)
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}

			if r.event == nil || len(r.event.Data) == 0 {
				continue
			}
			// temporarily store pipeline-converted client-format events; aggregate into final response body for logging after normal completion; no per-chunk persistence.
			responseEvents = append(responseEvents, r.event)
			if firstToken {
				ra.metrics.FirstTokenTime = time.Now()
				firstToken = false
				if firstTokenTimer != nil {
					if !firstTokenTimer.Stop() {
						select {
						case <-firstTokenTimer.C:
						default:
						}
					}
					firstTokenTimer = nil
					firstTokenC = nil
				}
			}

			ra.c.SSEvent(r.event.Type, r.event.Data)
			ra.c.Writer.Flush()
		}
	}
}

// relayPipelineMiddleware handles octopus-specific channel-level side effects:
// 1. Apply channel param override and custom headers before pipeline sends the upstream request;
// 2. Record usage after non-streaming response is converted to llm.Response.
// axonhub/llm only provides partial functional middleware constructors; usage callback has no public constructor,
// so we keep a thin struct implementing the full interface.
type relayPipelineMiddleware struct {
	pipeline.DummyMiddleware
	attempt *relayAttempt
}

func (m *relayPipelineMiddleware) Name() string {
	return "octopus_relay"
}

func (m *relayPipelineMiddleware) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if request.Headers == nil {
		request.Headers = make(http.Header)
	}
	m.attempt.applyChannelRequestOptions(request)
	return request, nil
}

func (m *relayPipelineMiddleware) OnOutboundLlmResponse(ctx context.Context, response *llm.Response) (*llm.Response, error) {
	if response != nil {
		// non-streaming usage has been normalized to llm.Response by outbound transformer; streaming usage is recorded during final aggregation to avoid double counting.
		m.attempt.metrics.RecordUsage(response.Usage)
	}
	return response, nil
}

// parsedRequestInbound allows the pipeline to reuse the llm.Request that relay already parsed before channel selection.
// This way each candidate channel attempt only re-executes outbound transform and HTTP request, without re-reading or re-parsing the client body.
type parsedRequestInbound struct {
	transformer.Inbound
	request *llm.Request
}

func (in *parsedRequestInbound) TransformRequest(ctx context.Context, request *httpclient.Request) (*llm.Request, error) {
	if in.request == nil {
		return nil, fmt.Errorf("missing parsed request")
	}
	// relay has already parsed the request for channel selection; pipeline reuses the result at entry to avoid parsing the same body on each channel attempt.
	in.request.RawRequest = request
	return in.request, nil
}

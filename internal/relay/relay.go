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

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/samber/lo"
	"github.com/vrichv/octopus-pro/internal/helper"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/relay/balancer"
	"github.com/vrichv/octopus-pro/internal/relay/plugins"
	"github.com/vrichv/octopus-pro/internal/server/resp"
	"github.com/vrichv/octopus-pro/internal/utils/log"
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
		availableModels, _ := op.GroupListModel(c.Request.Context())
		if !modelAllowedByAPIKey(supportedModels, availableModels, internalRequest.Model) {
			log.Debugf("unsupported model: model=%s supported_models=%s available=%v", internalRequest.Model, supportedModels, availableModels)
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
		iter:             iter,
		group:            group,
		piiFilterEnabled: c.GetBool("pii_filter_enabled"),
	}, nil
}
func modelAllowedByAPIKey(supportedModels string, availableModels []string, requestedModel string) bool {
	if supportedModels == "" {
		return true
	}
	supportedModelsArray := lo.Map(strings.Split(supportedModels, ","), func(s string, _ int) string {
		return strings.TrimSpace(s)
	})
	effectiveModels := lo.Filter(supportedModelsArray, func(m string, _ int) bool {
		return lo.Contains(availableModels, m)
	})
	return len(effectiveModels) == 0 || lo.Contains(effectiveModels, requestedModel)
}

func (r *relayRun) run() {
	ctx := r.c.Request.Context()
	var lastErr error

	// round 0 = first pass, round 1 = retry after rate-limit wait
	for round := 0; round <= 1; round++ {
		if round > 0 {
			r.iter.Reset()
			lastErr = nil
		}

		allRateLimited := true
		var minWait time.Duration

		for r.iter.Next() {
			select {
			case <-ctx.Done():
				log.Debugf("request context canceled, stopping retry")
				r.metrics.Save(ctx, false, context.Canceled, r.iter.Attempts())
				return
			default:
			}

			// 对同一个渠道的多个 key 进行重试（429 时自动切换到下一个 key）
			// 前一个 key 已被标记冷却，GetChannelKey 会跳过它并返回其他可用 key
			for keyRetry := 0; keyRetry < 10; keyRetry++ {
				attempt, err := r.prepareAttempt()
				if err != nil {
					lastErr = err
					break
				}
				if attempt == nil {
					break
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

				if attempt.rateLimited {
					// 本地限流：记录等待时间，跳出 key 循环
					if minWait == 0 || attempt.rateLimitWait < minWait {
						minWait = attempt.rateLimitWait
					}
					break
				}
				// 非限流错误 → 标记有真实故障
				allRateLimited = false
				// 429 → 尝试渠道的下一个 key
				if attempt.statusCode != http.StatusTooManyRequests {
					break
				}
			}
			if !allRateLimited {
				break // 有真实故障，无需等待
			}
		}

		// 首次遍历：全部限流 + 等待时间合理 → 等待后重试
		if round == 0 && allRateLimited && minWait > 0 && minWait <= 2*time.Minute {
			log.Infof("all channels rate-limited, waiting %v for next slot", minWait)
			select {
			case <-time.After(minWait):
				log.Infof("rate-limit wait completed, retrying all channels")
				continue
			case <-ctx.Done():
				r.metrics.Save(ctx, false, context.Canceled, r.iter.Attempts())
				return
			}
		}
		break // 非限流故障或等待超时，退出循环
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
	span := ra.iter.StartAttempt(ra.channel.ID, ra.usedKey.ID, ra.channel.Name, helper.MaskKeySuffix(ra.usedKey.ChannelKey))

	upstreamStatusCode, fwdErr := ra.forward()
	nowSec := time.Now().Unix()
	statusCode := upstreamStatusCode
	if fwdErr == nil && statusCode == 0 {
		statusCode = http.StatusOK
	}
	if fwdErr != nil && statusCode == 0 {
		statusCode = ra.statusCode
	}
	if fwdErr != nil && statusCode == 0 {
		statusCode = http.StatusBadGateway
	}

	// success path
	if fwdErr == nil {
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{
			StatusCode:       statusCode,
			LastUseTimeStamp: nowSec,
			CostDelta:        ra.metrics.Stats.InputCost + ra.metrics.Stats.OutputCost,
			AuthResult:       op.ChannelKeyAuthSuccess,
		})

		span.End(dbmodel.AttemptSuccess, "")
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:       span.Duration().Milliseconds(),
			RequestSuccess: 1,
		})
		balancer.RecordSuccess(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		balancer.SetSticky(ra.metrics.APIKeyID, ra.metrics.RequestModel, ra.channel.ID, ra.usedKey.ID)
		return false, nil
	}

	// 本地限流：不触发熔断器、不计入失败统计
	if fwdErr != nil && errors.Is(fwdErr, plugins.ErrRateLimited) {
		ra.rateLimited = true
		var rlErr *plugins.RateLimitedError
		if errors.As(fwdErr, &rlErr) {
			ra.rateLimitWait = rlErr.Wait
		}
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		return false, fwdErr // written=false, 不进入 statusCode switch
	}

	// error path: handle by status code

	switch statusCode {
	case http.StatusBadRequest: // 400 — abort, no retry
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone})
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		resp.Error(ra.c, statusCode, fwdErr.Error())
		return true, fmt.Errorf("channel %s bad request (400): %v", ra.channel.Name, fwdErr)

	case http.StatusUnauthorized, http.StatusForbidden: // 401/403 — accumulate auth errors, disable key within 5min window
		latestKey := ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthFailure})
		if latestKey.ID != 0 && !latestKey.Enabled {
			log.Warnf("key %d disabled after %d consecutive auth errors (channel: %s)",
				latestKey.ID, latestKey.ConsecutiveAuthErrors, ra.channel.Name)
		}
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s auth error (%d): %v", ra.channel.Name, statusCode, fwdErr)

	case http.StatusTooManyRequests: // 429 — cool down this key
		update := op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone}
		if ra.retryAfter > 0 {
			update.RetryAfter = int64(ra.retryAfter.Seconds())
		}
		ra.applyKeyRuntimeUpdate(update)
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s rate limited (429): %v", ra.channel.Name, fwdErr)

	case http.StatusNotFound: // 404 — switch channel
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone})
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s not found (404): %v", ra.channel.Name, fwdErr)

	default: // 5xx/network error → circuit breaker
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone})
		span.End(dbmodel.AttemptFailed, fwdErr.Error())
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s failed: %v", ra.channel.Name, fwdErr)
	}
}

func (ra *relayAttempt) hasWrittenResponse() bool {
	return ra.responseWritten || ra.c.Writer.Written()
}

func (ra *relayAttempt) applyKeyRuntimeUpdate(update op.ChannelKeyRuntimeUpdate) dbmodel.ChannelKey {
	update.ChannelID = ra.channel.ID
	update.KeyID = ra.usedKey.ID
	key, err := op.ChannelKeyApplyRuntimeUpdate(update)
	if err != nil {
		log.Warnf("failed to update key runtime state (channel: %s, key: %d): %v", ra.channel.Name, ra.usedKey.ID, err)
		return dbmodel.ChannelKey{}
	}
	return key
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
		plugins.NewPrivacyFilter(ra.piiFilterEnabled),
		plugins.NewRateLimiter(ra.channel, ra.usedKey.ID, ra.internalRequest.Model),
		relayMiddleware,
		stream.EnsureUsage(),
		plugins.NewStatusHandler(func(code int, retryAfter time.Duration) {
			ra.statusCode = code
			ra.retryAfter = retryAfter
		}),
		plugins.NewLogger(logFields),
	}
	processCtx := ctx
	var cancel context.CancelFunc
	if ra.group.UpstreamTimeOut > 0 {
		processCtx, cancel = context.WithTimeout(ctx, time.Duration(ra.group.UpstreamTimeOut)*time.Second)
		defer cancel()
	}
	result, err := pipeline.NewFactory(httpclient.NewHttpClientWithClient(httpClient)).
		Pipeline(
			&parsedRequestInbound{Inbound: ra.inAdapter, request: ra.internalRequest},
			ra.outAdapter,
			pipeline.WithMiddlewares(middlewares...),
			pipeline.WithEmptyResponseDetection(),
		).
		Process(processCtx, ra.internalRequest.RawRequest)
	if err != nil {
		if processCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = fmt.Errorf("upstream timeout (%ds): %w", ra.group.UpstreamTimeOut, context.DeadlineExceeded)
		}
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
	statusCode := result.Response.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	if statusCode == http.StatusOK {
		if msg := successShapedError(result.Response.Body); msg != "" {
			return 0, fmt.Errorf("success-shaped upstream error: %s", msg)
		}
	}
	ra.metrics.InternalResponse = result.Response.Body
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
			var raw map[string]any
			if err := json.Unmarshal([]byte(*ra.channel.ParamOverride), &raw); err != nil {
				log.Warnf("failed to unmarshal param_override: %v, skipping", err)
			} else {
				override := resolveModelOverride(raw, bodyMap["model"])
				if override != nil {
					maps.Copy(bodyMap, override)
					modifiedBody, err := json.Marshal(bodyMap)
					if err != nil {
						log.Warnf("failed to marshal modified body: %v, skipping param_override", err)
					} else {
						outboundRequest.Body = modifiedBody
						if effectiveJSON, err := json.Marshal(override); err == nil {
							ra.metrics.ParamOverride = string(effectiveJSON)
						}
					}
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

// resolveModelOverride selects the correct override from param_override JSON.
// New format: top-level keys are model names, values are override objects.
// Old format: top-level keys are param names (non-object values) → applies to all models.
// Returns nil if the new format has no match for the given model.
func resolveModelOverride(raw map[string]any, model any) map[string]any {
	// Detect new format: all top-level values are objects (model→override mapping).
	isModelMap := len(raw) > 0
	for _, v := range raw {
		if _, ok := v.(map[string]any); !ok {
			isModelMap = false
			break
		}
	}
	if !isModelMap {
		return raw // old format, applies to all models
	}
	modelName, ok := model.(string)
	if !ok || modelName == "" {
		return nil
	}
	if ov, ok := raw[modelName].(map[string]any); ok {
		return ov
	}
	return nil
}

func successShapedError(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 0 || len(trimmed) > 512 {
		return ""
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
		if payload.Error.Message != "" {
			return payload.Error.Message
		}
		if payload.Error.Error.Message != "" {
			return payload.Error.Error.Message
		}
	}

	return ""
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

	streamHardTimeOutSec := ra.group.StreamHardTimeOut
	var streamHardTimer *time.Timer
	var streamHardC <-chan time.Time
	if streamHardTimeOutSec > 0 {
		streamHardTimer = time.NewTimer(time.Duration(streamHardTimeOutSec) * time.Second)
		streamHardC = streamHardTimer.C
		defer func() {
			if streamHardTimer != nil {
				streamHardTimer.Stop()
			}
		}()
	}

	streamIdleTimeOutSec := ra.group.StreamIdleTimeOut
	var streamIdleTimer *time.Timer
	var streamIdleC <-chan time.Time
	defer func() {
		if streamIdleTimer != nil {
			streamIdleTimer.Stop()
		}
	}()

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
		case <-streamHardC:
			log.Warnf("stream hard timeout (%ds), stopping stream", streamHardTimeOutSec)
			_ = clientStream.Close()
			return fmt.Errorf("stream hard timeout (%ds)", streamHardTimeOutSec)
		case <-streamIdleC:
			log.Warnf("stream idle timeout (%ds), stopping stream", streamIdleTimeOutSec)
			_ = clientStream.Close()
			return fmt.Errorf("stream idle timeout (%ds)", streamIdleTimeOutSec)
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
			if firstToken {
				if msg := successShapedError(r.event.Data); msg != "" {
					_ = clientStream.Close()
					return fmt.Errorf("success-shaped upstream stream error: %s", msg)
				}
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
				if streamIdleTimeOutSec > 0 {
					streamIdleTimer = time.NewTimer(time.Duration(streamIdleTimeOutSec) * time.Second)
					streamIdleC = streamIdleTimer.C
				}
			} else if streamIdleTimer != nil {
				if !streamIdleTimer.Stop() {
					select {
					case <-streamIdleTimer.C:
					default:
					}
				}
				streamIdleTimer.Reset(time.Duration(streamIdleTimeOutSec) * time.Second)
			}

			ra.c.SSEvent(r.event.Type, r.event.Data)
			ra.responseWritten = true
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

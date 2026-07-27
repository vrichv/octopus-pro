package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
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

	// Round-based retry: round 0 tries all channels/keys; if every error was
	// transient (no 400/401/403/404), wait for the shortest rate-limit window
	// and retry once to give the rate limiter a chance to open a slot.
	for round := 0; round <= 1; round++ {
		if round > 0 {
			r.iter.Reset()
			lastErr = nil
		}

		hadHardError := false
		var minWait time.Duration

		for r.iter.Next() {
		channelLoop:
			for {
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
					break channelLoop // channel-level error → next channel
				}
				if attempt == nil {
					break channelLoop // no more available keys in this channel
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

				// 本地限流：不阻塞等待，记录最小等待时间，立即试下一 key
				// 同渠道所有 key 共享同一个 model 级限流器，快速失败后切下一渠道
				if attempt.rateLimited {
					if minWait == 0 || attempt.rateLimitWait < minWait {
						minWait = attempt.rateLimitWait
					}
					// 冷却当前 key 使 prepareAttempt 跳过它，避免同一 key 无限循环
					dbmodel.RecordKeyModelTemporaryCooldown(attempt.channel.ID, attempt.usedKey.ID,
						attempt.internalRequest.Model, attempt.rateLimitWait)
					continue
				}

				// 非本地限流：包含上游 429、5xx、超时等可重试错误，
				// 以及 400/401/403/404 等不可重试错误。
				// 只有不可重试错误才标记 hadHardError。
				if attempt.tryNextKey {
					// 429 / 5xx / 超时 — 可重试，继续试同渠道下一 key
					// 捕获上游 Retry-After 用于 round 级等待
					if attempt.retryAfter > 0 && (minWait == 0 || attempt.retryAfter < minWait) {
						minWait = attempt.retryAfter
					}
					continue
				}

				// 400 / 401/403 / 404 / 已写回响应 — 不可重试
				hadHardError = true
				break channelLoop
			}
		}

		// 第一轮全部为可重试错误（含限流）且存在限流等待时间 → 等待后重试
		retryWaitMax := 2 * time.Minute // default
		if r.group.RateLimitRetryWaitMax != nil {
			if *r.group.RateLimitRetryWaitMax == 0 {
				// 0 = disabled
				break
			}
			retryWaitMax = time.Duration(*r.group.RateLimitRetryWaitMax) * time.Second
		}
		if round == 0 && !hadHardError && minWait > 0 && minWait <= retryWaitMax {
			log.Infof("all channels retryable (transient errors), waiting %v before retry (max %v)", minWait, retryWaitMax)
			select {
			case <-time.After(minWait):
				continue
			case <-ctx.Done():
				r.metrics.Save(ctx, false, context.Canceled, r.iter.Attempts())
				return
			}
		}
		break
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

	// === 上下文窗口预检查：若模型有 MaxContext 且输入估算超出，跳过整个 channel ===
	if maxCtx := lookupModelMaxContext(item.ModelName); maxCtx > 0 {
		estTotal := estimateTotalTokens(r.internalRequest)
		if estTotal > maxCtx {
			log.Debugf("context window skip: channel=%s model=%s est_total=%d max_context=%d",
				channel.Name, item.ModelName, estTotal, maxCtx)
			r.iter.Skip(channel.ID, 0, channel.Name,
				fmt.Sprintf("estimated tokens %d exceeds context window %d", estTotal, maxCtx))
			return nil, nil
		}
	}
	// === 检查结束 ===
	orderedKeys := channel.GetChannelKeys(item.ModelName)
	if len(orderedKeys) == 0 {
		r.iter.Skip(channel.ID, 0, channel.Name, "no available key")
		return nil, nil
	}
	for _, usedKey := range orderedKeys {
		if r.iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
			continue
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
		if r.iter.Index() == 0 {
			log.Debugf("forwarding to channel: model=%s mode=%d channel=%s upstream_model=%s key=%s (attempt %d/%d, sticky=%t)",
				r.metrics.RequestModel, r.group.Mode, channel.Name, item.ModelName,
				helper.MaskKeySuffix(usedKey.ChannelKey),
				r.iter.Index()+1, r.iter.Len(), r.iter.IsSticky())
		}

		return &relayAttempt{
			relayRun:   r,
			outAdapter: outAdapter,
			channel:    channel,
			usedKey:    usedKey,
		}, nil
	}

	r.iter.Skip(channel.ID, 0, channel.Name, "all keys circuit-broken")
	return nil, nil
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

		dbmodel.ClearKeyModelCooldown(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
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
		span.End(dbmodel.AttemptFailed, ra.failureMessage(fwdErr))
		return false, fwdErr // written=false, 不进入 statusCode switch
	}

	// error path: handle by status code

	switch statusCode {
	case http.StatusBadRequest: // 400 — abort, no retry
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone})
		span.End(dbmodel.AttemptFailed, ra.failureMessage(fwdErr))
		resp.Error(ra.c, statusCode, fwdErr.Error())
		return true, fmt.Errorf("channel %s bad request (400): %v", ra.channel.Name, fwdErr)

	case http.StatusUnauthorized, http.StatusForbidden: // 401/403 — auth error, switch channel
		latestKey := ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthFailure})
		if latestKey.ID != 0 && !latestKey.Enabled {
			log.Warnf("key %d disabled after %d consecutive auth errors (channel: %s)",
				latestKey.ID, latestKey.ConsecutiveAuthErrors, ra.channel.Name)
		}
		span.End(dbmodel.AttemptFailed, ra.failureMessage(fwdErr))
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s auth error (%d): %v", ra.channel.Name, statusCode, fwdErr)
		// tryNextKey=false: 认证失败不应重试同渠道其他 key

	case http.StatusTooManyRequests: // 429 — cool down this key, try next key
		update := op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone}
		if ra.retryAfter > 0 {
			update.RetryAfter = int64(ra.retryAfter.Seconds())
		}
		ra.applyKeyRuntimeUpdate(update)
		// per-key-per-model 冷却：仅冷却当前 (key, model) 组合，指数退避
		dbmodel.RecordKeyModelCooldown(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model, ra.retryAfter)
		span.End(dbmodel.AttemptFailed, ra.failureMessage(fwdErr))
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		ra.tryNextKey = true // 429 → 试同渠道下一个 key
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s rate limited (429): %v", ra.channel.Name, fwdErr)

	case http.StatusNotFound: // 404 — switch channel
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone})
		span.End(dbmodel.AttemptFailed, ra.failureMessage(fwdErr))
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		return ra.hasWrittenResponse(), fmt.Errorf("channel %s not found (404): %v", ra.channel.Name, fwdErr)
		// tryNextKey=false: 404 表示上游不支持此模型，切渠道

	default: // 5xx/timeout/stream truncation → circuit break this key, try next key when possible
		ra.applyKeyRuntimeUpdate(op.ChannelKeyRuntimeUpdate{StatusCode: statusCode, LastUseTimeStamp: nowSec, AuthResult: op.ChannelKeyAuthNone})
		ra.applyTemporaryKeyCooldown()
		span.End(dbmodel.AttemptFailed, ra.failureMessage(fwdErr))
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		balancer.RecordFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
		ra.tryNextKey = true // 未写回客户端时，试同渠道下一个 key
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

func maxDuration(a, b time.Duration) time.Duration {
	if a < b {
		return b
	}
	return a
}

func streamPenalty(timeoutSec int, min time.Duration, multiplier time.Duration) time.Duration {
	if timeoutSec <= 0 {
		return min
	}
	return maxDuration(time.Duration(timeoutSec)*time.Second*multiplier, min)
}

func (ra *relayAttempt) applyTemporaryKeyCooldown() {
	if ra.keyCooldown <= 0 {
		return
	}
	dbmodel.RecordKeyModelTemporaryCooldown(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model, ra.keyCooldown)
}

func (ra *relayAttempt) failureMessage(err error) string {
	msg := err.Error()
	if ra.keyCooldown > 0 {
		msg = fmt.Sprintf("%s; temporary key cooldown=%s", msg, ra.keyCooldown)
	}
	return msg
}

// lookupModelMaxContext 查询模型的 MaxContext，0 表示未知/不限制。
func lookupModelMaxContext(modelName string) int {
	price, err := op.LLMGet(modelName)
	if err != nil {
		return 0
	}
	return price.MaxContext
}

// estimateTotalTokens 从原始请求体粗略估算总 token 消耗（输入 + 输出预算）。
// 用 len(body)/3 做粗略估算（约 3 bytes/token），偏向高估以降低溢出风险。
// 输出预算取 client 的 max_tokens / max_completion_tokens，没有则默认 4096。
func estimateTotalTokens(req *llm.Request) int {
	bodySize := 0
	if req.RawRequest != nil {
		bodySize = len(req.RawRequest.Body)
	}
	if bodySize == 0 {
		return 0 // 无法估算，放行
	}
	inputEst := bodySize / 3

	outputBudget := 4096
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		outputBudget = int(*req.MaxTokens)
	} else if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		outputBudget = int(*req.MaxCompletionTokens)
	}
	return inputEst + outputBudget
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
	// Streaming timeout strategy:
	//   HTTP connection phase (pipeline.Process) — applies ResponseHeaderTimeout
	//     to bound the time waiting for upstream HTTP response headers.
	//     ResponseHeaderTimeout only fires before headers arrive; once the
	//     HTTP 200 + headers are received, the streaming body is unaffected.
	//   SSE reading phase (writeStream) — first-token timer, idle timer, hard timer.
	//   FirstTokenTimeOut must NOT be set on processCtx because that context is
	//   bound to the HTTP request via http.NewRequestWithContext. When the
	//   deadline fires, the transport cancels all subsequent response body reads,
	//   killing streams that are actively producing tokens even when the first
	//   token arrived well within the timeout.
	// See also: stream_timeout_test.go for writeStream timeout tests.
	processCtx := ctx
	var cancel context.CancelFunc
	isStreaming := ra.internalRequest.Stream != nil && *ra.internalRequest.Stream
	if ra.group.UpstreamTimeOut > 0 && !isStreaming {
		processCtx, cancel = context.WithTimeout(ctx, time.Duration(ra.group.UpstreamTimeOut)*time.Second)
		defer cancel()
	}
	// For streaming requests, set ResponseHeaderTimeout on the HTTP transport so
	// the connection phase cannot hang longer than FirstTokenTimeOut. This is
	// separate from writeStream's first-token timer which covers the SSE phase.
	// We clone the shared transport to avoid affecting other concurrent requests.
	if isStreaming && ra.group.FirstTokenTimeOut > 0 {
		timeout := time.Duration(ra.group.FirstTokenTimeOut) * time.Second
		if transport, ok := httpClient.Transport.(*http.Transport); ok && transport != nil {
			cloned := transport.Clone()
			cloned.ResponseHeaderTimeout = timeout
			httpClient = &http.Client{
				Transport: cloned,
				Timeout:   0,
			}
		}
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
			if isStreaming && ra.group.FirstTokenTimeOut > 0 {
				err = fmt.Errorf("first token timeout (%ds): %w", ra.group.FirstTokenTimeOut, context.DeadlineExceeded)
			} else {
				err = fmt.Errorf("upstream timeout (%ds): %w", ra.group.UpstreamTimeOut, context.DeadlineExceeded)
			}
		} else if isStreaming && ra.group.FirstTokenTimeOut > 0 && ctx.Err() == nil {
			// Detect transport-level timeout (ResponseHeaderTimeout fired before
			// the upstream sent any HTTP response headers).
			var urlErr *url.Error
			if errors.As(err, &urlErr) && urlErr.Timeout() {
				err = fmt.Errorf("first token timeout (%ds): %w", ra.group.FirstTokenTimeOut, context.DeadlineExceeded)
			}
		}
		logFields.URL = ra.upstreamURL
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

// finishReasonContentFilter is the finish_reason value for content-filtered responses.
const finishReasonContentFilter = "content_filter"

// hasContentFilterFinishReason checks if any event in the response stream contains
// a finish_reason of "content_filter". The upstream returns this when its content
// filter catches the output.
func hasContentFilterFinishReason(events []*httpclient.StreamEvent) bool {
	var event struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	for _, ev := range events {
		if ev == nil || len(ev.Data) == 0 {
			continue
		}
		if err := json.Unmarshal(ev.Data, &event); err != nil {
			continue
		}
		for _, choice := range event.Choices {
			if choice.FinishReason != nil && *choice.FinishReason == finishReasonContentFilter {
				return true
			}
		}
	}
	return false
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
	usageRecorded := false
	type sseReadResult struct {
		event *httpclient.StreamEvent
		err   error
	}
	results := make(chan sseReadResult, 1)
	done := make(chan struct{})
	// recordStreamUsage drains any remaining events from the results channel,
	// aggregates collected stream events, and records usage.
	// Called on every exit path (normal end, disconnect, timeout, error) so that
	// partial usage is never silently dropped.
	recordStreamUsage := func() {
		if usageRecorded {
			return
		}
		// Drain remaining events from the goroutine that may have been sent
		// before the main loop exited via ctx.Done() / timeout.
		for r := range results {
			if r.err == nil && r.event != nil && len(r.event.Data) > 0 {
				responseEvents = append(responseEvents, r.event)
			}
		}
		if len(responseEvents) == 0 {
			return
		}
		usageRecorded = true
		responseBody, meta, err := ra.inAdapter.AggregateStreamChunks(context.WithoutCancel(ctx), responseEvents)
		if err != nil {
			log.Warnf("failed to aggregate stream response for log: %v", err)
			return
		}
		ra.metrics.InternalResponse = responseBody
		ra.metrics.RecordUsage(meta.Usage)
	}
	defer recordStreamUsage()
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
			ra.statusCode = http.StatusGatewayTimeout
			ra.keyCooldown = streamPenalty(firstTokenTimeoutSec, 30*time.Second, 2)
			log.Warnf("first token timeout (%ds), switching channel", firstTokenTimeoutSec)
			_ = clientStream.Close()
			return fmt.Errorf("first token timeout (%ds)", firstTokenTimeoutSec)
		case <-streamHardC:
			ra.statusCode = http.StatusGatewayTimeout
			ra.keyCooldown = streamPenalty(streamHardTimeOutSec, 60*time.Second, 1)
			log.Warnf("stream hard timeout (%ds), stopping stream", streamHardTimeOutSec)
			_ = clientStream.Close()
			return fmt.Errorf("stream hard timeout (%ds)", streamHardTimeOutSec)
		case <-streamIdleC:
			ra.statusCode = http.StatusGatewayTimeout
			ra.keyCooldown = streamPenalty(streamIdleTimeOutSec, 60*time.Second, 2)
			log.Warnf("stream idle timeout (%ds), stopping stream", streamIdleTimeOutSec)
			_ = clientStream.Close()
			return fmt.Errorf("stream idle timeout (%ds)", streamIdleTimeOutSec)
		case r, ok := <-results:
			if !ok {
				log.Debugf("stream end")
				if len(responseEvents) == 0 {
					return nil
				}
				// Check for upstream content filter before aggregating usage.
				// The upstream may return a successful HTTP stream with finish_reason "content_filter",
				// which means the model output was filtered. This should be counted as a failure
				// so that monitoring and circuit-breakers respond appropriately.
				if hasContentFilterFinishReason(responseEvents) {
					log.Warnf("upstream content filter detected, marking as failure")
					ra.statusCode = http.StatusBadGateway
					ra.keyCooldown = maxDuration(30*time.Second, ra.keyCooldown)
					return fmt.Errorf("upstream content filter: finish_reason=%s", finishReasonContentFilter)
				}
				responseBody, meta, err := ra.inAdapter.AggregateStreamChunks(context.WithoutCancel(ctx), responseEvents)
				if err != nil {
					log.Warnf("failed to aggregate stream response for log: %v", err)
					return nil
				}
				ra.metrics.InternalResponse = responseBody
				ra.metrics.RecordUsage(meta.Usage)
				usageRecorded = true
				return nil
			}
			if r.err != nil {
				ra.statusCode = http.StatusBadGateway
				if firstToken {
					ra.keyCooldown = maxDuration(30*time.Second, ra.keyCooldown)
				} else {
					ra.keyCooldown = maxDuration(60*time.Second, ra.keyCooldown)
				}
				log.Warnf("failed to read event: %v", r.err)
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}
			if r.event == nil || len(r.event.Data) == 0 {
				continue
			}
			if firstToken {
				if msg := successShapedError(r.event.Data); msg != "" {
					ra.statusCode = http.StatusBadGateway
					ra.keyCooldown = maxDuration(30*time.Second, ra.keyCooldown)
					_ = clientStream.Close()
					return fmt.Errorf("success-shaped upstream stream error: %s", msg)
				}
			}

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

// OnInboundLlmRequest normalizes roles for upstream compatibility.
// The "developer" role (OpenAI o-series) is not supported by all upstream APIs
// (e.g., GLM via ModelScope). Convert it to "system" which is universally accepted.
func (m *relayPipelineMiddleware) OnInboundLlmRequest(ctx context.Context, request *llm.Request) (*llm.Request, error) {
	if request == nil {
		return nil, nil
	}
	for i := range request.Messages {
		if request.Messages[i].Role == "developer" {
			request.Messages[i].Role = "system"
		}
	}
	return request, nil
}

func (m *relayPipelineMiddleware) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if request.Headers == nil {
		request.Headers = make(http.Header)
	}
	m.attempt.upstreamURL = request.URL
	m.attempt.applyChannelRequestOptions(request)
	// Set default User-Agent if none of the transformers or custom headers set one.
	// This overrides httpclient's own "axonhub/1.0" default.
	if request.Headers.Get("User-Agent") == "" {
		request.Headers.Set("User-Agent", "curl/8.5.0")
	}
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

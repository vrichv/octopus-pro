package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// StatusCallback 接收上游 HTTP 状态码和可选的 Retry-After 时长。
type StatusCallback func(statusCode int, retryAfter time.Duration)

// NewStatusHandler 创建状态码捕获中间件。
// OnOutboundRawResponse 捕获成功响应的状态码。
// OnOutboundRawError 仅在 pipeline 前置 middleware 失败时触发，不覆盖 HTTP 请求错误（429/5xx 等）；
// HTTP 错误的状态码在 forward() 中通过 errors.As 直接提取。
func NewStatusHandler(cb StatusCallback) pipeline.Middleware {
	return &statusHandlerMiddleware{cb: cb}
}

type statusHandlerMiddleware struct {
	pipeline.DummyMiddleware
	cb StatusCallback
}

func (m *statusHandlerMiddleware) Name() string {
	return "status_handler"
}

func (m *statusHandlerMiddleware) OnOutboundRawResponse(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	if response != nil {
		m.cb(response.StatusCode, 0)
	}
	return response, nil
}

func (m *statusHandlerMiddleware) OnOutboundRawError(ctx context.Context, err error) {
	var upstreamErr *httpclient.Error
	if errors.As(err, &upstreamErr) {
		retryAfter, _ := httpclient.ParseRetryAfter(err)
		m.cb(upstreamErr.StatusCode, retryAfter)
	}
}

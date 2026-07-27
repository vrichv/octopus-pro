package plugins

import (
	"context"
	"errors"
	"net/http"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/vrichv/octopus-pro/internal/utils/log"
)

// LogFields holds the contextual fields needed for logging.
type LogFields struct {
	ChannelName string
	KeyID       int
	ProxyDesc   string
	URL         string // 上游请求完整 URL，由调用方传入
}

// NewLogger creates a structured logging plugin.
// OnOutboundRawResponse records all upstream response status codes.
// OnOutboundRawError in axonhub/llm pipeline only triggers for middleware pre-errors,
// not for HTTP request failures; HTTP error logging is done directly in forward() via LogUpstreamError.
func NewLogger(fields LogFields) pipeline.Middleware {
	return &loggerMiddleware{fields: fields}
}

type loggerMiddleware struct {
	pipeline.DummyMiddleware
	fields LogFields
}

func (m *loggerMiddleware) Name() string {
	return "logger"
}

func (m *loggerMiddleware) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	m.fields.URL = request.URL
	return request, nil
}

func (m *loggerMiddleware) OnOutboundRawResponse(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	if response == nil {
		return nil, nil
	}
	if response.StatusCode >= http.StatusBadRequest {
		if response.StatusCode == http.StatusTooManyRequests {
			log.Debugf("relay upstream error: channel=%s keyId=%d proxy=%s url=%s status=%d",
				m.fields.ChannelName, m.fields.KeyID, m.fields.ProxyDesc, m.fields.URL, response.StatusCode)
		} else {
			log.Warnf("relay upstream error: channel=%s keyId=%d proxy=%s url=%s status=%d",
				m.fields.ChannelName, m.fields.KeyID, m.fields.ProxyDesc, m.fields.URL, response.StatusCode)
		}
	} else {
		log.Debugf("relay upstream response: channel=%s keyId=%d proxy=%s url=%s status=%d",
			m.fields.ChannelName, m.fields.KeyID, m.fields.ProxyDesc, m.fields.URL, response.StatusCode)
	}
	return response, nil
}

// LogUpstreamError is called from forward() when an HTTP request fails, logging structured error information.
func LogUpstreamError(fields LogFields, err error) {
	var upstreamErr *httpclient.Error
	if errors.As(err, &upstreamErr) {
		bodyPreview := ""
		if len(upstreamErr.Body) > 0 {
			maxLen := 200
			if len(upstreamErr.Body) < maxLen {
				maxLen = len(upstreamErr.Body)
			}
			bodyPreview = string(upstreamErr.Body[:maxLen])
		}
		log.Warnf("relay upstream error: channel=%s keyId=%d proxy=%s url=%s status=%d body=%s",
			fields.ChannelName, fields.KeyID, fields.ProxyDesc,
			upstreamErr.URL, upstreamErr.StatusCode, bodyPreview)
		return
	}

	if errors.Is(err, ErrRateLimited) {
		log.Debugf("relay upstream error: channel=%s keyId=%d proxy=%s url=%s err=%v",
			fields.ChannelName, fields.KeyID, fields.ProxyDesc, fields.URL, err)
		return
	}
	log.Warnf("relay upstream error: channel=%s keyId=%d proxy=%s url=%s err=%v",
		fields.ChannelName, fields.KeyID, fields.ProxyDesc, fields.URL, err)
}

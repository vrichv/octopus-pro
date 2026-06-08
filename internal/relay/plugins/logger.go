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

func (m *loggerMiddleware) OnOutboundRawResponse(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	if response == nil {
		return nil, nil
	}
	if response.StatusCode >= http.StatusBadRequest {
		log.Warnf("relay upstream error: channel=%s keyId=%d proxy=%s status=%d",
			m.fields.ChannelName, m.fields.KeyID, m.fields.ProxyDesc, response.StatusCode)
	} else {
		log.Infof("relay upstream response: channel=%s keyId=%d proxy=%s status=%d",
			m.fields.ChannelName, m.fields.KeyID, m.fields.ProxyDesc, response.StatusCode)
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

	log.Warnf("relay upstream error: channel=%s keyId=%d proxy=%s err=%v",
		fields.ChannelName, fields.KeyID, fields.ProxyDesc, err)
}

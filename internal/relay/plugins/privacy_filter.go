package plugins

import (
	"context"
	"sync"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/vrichv/octopus-pro/internal/privacyfilter"
	"github.com/vrichv/octopus-pro/internal/utils/log"
)

var (
	privacyOnce    sync.Once
	privacyFilter  *privacyfilter.Filter
	privacyInitErr error
)

func getPrivacyFilter() (*privacyfilter.Filter, error) {
	privacyOnce.Do(func() {
		privacyFilter, privacyInitErr = privacyfilter.NewFromBytes(privacyfilter.GitleaksRules)
		if privacyInitErr != nil {
			log.Errorf("privacy filter init failed: %v, filter disabled", privacyInitErr)
		}
	})
	return privacyFilter, privacyInitErr
}

// NewPrivacyFilter creates a request-scoped privacy filter middleware.
// It is a no-op unless the input API key enables PII filtering.
func NewPrivacyFilter(enabled bool) pipeline.Middleware {
	return newPrivacyFilter(enabled, getPrivacyFilter)
}

func newPrivacyFilter(enabled bool, getFilter func() (*privacyfilter.Filter, error)) pipeline.Middleware {
	return pipeline.OnLlmRequest("privacy_filter", func(_ context.Context, request *llm.Request) (*llm.Request, error) {
		if request == nil || !enabled {
			return request, nil
		}
		filter, err := getFilter()
		if err != nil || filter == nil {
			return request, nil
		}
		for i := range request.Messages {
			redactMessageContent(&request.Messages[i], filter)
		}
		return request, nil
	})
}

func redactMessageContent(message *llm.Message, filter *privacyfilter.Filter) {
	if message == nil || filter == nil {
		return
	}
	redactString(message.Content.Content, filter)
	for i := range message.Content.MultipleContent {
		part := &message.Content.MultipleContent[i]
		if part.Type == "text" {
			redactString(part.Text, filter)
		}
	}
}

func redactString(text *string, filter *privacyfilter.Filter) {
	if text == nil || *text == "" {
		return
	}
	result := filter.Redact(*text)
	if result.Hit {
		*text = result.Redacted
	}
}

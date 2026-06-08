package plugins

import (
	"context"
	"sync"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
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

func privacyFilterEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyPrivacyFilterEnabled)
	return err == nil && enabled
}

// NewPrivacyFilter creates a request-scoped privacy filter middleware.
// It is a no-op unless SettingKeyPrivacyFilterEnabled is true.
func NewPrivacyFilter() pipeline.Middleware {
	return newPrivacyFilter(privacyFilterEnabled, getPrivacyFilter)
}

func newPrivacyFilter(enabled func() bool, getFilter func() (*privacyfilter.Filter, error)) pipeline.Middleware {
	return pipeline.OnLlmRequest("privacy_filter", func(_ context.Context, request *llm.Request) (*llm.Request, error) {
		if request == nil || !enabled() || len(request.Messages) == 0 {
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

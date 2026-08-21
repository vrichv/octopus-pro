package plugins

import (
	"context"
	"encoding/json"
	"fmt"
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
			log.Errorf("privacy filter init failed: %v", privacyInitErr)
		}
	})
	return privacyFilter, privacyInitErr
}

// EnsurePrivacyFilter validates the shared detector before relay accepts a
// request whose API key requires PII filtering.
func EnsurePrivacyFilter() error {
	_, err := getPrivacyFilter()
	return err
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
			return nil, fmt.Errorf("privacy filter unavailable: %w", err)
		}
		for i := range request.Messages {
			redactMessageContent(&request.Messages[i], filter)
		}
		for i := range request.Tools {
			redactTool(&request.Tools[i], filter)
		}
		redactRequestText(request, filter)
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
	for i := range message.ToolCalls {
		call := &message.ToolCalls[i]
		redactString(&call.Function.Arguments, filter)
		if call.ResponseCustomToolCall != nil {
			redactString(&call.ResponseCustomToolCall.Input, filter)
		}
	}
}

func redactRequestText(request *llm.Request, filter *privacyfilter.Filter) {
	if request == nil || filter == nil {
		return
	}
	redactString(request.User, filter)
	redactString(request.SafetyIdentifier, filter)
	redactString(request.PromptCacheKey, filter)
	if request.Embedding != nil {
		redactString(&request.Embedding.Input.String, filter)
		redactStrings(request.Embedding.Input.StringArray, filter)
	}
	if request.Rerank != nil {
		redactString(&request.Rerank.Query, filter)
		redactStrings(request.Rerank.Documents, filter)
	}
	if request.Image != nil {
		redactString(&request.Image.Prompt, filter)
	}
	if request.Completion != nil {
		redactString(&request.Completion.Prompt, filter)
		redactString(&request.Completion.Suffix, filter)
	}
	if request.Compact != nil {
		redactString(&request.Compact.Instructions, filter)
		for i := range request.Compact.Input {
			redactMessageContent(&request.Compact.Input[i], filter)
		}
	}
	if request.Speech != nil {
		redactString(&request.Speech.Input, filter)
		redactString(&request.Speech.Instructions, filter)
	}
	if request.Transcription != nil {
		redactString(&request.Transcription.Prompt, filter)
		redactStringMap(request.Transcription.Extra, filter)
	}
	if request.Translation != nil {
		redactString(&request.Translation.Prompt, filter)
		redactStringMap(request.Translation.Extra, filter)
	}
	if request.Moderation != nil {
		redactString(&request.Moderation.Input.String, filter)
		redactStrings(request.Moderation.Input.StringArray, filter)
		for i := range request.Moderation.Input.Parts {
			redactString(&request.Moderation.Input.Parts[i].Text, filter)
		}
	}
}

func redactStrings(texts []string, filter *privacyfilter.Filter) {
	for i := range texts {
		redactString(&texts[i], filter)
	}
}

func redactStringMap(values map[string][]string, filter *privacyfilter.Filter) {
	for key := range values {
		redactStrings(values[key], filter)
	}
}

func redactTool(tool *llm.Tool, filter *privacyfilter.Filter) {
	if tool == nil || filter == nil {
		return
	}
	redactString(&tool.Function.Description, filter)
	redactJSON(&tool.Function.Parameters, filter)
	redactJSON(&tool.Function.ParametersJsonSchema, filter)
	if tool.ResponseCustomTool != nil {
		redactString(&tool.ResponseCustomTool.Description, filter)
		if tool.ResponseCustomTool.Format != nil {
			redactString(&tool.ResponseCustomTool.Format.Definition, filter)
		}
	}
}

func redactJSON(raw *json.RawMessage, filter *privacyfilter.Filter) {
	if raw == nil || len(*raw) == 0 || filter == nil {
		return
	}
	var value any
	if err := json.Unmarshal(*raw, &value); err != nil {
		text := string(*raw)
		redactString(&text, filter)
		*raw = json.RawMessage(text)
		return
	}
	if redacted, err := json.Marshal(redactJSONValue(value, filter)); err == nil {
		*raw = redacted
	}
}

func redactJSONValue(value any, filter *privacyfilter.Filter) any {
	switch value := value.(type) {
	case string:
		if result := filter.Redact(value); result.Hit {
			return result.Redacted
		}
		return value
	case []any:
		for i, child := range value {
			value[i] = redactJSONValue(child, filter)
		}
		return value
	case map[string]any:
		for key, child := range value {
			value[key] = redactJSONValue(child, filter)
		}
		return value
	default:
		return value
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

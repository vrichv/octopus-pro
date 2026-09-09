package relay

import (
	"context"
	"testing"

	"github.com/looplj/axonhub/llm"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
)

// Provider channels must reach their own upstream endpoint per request type;
// silently falling back to the generic OpenAI transformer changes the URL and
// payload shape.
func TestOutboundRoutesProviderEndpoints(t *testing.T) {
	chat := &llm.Request{
		Model:       "test-model",
		RequestType: llm.RequestTypeChat,
		Messages:    []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hi")}}},
	}
	image := &llm.Request{
		Model:       "test-model",
		RequestType: llm.RequestTypeImage,
		Image:       &llm.ImageRequest{Prompt: "a cat"},
	}
	completion := &llm.Request{
		Model:       "test-model",
		RequestType: llm.RequestTypeCompletion,
		Completion:  &llm.CompletionRequest{Prompt: "def fib("},
	}
	compact := &llm.Request{
		Model:       "test-model",
		RequestType: llm.RequestTypeCompact,
		Compact:     &llm.CompactRequest{Input: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hi")}}}},
	}

	for _, tc := range []struct {
		name        string
		channelType llm.APIFormat
		request     *llm.Request
		baseURL     string
		wantURL     string
	}{
		{"openrouter image", dbmodel.ChannelTypeOpenRouter, image, "https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/images"},
		{"zai image", dbmodel.ChannelTypeZAI, image, "https://api.z.ai/api/paas/v4", "https://api.z.ai/api/paas/v4/images/generations"},
		{"modelscope chat", dbmodel.ChannelTypeModelScope, chat, "https://api-inference.modelscope.cn/v1", "https://api-inference.modelscope.cn/v1/chat/completions"},
		{"moonshot chat", dbmodel.ChannelTypeMoonshot, chat, "https://api.moonshot.cn/v1", "https://api.moonshot.cn/v1/chat/completions"},
		{"zai chat", dbmodel.ChannelTypeZAI, chat, "https://api.z.ai/api/paas/v4", "https://api.z.ai/api/paas/v4/chat/completions"},
		{"deepseek completion", dbmodel.ChannelTypeDeepSeek, completion, "https://api.deepseek.com", "https://api.deepseek.com/beta/completions"},
		{"openai completion", llm.APIFormatOpenAIChatCompletion, completion, "https://api.openai.com/v1", "https://api.openai.com/v1/completions"},
		{"openai responses compact", llm.APIFormatOpenAIResponse, compact, "https://api.openai.com/v1", "https://api.openai.com/v1/responses/compact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := newOutbound(tc.channelType, tc.request, tc.baseURL, "test-key")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := adapter.TransformRequest(context.Background(), tc.request)
			if err != nil {
				t.Fatal(err)
			}
			if raw.URL != tc.wantURL {
				t.Fatalf("URL = %q, want %q", raw.URL, tc.wantURL)
			}
		})
	}
}

// Chat-only providers must reject request types they cannot serve instead of
// being routed to a transformer that would post the wrong payload.
func TestOutboundRejectsUnsupportedRequestTypes(t *testing.T) {
	image := &llm.Request{Model: "test-model", RequestType: llm.RequestTypeImage, Image: &llm.ImageRequest{Prompt: "a cat"}}
	completion := &llm.Request{Model: "test-model", RequestType: llm.RequestTypeCompletion, Completion: &llm.CompletionRequest{Prompt: "def fib("}}

	for _, tc := range []struct {
		name        string
		channelType llm.APIFormat
		request     *llm.Request
	}{
		{"moonshot image", dbmodel.ChannelTypeMoonshot, image},
		{"modelscope image", dbmodel.ChannelTypeModelScope, image},
		{"moonshot completion", dbmodel.ChannelTypeMoonshot, completion},
		{"openrouter completion", dbmodel.ChannelTypeOpenRouter, completion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newOutbound(tc.channelType, tc.request, "https://example.com/v1", "test-key"); err == nil {
				t.Fatal("expected error for unsupported request type")
			}
		})
	}
}

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/vrichv/octopus-pro/internal/privacyfilter"
)

func TestPrivacyFilterMiddlewareRedactsStringContent(t *testing.T) {
	filter := newTestPrivacyFilter(t)
	content := "我的邮箱是 test@example.com，密码是 Hunter2xyz"
	request := &llm.Request{Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: &content}}}}

	middleware := newPrivacyFilter(true, func() (*privacyfilter.Filter, error) { return filter, nil })
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}

	redacted := *got.Messages[0].Content.Content
	if !strings.Contains(redacted, "[邮箱]") || !strings.Contains(redacted, "[密钥]") {
		t.Fatalf("content was not redacted: %q", redacted)
	}
}

func TestPrivacyFilterMiddlewareRedactsTextPartsOnly(t *testing.T) {
	filter := newTestPrivacyFilter(t)
	text := "联系我 test@example.com"
	imageURL := &llm.ImageURL{URL: "https://example.com/image.png"}
	request := &llm.Request{Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{MultipleContent: []llm.MessageContentPart{
		{Type: "text", Text: &text},
		{Type: "image_url", ImageURL: imageURL},
	}}}}}

	middleware := newPrivacyFilter(true, func() (*privacyfilter.Filter, error) { return filter, nil })
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}

	if redacted := *got.Messages[0].Content.MultipleContent[0].Text; !strings.Contains(redacted, "[邮箱]") {
		t.Fatalf("text part was not redacted: %q", redacted)
	}
	if got.Messages[0].Content.MultipleContent[1].ImageURL != imageURL {
		t.Fatal("non-text part was changed")
	}
}

func TestPrivacyFilterMiddlewareRedactsToolFields(t *testing.T) {
	filter := newTestPrivacyFilter(t)
	request := &llm.Request{
		Messages: []llm.Message{{ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Arguments: `{"email":"test@example.com"}`}}}}},
		Tools: []llm.Tool{{Function: llm.Function{
			Description: "send credentials to test@example.com",
			Parameters:  json.RawMessage(`{"examples":["test@example.com"]}`),
		}}},
	}
	middleware := newPrivacyFilter(true, func() (*privacyfilter.Filter, error) { return filter, nil })
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}
	if strings.Contains(got.Messages[0].ToolCalls[0].Function.Arguments, "test@example.com") {
		t.Fatalf("tool call arguments were not redacted: %s", got.Messages[0].ToolCalls[0].Function.Arguments)
	}
	if strings.Contains(got.Tools[0].Function.Description, "test@example.com") || strings.Contains(string(got.Tools[0].Function.Parameters), "test@example.com") {
		t.Fatalf("tool definition was not redacted: %+v", got.Tools[0].Function)
	}
}

func TestPrivacyFilterMiddlewareRedactsAllRouteTextFields(t *testing.T) {
	filter := newTestPrivacyFilter(t)
	request := &llm.Request{
		User: new("test@example.com"),
		Embedding: &llm.EmbeddingRequest{Input: llm.EmbeddingInput{
			String:      "test@example.com",
			StringArray: []string{"test@example.com"},
		}},
		Rerank:     &llm.RerankRequest{Query: "test@example.com", Documents: []string{"test@example.com"}},
		Image:      &llm.ImageRequest{Prompt: "test@example.com"},
		Completion: &llm.CompletionRequest{Prompt: "test@example.com", Suffix: "test@example.com"},
		Compact: &llm.CompactRequest{
			Instructions: "test@example.com",
			Input:        []llm.Message{{Content: llm.MessageContent{Content: new("test@example.com")}}},
		},
	}
	middleware := newPrivacyFilter(true, func() (*privacyfilter.Filter, error) { return filter, nil })
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}
	values := []string{
		*got.User,
		got.Embedding.Input.String,
		got.Embedding.Input.StringArray[0],
		got.Rerank.Query,
		got.Rerank.Documents[0],
		got.Image.Prompt,
		got.Completion.Prompt,
		got.Completion.Suffix,
		got.Compact.Instructions,
		*got.Compact.Input[0].Content.Content,
	}
	for _, value := range values {
		if strings.Contains(value, "test@example.com") {
			t.Fatalf("text field was not redacted: %q", value)
		}
	}
}

func TestPrivacyFilterMiddlewareDisabledIsNoop(t *testing.T) {
	content := "test@example.com"
	request := &llm.Request{Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: &content}}}}

	middleware := newPrivacyFilter(false, func() (*privacyfilter.Filter, error) {
		t.Fatal("filter should not be loaded when disabled")
		return nil, nil
	})
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}
	if *got.Messages[0].Content.Content != "test@example.com" {
		t.Fatalf("disabled middleware changed content: %q", *got.Messages[0].Content.Content)
	}
}

func TestPrivacyFilterMiddlewareInitErrorFailsClosed(t *testing.T) {
	content := "test@example.com"
	request := &llm.Request{Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: &content}}}}

	middleware := newPrivacyFilter(true, func() (*privacyfilter.Filter, error) {
		return nil, errors.New("boom")
	})
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "privacy filter unavailable") {
		t.Fatalf("expected privacy filter error, got request=%v err=%v", got, err)
	}
}

func TestPrivacyFilterMiddlewareEmptyMessagesIsNoop(t *testing.T) {
	filter := newTestPrivacyFilter(t)
	request := &llm.Request{}

	middleware := newPrivacyFilter(true, func() (*privacyfilter.Filter, error) { return filter, nil })
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}
	if got != request {
		t.Fatal("empty request should be returned unchanged")
	}
}

func newTestPrivacyFilter(t *testing.T) *privacyfilter.Filter {
	t.Helper()
	filter, err := privacyfilter.NewFromBytes(privacyfilter.GitleaksRules)
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	return filter
}

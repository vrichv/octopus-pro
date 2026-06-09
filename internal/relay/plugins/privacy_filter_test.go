package plugins

import (
	"context"
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

	middleware := newPrivacyFilter(func() bool { return true }, func() (*privacyfilter.Filter, error) { return filter, nil })
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

	middleware := newPrivacyFilter(func() bool { return true }, func() (*privacyfilter.Filter, error) { return filter, nil })
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

func TestPrivacyFilterMiddlewareDisabledIsNoop(t *testing.T) {
	content := "test@example.com"
	request := &llm.Request{Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: &content}}}}

	middleware := newPrivacyFilter(func() bool { return false }, func() (*privacyfilter.Filter, error) {
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

func TestPrivacyFilterMiddlewareInitErrorIsNoop(t *testing.T) {
	content := "test@example.com"
	request := &llm.Request{Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: &content}}}}

	middleware := newPrivacyFilter(func() bool { return true }, func() (*privacyfilter.Filter, error) {
		return nil, errors.New("boom")
	})
	got, err := middleware.OnInboundLlmRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnInboundLlmRequest: %v", err)
	}
	if *got.Messages[0].Content.Content != "test@example.com" {
		t.Fatalf("init-error middleware changed content: %q", *got.Messages[0].Content.Content)
	}
}

func TestPrivacyFilterMiddlewareEmptyMessagesIsNoop(t *testing.T) {
	filter := newTestPrivacyFilter(t)
	request := &llm.Request{}

	middleware := newPrivacyFilter(func() bool { return true }, func() (*privacyfilter.Filter, error) { return filter, nil })
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

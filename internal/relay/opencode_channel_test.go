package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
)

// Zen must preserve its own protocol matrix and session headers when proxied.
func TestOpenCodeZenRoutesModelsAndPreservesProxySession(t *testing.T) {
	type observed struct{ path, session, agent, client string }
	requests := make(chan observed, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observed{r.URL.Path, r.Header.Get("x-opencode-session"), r.UserAgent(), r.Header.Get("x-opencode-client")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	middleware := &relayPipelineMiddleware{attempt: &relayAttempt{
		channel: &dbmodel.Channel{Type: dbmodel.ChannelTypeOpenCodeZen, CustomHeader: []dbmodel.CustomHeader{
			{HeaderKey: "x-opencode-session", HeaderValue: "custom-session"},
			{HeaderKey: "x-opencode-client", HeaderValue: "custom-client"},
			{HeaderKey: "User-Agent", HeaderValue: "custom-agent"},
		}},
		usedKey: dbmodel.ChannelKey{ID: 91237, ChannelKey: "opencode-routing-test"},
	}}
	var session string
	for _, tc := range []struct{ model, path string }{
		{"muse-spark-1.3-contributor-free", "/v1/responses"},
		{"deepseek-v4-flash", "/v1/chat/completions"},
		{"gpt-5.6-luna", "/v1/responses"},
		{"minimax-m3", "/v1/chat/completions"},
		{"qwen3.7-max", "/v1/messages"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			request := &llm.Request{Model: tc.model, Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hello")}}}}
			adapter, err := newOutbound(dbmodel.ChannelTypeOpenCodeZen, request, upstream.URL, "test-key")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := adapter.TransformRequest(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = middleware.OnOutboundRawRequest(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			transport, err := httpclient.BuildHttpRequest(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			response, err := upstream.Client().Do(transport)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			got := <-requests
			if got.path != tc.path {
				t.Fatalf("path = %q, want %q", got.path, tc.path)
			}
			if got.session == "" || got.session == "custom-session" {
				t.Fatalf("invalid session %q", got.session)
			}
			if session == "" {
				session = got.session
			} else if got.session != session {
				t.Fatalf("session changed across protocol routes: %q != %q", got.session, session)
			}
			if got.agent != "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14" || got.client != "cli" {
				t.Fatalf("unexpected OpenCode headers: %+v", got)
			}
		})
	}
}

func TestOpenCodeZenPreservesOfficialBaseURL(t *testing.T) {
	request := &llm.Request{Model: "muse-spark-1.3-contributor-free", Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hello")}}}}
	adapter, err := newOutbound(dbmodel.ChannelTypeOpenCodeZen, request, "https://opencode.ai/zen/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := adapter.TransformRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if raw.URL != "https://opencode.ai/zen/v1/responses" {
		t.Fatalf("Zen URL = %q", raw.URL)
	}
}

func TestOpenCodeGoRoutesModelsIndependently(t *testing.T) {
	request := &llm.Request{Model: "minimax-m3", Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hello")}}}}
	adapter, err := newOutbound(dbmodel.ChannelTypeOpenCodeGo, request, "https://opencode.ai/zen/go/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := adapter.TransformRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if raw.URL != "https://opencode.ai/zen/go/v1/messages" {
		t.Fatalf("Go URL = %q", raw.URL)
	}
}

func TestOpenCodeZenMetadataProtocols(t *testing.T) {
	tests := []struct {
		name, model, provider string
		want                  openCodeProtocol
	}{
		{name: "mimo free defaults to chat", model: "mimo-v2.6-flash-free", want: openCodeProtocolChat},
		{name: "openai sdk uses responses", model: "new-model", provider: "@ai-sdk/openai", want: openCodeProtocolResponses},
		{name: "compatible sdk uses chat", model: "new-model", provider: "@ai-sdk/openai-compatible", want: openCodeProtocolChat},
		{name: "anthropic sdk uses messages", model: "new-model", provider: "@ai-sdk/anthropic", want: openCodeProtocolAnthropic},
		{name: "google sdk uses gemini", model: "new-model", provider: "@ai-sdk/google", want: openCodeProtocolGemini},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := openCodeModelProtocol(test.model, test.provider); got != test.want {
				t.Fatalf("protocol = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOpenCodeZenChatKeepsCurrentClientToolSchema(t *testing.T) {
	request := &llm.Request{
		APIFormat: llm.APIFormatOpenAIChatCompletion,
		Model:     "mimo-v2.6-flash-free",
		Messages:  []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hello")}}},
		Tools: []llm.Tool{{
			Type: "function",
			Function: llm.Function{
				Name:       "bash",
				Parameters: []byte(`{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","minimum":1,"maximum":600000},"workdir":{"type":"string"}},"required":["command"]}`),
			},
		}},
	}
	adapter, err := newOutbound(dbmodel.ChannelTypeOpenCodeZen, request, "https://opencode.ai/zen/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := adapter.TransformRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw.Body, &body); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(body.Tools))
	for _, tool := range body.Tools {
		names = append(names, tool.Function.Name)
	}
	// Client schema is kept; the free tier's mandatory `read` is added from the profile.
	if len(body.Tools) != 2 || body.Tools[0].Function.Name != "bash" || body.Tools[1].Function.Name != "read" {
		t.Fatalf("unexpected Mimo tools %v", names)
	}
	var schema map[string]any
	if err := json.Unmarshal(body.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing tool properties: %s", raw.Body)
	}
	timeout, ok := properties["timeout"].(map[string]any)
	if !ok || timeout["maximum"] != float64(600000) {
		t.Fatalf("timeout schema = %v, want maximum 600000", properties["timeout"])
	}
}

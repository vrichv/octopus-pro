package relay

import (
	"context"
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
			if got.agent != "omp/18.1.14" || got.client != "" {
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

package relay

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/looplj/axonhub/llm/httpclient"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
)

func TestOpenCodeSessionCacheRefreshesAfterOneHourIdle(t *testing.T) {
	cache := newOpenCodeSessionCache()
	key := dbmodel.ChannelKey{ID: 1, ChannelKey: "upstream-key"}
	firstUse := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

	original := cache.sessionID(key, firstUse)
	if _, err := uuid.Parse(original); err != nil {
		t.Fatalf("generated session ID is not a UUID: %q (%v)", original, err)
	}
	if got := cache.sessionID(key, firstUse.Add(59*time.Minute)); got != original {
		t.Fatalf("session before idle TTL = %q, want %q", got, original)
	}
	if got := cache.sessionID(key, firstUse.Add(59*time.Minute+openCodeSessionIdleTTL)); got != original {
		t.Fatalf("session after continued activity = %q, want %q", got, original)
	}

	refreshed := cache.sessionID(key, firstUse.Add(59*time.Minute+2*openCodeSessionIdleTTL+time.Nanosecond))
	if refreshed == original {
		t.Fatal("session was not refreshed after more than one hour idle")
	}
}

func TestOpenCodeSessionCacheRefreshesWhenKeyChanges(t *testing.T) {
	cache := newOpenCodeSessionCache()
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

	original := cache.sessionID(dbmodel.ChannelKey{ID: 7, ChannelKey: "old-key"}, now)
	refreshed := cache.sessionID(dbmodel.ChannelKey{ID: 7, ChannelKey: "new-key"}, now.Add(time.Minute))
	if refreshed == original {
		t.Fatal("session was reused after the configured upstream key changed")
	}
}

func TestOpenCodeSessionCacheSeparatesKeysAndIsConcurrent(t *testing.T) {
	cache := newOpenCodeSessionCache()
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	keyA := dbmodel.ChannelKey{ID: 11, ChannelKey: "same-secret"}
	keyB := dbmodel.ChannelKey{ID: 12, ChannelKey: "same-secret"}
	if sessionA, sessionB := cache.sessionID(keyA, now), cache.sessionID(keyB, now); sessionA == sessionB {
		t.Fatal("different channel keys received the same session ID")
	}

	const callers = 32
	ids := make(chan string, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			ids <- cache.sessionID(keyA, now)
		})
	}
	wg.Wait()
	close(ids)

	want := cache.sessionID(keyA, now)
	for got := range ids {
		if got != want {
			t.Fatalf("concurrent lookup = %q, want %q", got, want)
		}
	}
}

func TestOpenCodeMiddlewareOverridesHeadersAndAxonHubPreservesThem(t *testing.T) {
	middleware := &relayPipelineMiddleware{attempt: &relayAttempt{
		channel: &dbmodel.Channel{Type: dbmodel.ChannelTypeOpenCodeGo, CustomHeader: []dbmodel.CustomHeader{
			{HeaderKey: "x-opencode-session", HeaderValue: "channel-session"},
			{HeaderKey: "x-opencode-client", HeaderValue: "channel-client"},
			{HeaderKey: "User-Agent", HeaderValue: "channel-agent"},
		}},
		usedKey: dbmodel.ChannelKey{ID: 21, ChannelKey: "upstream-key"},
	}}
	request := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "https://proxy.example/v1/chat/completions",
		Headers: http.Header{
			"X-Opencode-Session": []string{"inbound-session"},
			"X-Opencode-Client":  []string{"inbound-client"},
			"User-Agent":         []string{"inbound-agent"},
		},
	}

	request, err := middleware.OnOutboundRawRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("OnOutboundRawRequest: %v", err)
	}
	if sessionID := request.Headers.Get("x-opencode-session"); sessionID == "" || sessionID == "channel-session" || sessionID == "inbound-session" {
		t.Fatalf("x-opencode-session = %q, want generated key session", sessionID)
	}
	if got := request.Headers.Get("x-opencode-client"); got != "" {
		t.Fatalf("x-opencode-client = %q, want removed", got)
	}
	if got := request.Headers.Get("User-Agent"); got != "omp/18.1.14" {
		t.Fatalf("User-Agent = %q, want omp/18.1.14", got)
	}

	finalRequest, err := httpclient.BuildHttpRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("BuildHttpRequest: %v", err)
	}
	for name, want := range map[string]string{
		"x-opencode-session": request.Headers.Get("x-opencode-session"),
		"User-Agent":         "omp/18.1.14",
	} {
		if got := finalRequest.Header.Get(name); got != want {
			t.Fatalf("AxonHub final %s = %q, want %q", name, got, want)
		}
	}
	if got := finalRequest.Header.Get("x-opencode-client"); got != "" {
		t.Fatalf("AxonHub final x-opencode-client = %q, want removed", got)
	}
}

func TestNonOpenCodeMiddlewareKeepsExistingBehavior(t *testing.T) {
	middleware := &relayPipelineMiddleware{attempt: &relayAttempt{
		channel: &dbmodel.Channel{},
		usedKey: dbmodel.ChannelKey{ID: 22, ChannelKey: "upstream-key"},
	}}
	request, err := middleware.OnOutboundRawRequest(context.Background(), &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "https://api.example.com/v1/chat/completions",
		Headers: make(http.Header),
	})
	if err != nil {
		t.Fatalf("OnOutboundRawRequest: %v", err)
	}
	if got := request.Headers.Get("x-opencode-session"); got != "" {
		t.Fatalf("x-opencode-session = %q, want empty", got)
	}
	if got := request.Headers.Get("x-opencode-client"); got != "" {
		t.Fatalf("x-opencode-client = %q, want empty", got)
	}
	if got := request.Headers.Get("User-Agent"); got != "codex_cli_rs/0.153.4 (Linux 6.18.0; x86_64) xterm-256color" {
		t.Fatalf("User-Agent = %q, want existing default", got)
	}
}

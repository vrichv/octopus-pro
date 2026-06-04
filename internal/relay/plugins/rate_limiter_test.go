package plugins

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestParseRateSpec(t *testing.T) {
	tests := []struct {
		input    string
		count    int
		interval time.Duration
		wantErr  bool
	}{
		{"2/1s", 2, time.Second, false},
		{"100/1m", 100, time.Minute, false},
		{"5/2h", 5, 2 * time.Hour, false},
		{"10/1d", 10, 24 * time.Hour, false},
		{"1/1m", 1, time.Minute, false},
		{"0/1m", 0, 0, true},
		{"abc/1m", 0, 0, true},
		{"100", 0, 0, true},
		{"/1m", 0, 0, true},
		{"100/", 0, 0, true},
		{"", 0, 0, true},
		{"100/1x", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			count, interval, err := ParseRateSpec(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseRateSpec(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseRateSpec(%q) unexpected error: %v", tt.input, err)
				return
			}
			if count != tt.count {
				t.Errorf("ParseRateSpec(%q) count = %d, want %d", tt.input, count, tt.count)
			}
			if interval != tt.interval {
				t.Errorf("ParseRateSpec(%q) interval = %v, want %v", tt.input, interval, tt.interval)
			}
		})
	}
}

func TestParseModelRateLimit(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]string
	}{
		{
			"gpt-4=2/1m,claude-3=10/1h",
			map[string]string{"gpt-4": "2/1m", "claude-3": "10/1h"},
		},
		{
			"gpt-4=2/1m",
			map[string]string{"gpt-4": "2/1m"},
		},
		{
			"gpt-4:free=2/1m,claude-3:free=10/1h",
			map[string]string{"gpt-4:free": "2/1m", "claude-3:free": "10/1h"},
		},
		{
			"my:model=5/1d",
			map[string]string{"my:model": "5/1d"},
		},
		{
			"",
			map[string]string{},
		},
		{
			"  gpt-4 = 2/1m , claude-3:free=10/1h  ",
			map[string]string{"gpt-4": "2/1m", "claude-3:free": "10/1h"},
		},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%.20s", tt.input), func(t *testing.T) {
			got := ParseModelRateLimit(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("ParseModelRateLimit(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("ParseModelRateLimit(%q)[%q] = %q, want %q", tt.input, k, got[k], v)
				}
			}
		})
	}
}

func TestRateLimiterAllow(t *testing.T) {
	rl := newRateLimiter(2, 100*time.Millisecond)
	if !rl.Allow() {
		t.Error("first request should be allowed")
	}
	if !rl.Allow() {
		t.Error("second request should be allowed")
	}
	if rl.Allow() {
		t.Error("third request should be blocked")
	}
	time.Sleep(150 * time.Millisecond)
	if !rl.Allow() {
		t.Error("request after window should be allowed")
	}
}

func TestRateLimiterReconfigure(t *testing.T) {
	rl := newRateLimiter(1, time.Minute)
	rl.reconfigure(3, 100*time.Millisecond)
	if !rl.Allow() {
		t.Error("first request should be allowed")
	}
	if !rl.Allow() {
		t.Error("second request should be allowed")
	}
	if !rl.Allow() {
		t.Error("third request should be allowed")
	}
	if rl.Allow() {
		t.Error("fourth request should be blocked")
	}
}

func TestNewRateLimiterMiddleware(t *testing.T) {
	ch := &model.Channel{
		ID:        1,
		RateLimit: "2/1s",
	}
	m := NewRateLimiter(ch, 42, "gpt-4")
	if m == nil {
		t.Fatal("NewRateLimiter returned nil")
	}
	// Should have key-level limiter active
	rlM, ok := m.(*rateLimiterMiddleware)
	if !ok {
		t.Fatal("unexpected type")
	}
	if rlM.limitKey == "" {
		t.Fatal("expected non-empty limitKey")
	}
}

func TestRateLimiterMiddlewareBlocks(t *testing.T) {
	ch := &model.Channel{
		ID:        1,
		RateLimit: "1/1s",
	}
	m := NewRateLimiter(ch, 42, "gpt-4").(*rateLimiterMiddleware)

	req := &httpclient.Request{}
	ctx := context.Background()

	// First request should pass
	_, err := m.OnOutboundRawRequest(ctx, req)
	if err != nil {
		t.Fatalf("first request should pass: %v", err)
	}

	// Second request should be blocked
	_, err = m.OnOutboundRawRequest(ctx, req)
	if err == nil {
		t.Fatal("second request should be blocked")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestModelRateLimitMiddleware(t *testing.T) {
	ch := &model.Channel{
		ID:             1,
		ModelRateLimit: "gpt-4=1/1s",
	}
	m := NewRateLimiter(ch, 42, "gpt-4").(*rateLimiterMiddleware)

	req := &httpclient.Request{}
	ctx := context.Background()

	// First request should pass
	_, err := m.OnOutboundRawRequest(ctx, req)
	if err != nil {
		t.Fatalf("first request should pass: %v", err)
	}

	// Second request should be blocked
	_, err = m.OnOutboundRawRequest(ctx, req)
	if err == nil {
		t.Fatal("second request should be blocked")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}

	// Different model should not be limited
	m2 := NewRateLimiter(ch, 42, "claude-3").(*rateLimiterMiddleware)
	_, err = m2.OnOutboundRawRequest(ctx, req)
	if err != nil {
		t.Errorf("different model should not be limited: %v", err)
	}
}

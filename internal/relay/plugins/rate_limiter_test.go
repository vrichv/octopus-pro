package plugins

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/vrichv/octopus-pro/internal/model"
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

func TestRateLimiterWaitDuration(t *testing.T) {
	rl := newRateLimiter(2, 100*time.Millisecond)
	rl.Allow()
	rl.Allow()

	// Window full, WaitDuration should return positive
	d := rl.WaitDuration()
	if d <= 0 {
		t.Errorf("expected positive wait duration when window full, got %v", d)
	}
	if d > 100*time.Millisecond {
		t.Errorf("wait duration %v exceeds interval 100ms", d)
	}

	// After window expires, WaitDuration should return 0
	time.Sleep(150 * time.Millisecond)
	d = rl.WaitDuration()
	if d != 0 {
		t.Errorf("expected zero wait duration after window expired, got %v", d)
	}
}

func TestRateLimiterTryAllow(t *testing.T) {
	rl := newRateLimiter(1, 500*time.Millisecond)

	// First request should be allowed (returns nil)
	if err := rl.TryAllow(); err != nil {
		t.Errorf("first TryAllow should return nil, got %v", err)
	}

	// Second request should be blocked (returns RateLimitedError)
	err := rl.TryAllow()
	if err == nil {
		t.Fatal("second TryAllow should return error")
	}

	// Should be a RateLimitedError with positive wait
	if err.Wait <= 0 {
		t.Errorf("expected positive wait, got %v", err.Wait)
	}
	if err.Wait > 500*time.Millisecond {
		t.Errorf("wait %v exceeds interval 500ms", err.Wait)
	}

	// errors.Is should match ErrRateLimited
	if !errors.Is(err, ErrRateLimited) {
		t.Error("RateLimitedError should match ErrRateLimited via errors.Is")
	}

	// errors.As should extract *RateLimitedError
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Error("errors.As should extract *RateLimitedError")
	}
	if rlErr.Wait != err.Wait {
		t.Errorf("extracted wait %v != original %v", rlErr.Wait, err.Wait)
	}
}

func TestRateLimitedErrorIs(t *testing.T) {
	err := &RateLimitedError{
		Key:      "test",
		Limit:    5,
		Interval: time.Minute,
		Wait:     30 * time.Second,
	}

	// errors.Is should match ErrRateLimited
	if !errors.Is(err, ErrRateLimited) {
		t.Error("RateLimitedError.Is should return true for ErrRateLimited")
	}

	// Should not match other errors
	if errors.Is(err, errors.New("something else")) {
		t.Error("RateLimitedError.Is should not match unrelated errors")
	}

	// Error() should contain useful info
	msg := err.Error()
	if msg == "" {
		t.Error("Error() should not be empty")
	}
}

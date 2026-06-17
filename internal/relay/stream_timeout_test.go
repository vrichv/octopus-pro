package relay

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/vrichv/octopus-pro/internal/model"
)

type fakeStream struct {
	events chan *httpclient.StreamEvent
	closed chan struct{}
	once   sync.Once

	current *httpclient.StreamEvent
	err     error
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		events: make(chan *httpclient.StreamEvent, 1),
		closed: make(chan struct{}),
	}
}

func (s *fakeStream) Next() bool {
	select {
	case event, ok := <-s.events:
		if !ok {
			return false
		}
		s.current = event
		return true
	case <-s.closed:
		return false
	}
}

func (s *fakeStream) Current() *httpclient.StreamEvent {
	return s.current
}

func (s *fakeStream) Err() error {
	return s.err
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type fakeInbound struct{}

func (fakeInbound) TransformRequest(context.Context, *httpclient.Request) (*llm.Request, error) {
	return nil, errors.New("not implemented")
}

func (fakeInbound) TransformResponse(context.Context, *llm.Response) (*httpclient.Response, error) {
	return nil, errors.New("not implemented")
}

func (fakeInbound) TransformStream(context.Context, streams.Stream[*llm.Response]) (streams.Stream[*httpclient.StreamEvent], error) {
	return nil, errors.New("not implemented")
}

func (fakeInbound) TransformError(context.Context, error) *httpclient.Error {
	return nil
}

func (fakeInbound) AggregateStreamChunks(context.Context, []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return nil, llm.ResponseMeta{}, nil
}

// trackingInbound wraps fakeInbound and tracks AggregateStreamChunks calls,
// returning configurable usage data for testing the defer-based usage recording.
type trackingInbound struct {
	fakeInbound
	aggCallCount int
	aggChunks    []*httpclient.StreamEvent // captured from last call
	usage        *llm.Usage                // usage to return from aggregation
}

func (in *trackingInbound) AggregateStreamChunks(ctx context.Context, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	in.aggCallCount++
	in.aggChunks = chunks
	meta := llm.ResponseMeta{}
	if in.usage != nil {
		meta.Usage = in.usage
	}
	return []byte("{}"), meta, nil
}

func newTestRelayAttemptWithInbound(group model.Group, inAdapter transformer.Inbound) (*relayAttempt, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	return &relayAttempt{
		relayRun: &relayRun{
			c:         context,
			inAdapter: inAdapter,
			metrics:   &RelayMetrics{},
			group:     group,
		},
	}, recorder
}

func TestWriteStream_ClientDisconnectRecordsPartialUsage(t *testing.T) {
	in := &trackingInbound{
		usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 50},
	}
	ra, _ := newTestRelayAttemptWithInbound(model.Group{}, in)
	stream := newFakeStream()

	ctx, cancel := context.WithCancel(context.Background())

	// Send one event; the goroutine will pick it up and send to results.
	stream.events <- &httpclient.StreamEvent{Type: "message", Data: []byte("{\"choices\":[]}")}
	// Close events channel to signal stream end; goroutine will close results.
	close(stream.events)

	// Give writeStream a chance to process the event and reach the stream-end path
	// before we cancel — the event is already in results, so the main loop
	// will see !ok (stream end) first if results is read before ctx.Done().
	// We cancel AFTER a short window so the stream-end path wins the select.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	err := ra.writeStream(ctx, stream)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// At least one of the exit paths should have called AggregateStreamChunks:
	// - If stream-end path won: inline call + defer skipped via usageRecorded
	// - If ctx.Done() won: defer call
	// Either way, RecordUsage should have been called.
	if in.aggCallCount != 1 {
		t.Fatalf("expected AggregateStreamChunks to be called exactly once, got %d calls", in.aggCallCount)
	}
	if ra.metrics.Stats.InputToken != 100 {
		t.Fatalf("expected InputToken=100, got %d", ra.metrics.Stats.InputToken)
	}
	if ra.metrics.Stats.OutputToken != 50 {
		t.Fatalf("expected OutputToken=50, got %d", ra.metrics.Stats.OutputToken)
	}
}

func TestWriteStream_StreamEndNoDuplicateUsage(t *testing.T) {
	in := &trackingInbound{
		usage: &llm.Usage{PromptTokens: 500, CompletionTokens: 100},
	}
	ra, _ := newTestRelayAttemptWithInbound(model.Group{}, in)
	stream := newFakeStream()

	// Send one event and close the stream to simulate normal stream end
	stream.events <- &httpclient.StreamEvent{Type: "message", Data: []byte("{\"choices\":[]}")}
	close(stream.events)

	err := ra.writeStream(ra.c.Request.Context(), stream)
	if err != nil {
		t.Fatalf("expected nil error on normal stream end, got %v", err)
	}

	// Verify AggregateStreamChunks was called exactly once (not twice)
	if in.aggCallCount != 1 {
		t.Fatalf("expected AggregateStreamChunks to be called exactly once (no duplicate), got %d calls", in.aggCallCount)
	}

	// Verify RecordUsage captured the tokens
	if ra.metrics.Stats.InputToken != 500 {
		t.Fatalf("expected InputToken=500, got %d", ra.metrics.Stats.InputToken)
	}
	if ra.metrics.Stats.OutputToken != 100 {
		t.Fatalf("expected OutputToken=100, got %d", ra.metrics.Stats.OutputToken)
	}
}

func TestWriteStream_EmptyStreamSkipsUsage(t *testing.T) {
	in := &trackingInbound{
		usage: &llm.Usage{PromptTokens: 999, CompletionTokens: 999},
	}
	ra, _ := newTestRelayAttemptWithInbound(model.Group{}, in)
	stream := newFakeStream()

	// Close the stream immediately — no events
	close(stream.events)

	err := ra.writeStream(ra.c.Request.Context(), stream)
	if err != nil {
		t.Fatalf("expected nil error on empty stream, got %v", err)
	}

	// Verify AggregateStreamChunks was NOT called (empty stream)
	if in.aggCallCount != 0 {
		t.Fatalf("expected AggregateStreamChunks NOT to be called for empty stream, got %d calls", in.aggCallCount)
	}

	// Verify no tokens recorded
	if ra.metrics.Stats.InputToken != 0 {
		t.Fatalf("expected InputToken=0 for empty stream, got %d", ra.metrics.Stats.InputToken)
	}
	if ra.metrics.Stats.OutputToken != 0 {
		t.Fatalf("expected OutputToken=0 for empty stream, got %d", ra.metrics.Stats.OutputToken)
	}
}

func newTestRelayAttempt(group model.Group) (*relayAttempt, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	return &relayAttempt{
		relayRun: &relayRun{
			c:         context,
			inAdapter: fakeInbound{},
			metrics:   &RelayMetrics{},
			group:     group,
		},
	}, recorder
}

func TestWriteStream_FirstTokenTimeoutBeforeWriteAllowsRetry(t *testing.T) {
	ra, _ := newTestRelayAttempt(model.Group{FirstTokenTimeOut: 1})
	stream := newFakeStream()

	err := ra.writeStream(ra.c.Request.Context(), stream)
	if err == nil || !strings.Contains(err.Error(), "first token timeout") {
		t.Fatalf("expected first token timeout error, got %v", err)
	}
	if ra.responseWritten {
		t.Fatal("expected responseWritten to stay false before first event")
	}
}

func TestWriteStream_StreamIdleTimeoutAfterFirstEventMarksWritten(t *testing.T) {
	ra, recorder := newTestRelayAttempt(model.Group{StreamIdleTimeOut: 1})
	stream := newFakeStream()
	stream.events <- &httpclient.StreamEvent{Type: "message", Data: []byte("{\"choices\":[]}")}

	err := ra.writeStream(ra.c.Request.Context(), stream)
	if err == nil || !strings.Contains(err.Error(), "stream idle timeout") {
		t.Fatalf("expected stream idle timeout error, got %v", err)
	}
	if !ra.responseWritten {
		t.Fatal("expected responseWritten after first event")
	}
	if body := recorder.Body.String(); !strings.Contains(body, "{\"choices\":[]}") {
		t.Fatalf("expected recorder body to contain SSE data, got %q", body)
	}
}

func TestWriteStream_StreamHardTimeoutBeforeFirstEventAllowsRetry(t *testing.T) {
	ra, _ := newTestRelayAttempt(model.Group{StreamHardTimeOut: 1})
	stream := newFakeStream()

	err := ra.writeStream(ra.c.Request.Context(), stream)
	if err == nil || !strings.Contains(err.Error(), "stream hard timeout") {
		t.Fatalf("expected stream hard timeout error, got %v", err)
	}
	if ra.responseWritten {
		t.Fatal("expected responseWritten to stay false before first event")
	}
}

func TestSuccessShapedError(t *testing.T) {
	longBody := strings.Repeat("x", 513) + "model is currently unavailable"
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "json error message",
			body: []byte(`{"error":{"message":"model is currently unavailable"}}`),
			want: "model is currently unavailable",
		},
		{
			name: "wrapped json error message",
			body: []byte(`{"error":{"error":{"message":"model is currently unavailable"}}}`),
			want: "model is currently unavailable",
		},
		{
			name: "plain signature ignored",
			body: []byte("model is currently unavailable"),
			want: "",
		},
		{
			name: "long body ignored",
			body: []byte(longBody),
			want: "",
		},
		{
			name: "normal text ignored",
			body: []byte("normal model text"),
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := successShapedError(test.body); got != test.want {
				t.Fatalf("successShapedError() = %q, want %q", got, test.want)
			}
		})
	}
}

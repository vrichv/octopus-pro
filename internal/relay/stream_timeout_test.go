package relay

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
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

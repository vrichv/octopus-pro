package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm/httpclient"
)

type nextPanickingStream struct{}

func (*nextPanickingStream) Next() bool                       { panic("nil upstream stream") }
func (*nextPanickingStream) Current() *httpclient.StreamEvent { return nil }
func (*nextPanickingStream) Err() error                       { return nil }
func (*nextPanickingStream) Close() error                     { return nil }

func TestGuardedStreamConvertsNextPanicIntoError(t *testing.T) {
	stream := newGuardedStream(&nextPanickingStream{})
	if stream.Next() {
		t.Fatal("Next returned true after an upstream panic")
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "upstream stream Next panic") {
		t.Fatalf("Err() = %v, want converted Next panic", err)
	}
}

func TestWriteStreamReturnsErrorForPanickingUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	attempt := &relayAttempt{relayRun: &relayRun{c: c}}

	err := attempt.writeStream(context.Background(), &nextPanickingStream{})
	if err == nil || !strings.Contains(err.Error(), "upstream stream Next panic") {
		t.Fatalf("writeStream() error = %v, want converted upstream panic", err)
	}
}

func TestWriteStreamRejectsTypedNilUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	attempt := &relayAttempt{relayRun: &relayRun{c: c}}
	var stream *nextPanickingStream

	err := attempt.writeStream(context.Background(), stream)
	if err == nil || !strings.Contains(err.Error(), "empty pipeline stream") {
		t.Fatalf("writeStream() error = %v, want typed-nil rejection", err)
	}
}

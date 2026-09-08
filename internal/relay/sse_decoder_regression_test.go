package relay

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/looplj/axonhub/llm/httpclient"
)

// OpenAI Responses can emit a terminal event before the transport reaches EOF.
// The decoder is then advanced again while response wrappers flush their queued
// chunks; EOF must remain idempotent rather than dereferencing a cleared scanner.
func TestSSEDecoderEOFIsIdempotent(t *testing.T) {
	decoder := httpclient.NewDefaultSSEDecoder(
		context.Background(),
		io.NopCloser(strings.NewReader("data: trailing-event\n")),
	)

	if !decoder.Next() {
		t.Fatalf("first Next() = false, err = %v", decoder.Err())
	}
	if got := string(decoder.Current().Data); got != "trailing-event" {
		t.Fatalf("event data = %q", got)
	}

	for range 3 {
		if decoder.Next() {
			t.Fatal("Next() = true after EOF")
		}
		if err := decoder.Err(); err != nil {
			t.Fatalf("Err() after EOF = %v", err)
		}
	}
}

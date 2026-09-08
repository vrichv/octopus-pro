package relay

import (
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// guardedStream converts panics from an upstream stream implementation into
// ordinary stream errors. Typed-nil streams are rejected before this wrapper
// is constructed; the guard remains for malformed or unstable upstream streams.
type guardedStream struct {
	stream streams.Stream[*httpclient.StreamEvent]

	mu  sync.Mutex
	err error

	closeOnce sync.Once
	closeErr  error
}

func newGuardedStream(stream streams.Stream[*httpclient.StreamEvent]) *guardedStream {
	return &guardedStream{stream: stream}
}

// isNilStream detects both a nil interface and an interface containing a nil
// pointer. Calling methods through the latter panics only after a request has
// reached the stream reader.
func isNilStream(stream streams.Stream[*httpclient.StreamEvent]) bool {
	if stream == nil {
		return true
	}
	value := reflect.ValueOf(stream)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (s *guardedStream) Next() (next bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.setPanicError("Next", recovered)
			next = false
		}
	}()
	return s.stream.Next()
}

func (s *guardedStream) Current() (event *httpclient.StreamEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.setPanicError("Current", recovered)
			event = nil
		}
	}()
	return s.stream.Current()
}

func (s *guardedStream) Err() (err error) {
	if err = s.savedErr(); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = s.setPanicError("Err", recovered)
		}
	}()
	return s.stream.Err()
}

func (s *guardedStream) Close() error {
	s.closeOnce.Do(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.closeErr = s.setPanicError("Close", recovered)
			}
		}()
		s.closeErr = s.stream.Close()
	})
	return s.closeErr
}

func (s *guardedStream) savedErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *guardedStream) setPanicError(operation string, recovered any) error {
	err := &streamPanicError{
		operation: operation,
		recovered: recovered,
		stack:     debug.Stack(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
	return s.err
}

type streamPanicError struct {
	operation string
	recovered any
	stack     []byte
}

func (e *streamPanicError) Error() string {
	return fmt.Sprintf("upstream stream %s panic: %v", e.operation, e.recovered)
}

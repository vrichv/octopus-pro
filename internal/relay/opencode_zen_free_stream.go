package relay

import (
	"fmt"
	"slices"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
)

type zenCallIndex struct{ choice, tool int }

type zenPendingCall struct {
	call      llm.ToolCall
	arguments strings.Builder
}

// The provider transformer first normalizes SSE into llm.Response chunks. Only
// calls needing schema conversion are buffered here. Their complete arguments
// are emitted before the original finish event, then the existing inbound
// transformer renders the client's protocol, including Responses done events.
type zenToolStream struct {
	source      streams.Stream[*llm.Response]
	mappings    map[string]*zenToolMapping
	pending     map[zenCallIndex]*zenPendingCall
	passthrough map[zenCallIndex]bool
	queue       []*llm.Response
	current     *llm.Response
	err         error
}

func (s *zenToolStream) Next() bool {
	if s.err != nil {
		return false
	}
	for len(s.queue) == 0 {
		if !s.source.Next() {
			s.err = s.source.Err()
			if s.err == nil && len(s.pending) > 0 {
				s.err = fmt.Errorf("OpenCode Zen Free: stream ended with unfinished tool arguments")
			}
			return false
		}
		response := s.source.Current()
		if response == nil {
			continue
		}
		if err := s.convert(response); err != nil {
			s.err = err
			return false
		}
	}
	s.current = s.queue[0]
	s.queue = s.queue[1:]
	return true
}

func (s *zenToolStream) convert(response *llm.Response) error {
	if response == llm.DoneResponse {
		if len(s.pending) > 0 {
			return fmt.Errorf("OpenCode Zen Free: tool call ended without a finish event")
		}
		s.queue = append(s.queue, response)
		return nil
	}
	result := *response
	result.Choices = slices.Clone(response.Choices)
	if s.passthrough == nil {
		s.passthrough = make(map[zenCallIndex]bool)
	}
	for i := range result.Choices {
		choice := &result.Choices[i]
		if choice.Delta != nil {
			delta := *choice.Delta
			delta.ToolCalls = nil
			for _, call := range choice.Delta.ToolCalls {
				key := zenCallIndex{choice.Index, call.Index}
				mapping := s.mappings[call.Function.Name]
				if s.pending[key] == nil && (call.Type == "custom" || (call.Function.Name != "" && (mapping == nil || !mapping.needsConversion()))) {
					s.passthrough[key] = true
				}
				if s.passthrough[key] {
					delta.ToolCalls = append(delta.ToolCalls, call)
					continue
				}
				pending := s.pending[key]
				if pending == nil {
					pending = &zenPendingCall{call: call}
					pending.call.Function.Arguments = ""
					s.pending[key] = pending
				}
				if call.ID != "" {
					pending.call.ID = call.ID
				}
				if call.Type != "" {
					pending.call.Type = call.Type
				}
				if call.Function.Name != "" {
					pending.call.Function.Name = call.Function.Name
				}
				if call.Function.Namespace != "" {
					pending.call.Function.Namespace = call.Function.Namespace
				}
				pending.arguments.WriteString(call.Function.Arguments)
			}
			choice.Delta = &delta
		}
		if choice.FinishReason != nil {
			calls, err := s.flush(choice.Index)
			if err != nil {
				return err
			}
			if len(calls) > 0 {
				converted := *response
				converted.Usage = nil
				converted.TransformerMetadata = nil
				converted.Choices = []llm.Choice{{Index: choice.Index, Delta: &llm.Message{Role: "assistant", ToolCalls: calls}}}
				s.queue = append(s.queue, &converted)
			}
		}
	}
	s.queue = append(s.queue, &result)
	return nil
}

func (s *zenToolStream) flush(choice int) ([]llm.ToolCall, error) {
	var indices []int
	for key := range s.pending {
		if key.choice == choice {
			indices = append(indices, key.tool)
		}
	}
	slices.Sort(indices)
	var calls []llm.ToolCall
	for _, index := range indices {
		key := zenCallIndex{choice, index}
		pending := s.pending[key]
		call := pending.call
		call.Function.Arguments = pending.arguments.String()
		if mapping := s.mappings[call.Function.Name]; mapping != nil {
			arguments, err := mapping.arguments(call.Function.Arguments, false)
			if err != nil {
				return nil, err
			}
			call.Function.Name = mapping.clientName
			if mapping.clientNamespace != "" {
				call.Function.Namespace = mapping.clientNamespace
				call.Function.Name = strings.TrimPrefix(mapping.clientName, mapping.clientNamespace+"__")
				call.Function.Name = strings.TrimPrefix(call.Function.Name, mapping.clientNamespace+".")
			}
			call.Function.Arguments = arguments
		}
		if call.Function.Name == "" || call.ID == "" {
			return nil, fmt.Errorf("OpenCode Zen Free: incomplete tool call identity")
		}
		calls = append(calls, call)
		delete(s.pending, key)
	}
	return calls, nil
}

func (s *zenToolStream) Current() *llm.Response { return s.current }
func (s *zenToolStream) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.source.Err()
}
func (s *zenToolStream) Close() error { return s.source.Close() }

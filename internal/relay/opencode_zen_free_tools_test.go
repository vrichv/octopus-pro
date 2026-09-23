package relay

import (
	"testing"

	"github.com/looplj/axonhub/llm"
)

func TestZenToolMappingsIgnoresIncompatibleSameNameSchema(t *testing.T) {
	profile := []llm.Tool{{
		Type: "function",
		Function: llm.Function{
			Name:       "bash",
			Parameters: []byte(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		},
	}}
	client := []llm.Tool{{
		Type: "function",
		Function: llm.Function{
			Name:       "bash",
			Parameters: []byte(`{"type":"object","properties":{"command":{"type":"string"},"shell":{"type":"string"}},"required":["command","shell"]}`),
		},
	}}

	mappings, extras, err := zenToolMappings(profile, client)
	if err != nil {
		t.Fatalf("schema drift must not reject the request: %v", err)
	}
	if len(mappings) != 0 {
		t.Fatalf("incompatible same-name tool should use the fixed profile, got mappings: %v", mappings)
	}
	if len(extras) != 0 {
		t.Fatalf("profile tool should not be duplicated in extras: %v", extras)
	}
}

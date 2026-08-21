package relay

import (
	"testing"
)

func TestResolveModelOverride_OldFormat(t *testing.T) {
	raw := map[string]any{
		"temperature": 0.7,
		"top_p":       0.9,
	}
	// Old format returns raw itself regardless of model.
	override := resolveModelOverride(raw, "deepseek-v4-pro")
	if override == nil {
		t.Fatal("expected override for old format, got nil")
	}
	if override["temperature"] != 0.7 {
		t.Errorf("expected temperature=0.7, got %v", override["temperature"])
	}

	// Also works when model is empty/nil.
	override = resolveModelOverride(raw, "")
	if override == nil {
		t.Fatal("expected override for old format with empty model, got nil")
	}
}

func TestResolveModelOverride_NewFormat_Match(t *testing.T) {
	deepseekOverride := map[string]any{
		"thinking": map[string]any{"type": "disabled"},
	}
	raw := map[string]any{
		"deepseek-v4-pro": deepseekOverride,
		"deepseek-v4-flash": map[string]any{
			"thinking": map[string]any{"type": "disabled"},
		},
	}
	override := resolveModelOverride(raw, "deepseek-v4-pro")
	if override == nil {
		t.Fatal("expected override for matched model, got nil")
	}
	thinking, ok := override["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking to be map, got %T", override["thinking"])
	}
	if thinking["type"] != "disabled" {
		t.Errorf("expected thinking.type=disabled, got %v", thinking["type"])
	}
}

func TestResolveModelOverride_NewFormat_NoMatch(t *testing.T) {
	raw := map[string]any{
		"deepseek-v4-pro": map[string]any{
			"thinking": map[string]any{"type": "disabled"},
		},
	}
	override := resolveModelOverride(raw, "gpt-4o")
	if override != nil {
		t.Fatalf("expected nil for unmatched model, got %v", override)
	}
}

func TestResolveModelOverride_EmptyMap(t *testing.T) {
	override := resolveModelOverride(map[string]any{}, "model")
	// Empty map: isModelMap is false (len==0), so old format path returns raw.
	if override == nil {
		t.Fatal("expected non-nil for empty map (old format path)")
	}
}

func TestResolveModelOverride_NilModel(t *testing.T) {
	// Old format: returns raw override even with nil model (applies universally).
	raw := map[string]any{
		"temperature": 0.7,
	}
	override := resolveModelOverride(raw, nil)
	if override == nil {
		t.Fatal("expected non-nil for old format with nil model")
	}
}

func TestModelAllowedByAPIKey(t *testing.T) {
	tests := []struct {
		name            string
		supportedModels string
		availableModels []string
		requestedModel  string
		want            bool
	}{
		{
			name:            "empty supported models allows all",
			supportedModels: "",
			availableModels: []string{"test"},
			requestedModel:  "gpt-5.5",
			want:            true,
		},
		{
			name:            "empty intersection rejects request",
			supportedModels: "test,glm-5",
			availableModels: []string{"deepseek-v4-flash"},
			requestedModel:  "gpt-5.5",
			want:            false,
		},
		{
			name:            "effective list allows matching request",
			supportedModels: "test,glm-5",
			availableModels: []string{"test", "glm-5"},
			requestedModel:  "glm-5",
			want:            true,
		},
		{
			name:            "effective list rejects non matching request",
			supportedModels: "test,glm-5",
			availableModels: []string{"test", "glm-5"},
			requestedModel:  "gpt-5.5",
			want:            false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := modelAllowedByAPIKey(test.supportedModels, test.availableModels, test.requestedModel)
			if got != test.want {
				t.Fatalf("modelAllowedByAPIKey() = %v, want %v", got, test.want)
			}
		})
	}
}

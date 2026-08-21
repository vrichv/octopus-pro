package price

import (
	"testing"

	"github.com/vrichv/octopus-pro/internal/model"
)

func TestLookupCalibratedPriceExactAndPrefixed(t *testing.T) {
	llmPriceLock.Lock()
	original := llmPrice
	llmPrice = map[string]model.LLMPrice{
		"gpt-4o":      {Input: 2.5, Output: 10},
		"gpt-4o-mini": {Input: 0.15, Output: 0.6},
		"claude-4.5":  {Input: 3, Output: 15},
	}
	llmPriceLock.Unlock()
	t.Cleanup(func() {
		llmPriceLock.Lock()
		llmPrice = original
		llmPriceLock.Unlock()
	})

	exact := LookupCalibratedPrice("GPT-4o")
	if exact == nil || exact.Input != 2.5 {
		t.Fatalf("exact match = %+v, want gpt-4o", exact)
	}

	prefixed := LookupCalibratedPrice("openrouter/gpt-4o-mini")
	if prefixed == nil || prefixed.Input != 0.15 {
		t.Fatalf("prefixed match = %+v, want gpt-4o-mini", prefixed)
	}

	longest := LookupCalibratedPrice("provider/gpt-4o-mini-2024")
	if longest == nil || longest.Input != 0.15 {
		t.Fatalf("longest match = %+v, want gpt-4o-mini over gpt-4o", longest)
	}

	if got := LookupCalibratedPrice("unknown-model"); got != nil {
		t.Fatalf("unknown model = %+v, want nil", got)
	}
}

func TestLookupCalibratedPriceAmbiguous(t *testing.T) {
	llmPriceLock.Lock()
	original := llmPrice
	llmPrice = map[string]model.LLMPrice{
		"gpt-4": {Input: 1},
		"o4":    {Input: 2},
	}
	llmPriceLock.Unlock()
	t.Cleanup(func() {
		llmPriceLock.Lock()
		llmPrice = original
		llmPriceLock.Unlock()
	})

	// "custom-gpt-4" should uniquely match gpt-4.
	got := LookupCalibratedPrice("custom-gpt-4")
	if got == nil || got.Input != 1 {
		t.Fatalf("custom-gpt-4 = %+v, want gpt-4", got)
	}
}

func TestClaudeAliasesMatchGeneratedNames(t *testing.T) {
	aliases := claudeAliases("claude-sonnet-4-5-20251101")
	want := map[string]bool{
		"claude-sonnet-4.5-20251101": true,
		"claude-4.5-sonnet-20251101": true,
		"claude-4-5-sonnet-20251101": true,
	}
	if len(aliases) != len(want) {
		t.Fatalf("aliases = %v, want %d aliases", aliases, len(want))
	}
	for _, alias := range aliases {
		if !want[alias] {
			t.Fatalf("unexpected alias %q", alias)
		}
	}
}

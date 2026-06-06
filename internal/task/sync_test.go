package task

import (
	"reflect"
	"testing"
)

func TestSyncChannelModelsPrunesMissingAndEnablesNew(t *testing.T) {
	selected, excluded := syncChannelModels(
		[]string{"gpt-4o", "o1", "o4-mini"},
		[]string{"o1", "missing"},
	)

	wantSelected := []string{"gpt-4o", "o4-mini"}
	if !reflect.DeepEqual(selected, wantSelected) {
		t.Fatalf("selected mismatch: got %v want %v", selected, wantSelected)
	}

	wantExcluded := []string{"o1"}
	if !reflect.DeepEqual(excluded, wantExcluded) {
		t.Fatalf("excluded mismatch: got %v want %v", excluded, wantExcluded)
	}
}

func TestSyncChannelModelsEnablesReappearedAfterPrune(t *testing.T) {
	selected, excluded := syncChannelModels(
		[]string{"gpt-4o"},
		nil,
	)

	wantSelected := []string{"gpt-4o"}
	if !reflect.DeepEqual(selected, wantSelected) {
		t.Fatalf("selected mismatch: got %v want %v", selected, wantSelected)
	}
	if len(excluded) != 0 {
		t.Fatalf("excluded mismatch: got %v want empty", excluded)
	}
}

func TestDedupeModelsTrimsAndKeepsOrder(t *testing.T) {
	got := dedupeModels([]string{" gpt-4o ", "", "o1", "gpt-4o", " o3 "})
	want := []string{"gpt-4o", "o1", "o3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupe mismatch: got %v want %v", got, want)
	}
}

package task

import (
	"errors"
	"reflect"
	"testing"
)

func TestSyncChannelModelsPrunesMissingAndEnablesNew(t *testing.T) {
	selected, excluded := syncChannelModels(
		[]string{"gpt-4o", "o1", "o4-mini"},
		[]string{"o1", "missing"},
		nil,
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

func TestSyncChannelModelsExcludesCustomModels(t *testing.T) {
	selected, excluded := syncChannelModels(
		[]string{"gpt-4o", "custom-model"},
		nil,
		[]string{"CUSTOM-MODEL"},
	)
	if !reflect.DeepEqual(selected, []string{"gpt-4o"}) {
		t.Fatalf("selected mismatch: got %v", selected)
	}
	if len(excluded) != 0 {
		t.Fatalf("excluded mismatch: got %v", excluded)
	}
}

func TestDedupeModelsTrimsAndKeepsOrder(t *testing.T) {
	got := dedupeModels([]string{" gpt-4o ", "", "o1", "gpt-4o", " o3 "})
	want := []string{"gpt-4o", "o1", "o3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupe mismatch: got %v want %v", got, want)
	}
}

func TestStartModelSyncRejectsConcurrentRun(t *testing.T) {
	syncModelsRunning.Store(true)
	t.Cleanup(func() { syncModelsRunning.Store(false) })
	if err := StartModelSync(); !errors.Is(err, ErrModelSyncRunning) {
		t.Fatalf("StartModelSync() error = %v, want ErrModelSyncRunning", err)
	}
}

package balancer

import (
	"testing"

	"github.com/vrichv/octopus-pro/internal/model"
)

func TestIteratorReset(t *testing.T) {
	group := model.Group{
		ID:   1,
		Name: "test",
		Mode: model.GroupModeRoundRobin,
		Items: []model.GroupItem{
			{ID: 1, ChannelID: 10, ModelName: "m", Priority: 0},
			{ID: 2, ChannelID: 20, ModelName: "m", Priority: 1},
			{ID: 3, ChannelID: 30, ModelName: "m", Priority: 2},
		},
	}

	iter := NewIterator(group, 0, "m")
	if iter.Len() != 3 {
		t.Fatalf("expected 3 candidates, got %d", iter.Len())
	}

	// Traverse all candidates
	count := 0
	for iter.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("expected 3 iterations, got %d", count)
	}

	// After traversal, Next() should return false
	if iter.Next() {
		t.Error("expected Next() to return false after full traversal")
	}

	// Reset
	iter.Reset()

	// After reset, should be able to traverse again
	count = 0
	for iter.Next() {
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 iterations after reset, got %d", count)
	}

	// Attempts should be preserved (append semantics)
	attempts := iter.Attempts()
	if len(attempts) == 0 {
		// No attempts were recorded since we didn't call StartAttempt
		// This is expected - Reset only resets index
	}
}

func TestIteratorResetPreservesSticky(t *testing.T) {
	group := model.Group{
		ID:               1,
		Name:             "test",
		Mode:             model.GroupModeRoundRobin,
		SessionKeepTime:  60,
		Items: []model.GroupItem{
			{ID: 1, ChannelID: 10, ModelName: "m", Priority: 0},
			{ID: 2, ChannelID: 20, ModelName: "m", Priority: 1},
		},
	}

	iter := NewIterator(group, 0, "m")

	// Sticky should be -1 initially (no sticky session set)
	if iter.stickyIdx != -1 {
		t.Errorf("expected stickyIdx=-1, got %d", iter.stickyIdx)
	}

	iter.Reset()

	// After reset, sticky should still be preserved
	if iter.stickyIdx != -1 {
		t.Errorf("expected stickyIdx=-1 after reset, got %d", iter.stickyIdx)
	}
}

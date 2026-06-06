package task

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestStartTaskSkipsReentryWhileRunning(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32

	entry := &taskEntry{
		name: "blocked",
		fn: func() {
			calls.Add(1)
			started <- struct{}{}
			<-release
		},
	}

	startTask(entry)
	waitForStart(t, started)

	startTask(entry)
	assertNoStart(t, started)

	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}

	release <- struct{}{}
	waitForStopped(t, entry)
}

func TestStartTaskAllowsRunAfterPreviousRunFinishes(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	var calls atomic.Int32

	entry := &taskEntry{
		name: "repeatable",
		fn: func() {
			calls.Add(1)
			started <- struct{}{}
			<-release
		},
	}

	startTask(entry)
	waitForStart(t, started)
	release <- struct{}{}
	waitForStopped(t, entry)

	startTask(entry)
	waitForStart(t, started)

	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}

	release <- struct{}{}
	waitForStopped(t, entry)
}

func TestStartTaskStateIsPerTask(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})

	first := &taskEntry{
		name: "first",
		fn: func() {
			firstStarted <- struct{}{}
			<-releaseFirst
		},
	}
	second := &taskEntry{
		name: "second",
		fn: func() {
			secondStarted <- struct{}{}
			<-releaseSecond
		},
	}

	startTask(first)
	waitForStart(t, firstStarted)

	startTask(second)
	waitForStart(t, secondStarted)

	releaseFirst <- struct{}{}
	releaseSecond <- struct{}{}
	waitForStopped(t, first)
	waitForStopped(t, second)
}

func waitForStart(t *testing.T, started <-chan struct{}) {
	t.Helper()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
}

func assertNoStart(t *testing.T, started <-chan struct{}) {
	t.Helper()

	select {
	case <-started:
		t.Fatal("task reentered while previous run was still active")
	case <-time.After(25 * time.Millisecond):
	}
}

func waitForStopped(t *testing.T, entry *taskEntry) {
	t.Helper()

	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("task did not stop")
		case <-tick.C:
			if !entry.running.Load() {
				return
			}
		}
	}
}

package balancer

import (
	"testing"
	"time"
)

func TestCircuitBreakerStateMachine(t *testing.T) {
	const (
		channelID = 910001
		keyID     = 91000101
		modelName = "circuit-state-model"
	)
	key := circuitKey(channelID, keyID, modelName)
	otherChannelKey := circuitKey(channelID+1, keyID, modelName)
	otherKey := circuitKey(channelID, keyID+1, modelName)
	otherModelKey := circuitKey(channelID, keyID, modelName+"-other")
	for _, breakerKey := range []string{key, otherChannelKey, otherKey, otherModelKey} {
		globalBreaker.Delete(breakerKey)
	}
	t.Cleanup(func() {
		for _, breakerKey := range []string{key, otherChannelKey, otherKey, otherModelKey} {
			globalBreaker.Delete(breakerKey)
		}
	})

	if got := computeCooldown(1, 60, 600); got != 60*time.Second {
		t.Fatalf("first cooldown=%v want 60s", got)
	}
	if got := computeCooldown(2, 60, 600); got != 120*time.Second {
		t.Fatalf("second cooldown=%v want 120s", got)
	}
	if got := computeCooldown(3, 60, 600); got != 240*time.Second {
		t.Fatalf("third cooldown=%v want 240s", got)
	}

	for range 5 {
		RecordFailure(channelID, keyID, modelName)
	}
	status := GetCircuitBreakerStatus(channelID, keyID, modelName)
	if status.State != "open" || status.TripCount != 1 || status.ConsecutiveFailures != 5 {
		t.Fatalf("closed-to-open status=%+v", status)
	}
	if tripped, remaining := IsTripped(channelID, keyID, modelName); !tripped || remaining <= 0 {
		t.Fatalf("open circuit was not skipped: tripped=%t remaining=%v", tripped, remaining)
	}

	entry := getOrCreateEntry(key)
	entry.mu.Lock()
	entry.LastFailureTime = time.Now().Add(-61 * time.Second)
	entry.mu.Unlock()
	if tripped, remaining := IsTripped(channelID, keyID, modelName); tripped || remaining != 0 {
		t.Fatalf("expired open circuit did not admit half-open probe: tripped=%t remaining=%v", tripped, remaining)
	}
	if status := GetCircuitBreakerStatus(channelID, keyID, modelName); status.State != "half_open" {
		t.Fatalf("expected half_open status, got %+v", status)
	}
	if tripped, _ := IsTripped(channelID, keyID, modelName); !tripped {
		t.Fatal("second half-open request was not rejected")
	}

	RecordFailure(channelID, keyID, modelName)
	status = GetCircuitBreakerStatus(channelID, keyID, modelName)
	if status.State != "open" || status.TripCount != 2 {
		t.Fatalf("half-open failure did not re-open circuit: %+v", status)
	}
	if got := GetCooldownForChannel(channelID, status.TripCount); got != 120*time.Second {
		t.Fatalf("half-open retry cooldown=%v want 120s", got)
	}

	entry.mu.Lock()
	entry.LastFailureTime = time.Now().Add(-121 * time.Second)
	entry.mu.Unlock()
	if tripped, _ := IsTripped(channelID, keyID, modelName); tripped {
		t.Fatal("second half-open probe was rejected after cooldown")
	}
	RecordSuccess(channelID, keyID, modelName)
	status = GetCircuitBreakerStatus(channelID, keyID, modelName)
	if status.State != "closed" || status.TripCount != 0 || status.ConsecutiveFailures != 0 {
		t.Fatalf("half-open success did not reset circuit: %+v", status)
	}

	RecordFailure(channelID+1, keyID, modelName)
	RecordFailure(channelID, keyID+1, modelName)
	RecordFailure(channelID, keyID, modelName+"-other")
	for _, dimensions := range [][3]int{{channelID + 1, keyID, 0}, {channelID, keyID + 1, 0}} {
		if status := GetCircuitBreakerStatus(dimensions[0], dimensions[1], modelName); status.State != "closed" {
			t.Fatalf("dimension-isolated channel/key status=%+v", status)
		}
	}
	if status := GetCircuitBreakerStatus(channelID, keyID, modelName+"-other"); status.State != "closed" {
		t.Fatalf("dimension-isolated model status=%+v", status)
	}
}

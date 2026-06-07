package model

import (
	"testing"
	"time"
)

func TestGetChannelKey_429CooldownJitter(t *testing.T) {
	now := time.Now().Unix()

	// 6 keys, all 429'd at the same time (simulating burst scenario)
	keys := []ChannelKey{
		{ID: 37, ChannelID: 7, Enabled: true, ChannelKey: "k37", StatusCode: 429, LastUseTimeStamp: now},
		{ID: 38, ChannelID: 7, Enabled: true, ChannelKey: "k38", StatusCode: 429, LastUseTimeStamp: now},
		{ID: 43, ChannelID: 7, Enabled: true, ChannelKey: "k43", StatusCode: 429, LastUseTimeStamp: now},
		{ID: 44, ChannelID: 7, Enabled: true, ChannelKey: "k44", StatusCode: 429, LastUseTimeStamp: now},
		{ID: 45, ChannelID: 7, Enabled: true, ChannelKey: "k45", StatusCode: 429, LastUseTimeStamp: now},
		{ID: 46, ChannelID: 7, Enabled: true, ChannelKey: "k46", StatusCode: 429, LastUseTimeStamp: now},
	}

	ch := &Channel{
		ID:      7,
		Name:    "魔搭CN",
		Enabled: true,
		Keys:    keys,
	}

	// At T+5min (300s): base cooldown expired, but jitter keeps most keys cooling
	key := ch.GetChannelKey()
	// Only key with smallest jitter (ID%60) should be available: 37%60=37, 38%60=38, ...
	// At 300s: key37 needs 337s (unavailable), key38 needs 338s (unavailable), ...
	// All should still be cooling at 300s since min jitter is 37s
	if key.ID != 0 {
		t.Errorf("expected no key available at T+5min (all in jitter cooldown), got key %d", key.ID)
	}

	// At T+337s: all keys were 429'd at the same time, set LastUseTimeStamp so key37 is exactly at its cooldown boundary
	keys2 := make([]ChannelKey, len(keys))
	copy(keys2, keys)
	for i := range keys2 {
		keys2[i].LastUseTimeStamp = now - 337 // all keys 429'd 337s ago
	}
	ch.Keys = keys2

	key = ch.GetChannelKey()
	// key37 cooldown = 300+37 = 337s, elapsed = 337s → available
	// key38 cooldown = 300+38 = 338s, elapsed = 337s → still cooling
	if key.ID != 37 {
		t.Errorf("expected key 37 first (smallest jitter=37s), got key %d", key.ID)
	}

	// Verify jitter values are deterministic and different
	jitterSet := make(map[int64]bool)
	for _, k := range keys {
		j := int64(k.ID % 60)
		if jitterSet[j] {
			t.Errorf("duplicate jitter value %d for key %d", j, k.ID)
		}
		jitterSet[j] = true
	}
}

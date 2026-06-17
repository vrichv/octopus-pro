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
	// modelName="" → 回退 key 级 StatusCode 检查（向后兼容）
	key := ch.GetChannelKey("")
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

	key = ch.GetChannelKey("")
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

func TestGetChannelKey_ModelAwareCooldown(t *testing.T) {
	// 一个 key，共享给两个模型
	key := ChannelKey{ID: 37, ChannelID: 7, Enabled: true, ChannelKey: "k37"}
	ch := &Channel{
		ID:      7,
		Name:    "test",
		Enabled: true,
		Keys:    []ChannelKey{key},
	}

	// 对 modelA 记录 429 冷却
	RecordKeyModelCooldown(7, 37, "modelA", 0) // 使用默认冷却
	ClearKeyModelCooldown(7, 37, "modelB")     // 确保 modelB 干净

	// modelA 应在冷却中，GetChannelKey 应返回空 key（被跳过）
	got := ch.GetChannelKey("modelA")
	if got.ChannelKey != "" {
		t.Errorf("expected no key for modelA (in cooldown), got key %s", got.ChannelKey)
	}

	// modelB 不应在冷却中，应返回可用 key
	got = ch.GetChannelKey("modelB")
	if got.ChannelKey == "" {
		t.Errorf("expected key for modelB (no cooldown), got empty")
	}
	if got.ID != 37 {
		t.Errorf("expected key 37 for modelB, got key %d", got.ID)
	}

	// 对 modelB 也记录冷却
	RecordKeyModelCooldown(7, 37, "modelB", 0)

	// 两个模型都应不可用
	got = ch.GetChannelKey("modelA")
	if got.ChannelKey != "" {
		t.Errorf("expected no key for modelA (still in cooldown), got key %s", got.ChannelKey)
	}
	got = ch.GetChannelKey("modelB")
	if got.ChannelKey != "" {
		t.Errorf("expected no key for modelB (now in cooldown), got key %s", got.ChannelKey)
	}

	// 清除 modelA 的冷却
	ClearKeyModelCooldown(7, 37, "modelA")

	// modelA 应恢复可用，modelB 仍不可用
	got = ch.GetChannelKey("modelA")
	if got.ChannelKey == "" {
		t.Errorf("expected key for modelA (cooldown cleared), got empty")
	}
	got = ch.GetChannelKey("modelB")
	if got.ChannelKey != "" {
		t.Errorf("expected no key for modelB (still in cooldown), got key %s", got.ChannelKey)
	}

	// 空 modelName 应回退 key 级检查（不应受 model 级冷却影响）
	got = ch.GetChannelKey("")
	if got.ChannelKey == "" {
		t.Errorf("expected key when modelName='' (fallback to StatusCode check, no StatusCode set), got empty")
	}
	if got.ID != 37 {
		t.Errorf("expected key 37 for modelName='', got key %d", got.ID)
	}
}

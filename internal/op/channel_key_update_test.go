package op

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vrichv/octopus-pro/internal/db"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/utils/cache"
	"gorm.io/gorm"
)

func resetChannelKeyTestState(t *testing.T) {
	t.Helper()
	channelKeyRuntimeLock.Lock()
	defer channelKeyRuntimeLock.Unlock()
	channelCache = cache.New[int, model.Channel](16)
	channelKeyCache = cache.New[int, model.ChannelKey](16)
	channelKeyCacheNeedUpdateLock.Lock()
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()
}

func seedChannelKey(key model.ChannelKey) {
	channel := model.Channel{ID: key.ChannelID, Enabled: true, Keys: []model.ChannelKey{key}}
	channelCache.Set(channel.ID, channel)
	channelKeyCache.Set(key.ID, key)
}

func cachedKeyPair(t *testing.T, channelID, keyID int) (model.ChannelKey, model.ChannelKey) {
	t.Helper()
	key, ok := channelKeyCache.Get(keyID)
	if !ok {
		t.Fatalf("channelKeyCache missing key %d", keyID)
	}
	channel, ok := channelCache.Get(channelID)
	if !ok {
		t.Fatalf("channelCache missing channel %d", channelID)
	}
	for _, channelKey := range channel.Keys {
		if channelKey.ID == keyID {
			return key, channelKey
		}
	}
	t.Fatalf("channelCache channel %d missing key %d", channelID, keyID)
	return model.ChannelKey{}, model.ChannelKey{}
}

func assertKeyCachesEqual(t *testing.T, channelID, keyID int) model.ChannelKey {
	t.Helper()
	key, channelKey := cachedKeyPair(t, channelID, keyID)
	if key != channelKey {
		t.Fatalf("cache mismatch: channelKeyCache=%+v channelCache=%+v", key, channelKey)
	}
	return key
}

func assertDirty(t *testing.T, keyID int) {
	t.Helper()
	channelKeyCacheNeedUpdateLock.Lock()
	_, ok := channelKeyCacheNeedUpdate[keyID]
	channelKeyCacheNeedUpdateLock.Unlock()
	if !ok {
		t.Fatalf("expected dirty mark for key %d", keyID)
	}
}

func initTestDB(t *testing.T) {
	t.Helper()
	if db.GetDB() != nil {
		_ = db.Close()
	}
	dsn := filepath.Join(t.TempDir(), "octopus-test.db")
	if err := db.InitDB("sqlite", dsn, false); err != nil {
		t.Fatalf("init sqlite db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
}

func TestChannelKeyApplyRuntimeUpdateConcurrentCostAccumulation(t *testing.T) {
	resetChannelKeyTestState(t)
	const channelID = 1
	const keyID = 10
	seedChannelKey(model.ChannelKey{ID: keyID, ChannelID: channelID, Enabled: true})

	const goroutines = 64
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ChannelKeyApplyRuntimeUpdate(ChannelKeyRuntimeUpdate{
				ChannelID:        channelID,
				KeyID:            keyID,
				StatusCode:       200,
				LastUseTimeStamp: int64(1000 + i),
				CostDelta:        1,
				AuthResult:       ChannelKeyAuthNone,
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("runtime update failed: %v", err)
	}

	key := assertKeyCachesEqual(t, channelID, keyID)
	if math.Abs(key.TotalCost-goroutines) > 0.000001 {
		t.Fatalf("TotalCost = %v, want %v", key.TotalCost, float64(goroutines))
	}
	if key.LastUseTimeStamp != 1063 {
		t.Fatalf("LastUseTimeStamp = %d, want 1063", key.LastUseTimeStamp)
	}
}

func TestChannelKeyApplyRuntimeUpdateConcurrentAuthFailures(t *testing.T) {
	resetChannelKeyTestState(t)
	initTestDB(t)
	const channelID = 2
	const keyID = 20
	seedChannelKey(model.ChannelKey{ID: keyID, ChannelID: channelID, Enabled: true})

	const eventTime = int64(2000)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ChannelKeyApplyRuntimeUpdate(ChannelKeyRuntimeUpdate{
				ChannelID:        channelID,
				KeyID:            keyID,
				StatusCode:       401,
				LastUseTimeStamp: eventTime,
				AuthResult:       ChannelKeyAuthFailure,
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("runtime update failed: %v", err)
	}

	key := assertKeyCachesEqual(t, channelID, keyID)
	if key.ConsecutiveAuthErrors != 2 {
		t.Fatalf("ConsecutiveAuthErrors = %d, want 2", key.ConsecutiveAuthErrors)
	}
	if key.LastAuthErrorTime != eventTime {
		t.Fatalf("LastAuthErrorTime = %d, want %d", key.LastAuthErrorTime, eventTime)
	}
	if key.Enabled {
		t.Fatal("Enabled = true, want false after authentication failure")
	}
}

func TestChannelKeyApplyRuntimeUpdateAuthWindowExpiration(t *testing.T) {
	resetChannelKeyTestState(t)
	initTestDB(t)
	const channelID = 3
	const keyID = 30
	const eventTime = int64(3000)
	key := model.ChannelKey{
		ID:                    keyID,
		ChannelID:             channelID,
		Enabled:               false,
		ChannelKey:            "sk-window",
		ConsecutiveAuthErrors: 2,
		LastAuthErrorTime:     eventTime - 300,
	}
	channel := model.Channel{ID: channelID, Name: "window-channel", Enabled: true}
	if err := db.GetDB().Create(&channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := db.GetDB().Create(&key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}
	seedChannelKey(key)

	key, err := ChannelKeyApplyRuntimeUpdate(ChannelKeyRuntimeUpdate{
		ChannelID:        channelID,
		KeyID:            keyID,
		StatusCode:       403,
		LastUseTimeStamp: eventTime,
		AuthResult:       ChannelKeyAuthFailure,
	})
	if err != nil {
		t.Fatalf("runtime update failed: %v", err)
	}
	if key.ConsecutiveAuthErrors != 1 {
		t.Fatalf("ConsecutiveAuthErrors = %d, want 1 after expired window reset", key.ConsecutiveAuthErrors)
	}
	if key.LastAuthErrorTime != eventTime {
		t.Fatalf("LastAuthErrorTime = %d, want %d", key.LastAuthErrorTime, eventTime)
	}
	if key.Enabled {
		t.Fatal("Enabled = true, want false after authentication failure")
	}
	assertKeyCachesEqual(t, channelID, keyID)
}

func TestChannelKeyApplyRuntimeUpdateSuccessResetsAuthAndAddsCost(t *testing.T) {
	resetChannelKeyTestState(t)
	const channelID = 4
	const keyID = 40
	seedChannelKey(model.ChannelKey{
		ID:                    keyID,
		ChannelID:             channelID,
		Enabled:               true,
		TotalCost:             2.5,
		ConsecutiveAuthErrors: 2,
		LastAuthErrorTime:     3500,
	})

	_, err := ChannelKeyApplyRuntimeUpdate(ChannelKeyRuntimeUpdate{
		ChannelID:        channelID,
		KeyID:            keyID,
		StatusCode:       200,
		LastUseTimeStamp: 4000,
		CostDelta:        1.25,
		AuthResult:       ChannelKeyAuthSuccess,
	})
	if err != nil {
		t.Fatalf("runtime update failed: %v", err)
	}

	key := assertKeyCachesEqual(t, channelID, keyID)
	if key.ConsecutiveAuthErrors != 0 {
		t.Fatalf("ConsecutiveAuthErrors = %d, want 0", key.ConsecutiveAuthErrors)
	}
	if key.LastAuthErrorTime != 0 {
		t.Fatalf("LastAuthErrorTime = %d, want 0", key.LastAuthErrorTime)
	}
	if math.Abs(key.TotalCost-3.75) > 0.000001 {
		t.Fatalf("TotalCost = %v, want 3.75", key.TotalCost)
	}
	assertDirty(t, keyID)
}

func TestChannelKeyApplyRuntimeUpdateSuccessRestoresEnabledInDB(t *testing.T) {
	resetChannelKeyTestState(t)
	initTestDB(t)
	const channelID = 5
	const keyID = 50
	channel := model.Channel{ID: channelID, Name: "test-channel", Enabled: true}
	if err := db.GetDB().Create(&channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	key := model.ChannelKey{ID: keyID, ChannelID: channelID, Enabled: false, ChannelKey: "sk-test", ConsecutiveAuthErrors: 3, LastAuthErrorTime: 4500}
	if err := db.GetDB().Create(&key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}
	seedChannelKey(key)

	_, err := ChannelKeyApplyRuntimeUpdate(ChannelKeyRuntimeUpdate{
		ChannelID:        channelID,
		KeyID:            keyID,
		StatusCode:       200,
		LastUseTimeStamp: 5000,
		AuthResult:       ChannelKeyAuthSuccess,
	})
	if err != nil {
		t.Fatalf("runtime update failed: %v", err)
	}

	var dbKey model.ChannelKey
	if err := db.GetDB().First(&dbKey, keyID).Error; err != nil {
		t.Fatalf("load db key: %v", err)
	}
	if !dbKey.Enabled {
		t.Fatalf("db enabled = false, want true")
	}
	cached := assertKeyCachesEqual(t, channelID, keyID)
	if !cached.Enabled {
		t.Fatalf("cache enabled = false, want true")
	}
}

func TestChannelKeySaveDBFailureRefillsDirtyKeys(t *testing.T) {
	resetChannelKeyTestState(t)
	initTestDB(t)
	const channelID = 6
	channel := model.Channel{ID: channelID, Name: "save-failure-channel", Enabled: true}
	if err := db.GetDB().Create(&channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	for _, keyID := range []int{61, 62, 63} {
		key := model.ChannelKey{ID: keyID, ChannelID: channelID, Enabled: true, ChannelKey: fmt.Sprintf("sk-%d", keyID)}
		if err := db.GetDB().Create(&key).Error; err != nil {
			t.Fatalf("create key %d: %v", keyID, err)
		}
		channelKeyCache.Set(keyID, key)
		markChannelKeyCacheNeedUpdate(keyID)
	}

	dbConn := db.GetDB()
	if err := dbConn.Callback().Update().Before("gorm:update").Register("test:fail_key_62", func(tx *gorm.DB) {
		key, ok := tx.Statement.Dest.(*model.ChannelKey)
		if !ok || key.ID != 62 {
			return
		}
		markChannelKeyCacheNeedUpdate(61)
		tx.AddError(errors.New("forced save failure"))
	}); err != nil {
		t.Fatalf("register update callback: %v", err)
	}

	err := ChannelKeySaveDB(context.Background())
	if err == nil {
		t.Fatalf("ChannelKeySaveDB succeeded, want forced save error")
	}

	channelKeyCacheNeedUpdateLock.Lock()
	_, has61 := channelKeyCacheNeedUpdate[61]
	_, has62 := channelKeyCacheNeedUpdate[62]
	_, has63 := channelKeyCacheNeedUpdate[63]
	channelKeyCacheNeedUpdateLock.Unlock()
	if !has61 || !has62 || !has63 {
		t.Fatalf("dirty map after failure has 61=%t 62=%t 63=%t, want all true", has61, has62, has63)
	}
}

func TestChannelUpdatePreservesDirtyKeyRuntimeState(t *testing.T) {
	initTestDB(t)
	resetChannelKeyTestState(t)

	channel := model.Channel{ID: 1, Name: "before", Enabled: true}
	if err := db.GetDB().Create(&channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	stored := model.ChannelKey{ID: 10, ChannelID: channel.ID, Enabled: true, ChannelKey: "sk-test"}
	if err := db.GetDB().Create(&stored).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}
	live := stored
	live.TotalCost = 12.5
	live.StatusCode = 429
	live.LastUseTimeStamp = 100
	live.RetryAfter = 60
	live.ConsecutiveAuthErrors = 2
	live.LastAuthErrorTime = 99
	seedChannelKey(live)
	markChannelKeyCacheNeedUpdate(live.ID)

	name := "after"
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, Name: &name}, context.Background()); err != nil {
		t.Fatalf("ChannelUpdate: %v", err)
	}
	got := assertKeyCachesEqual(t, channel.ID, live.ID)
	if got.TotalCost != live.TotalCost || got.StatusCode != live.StatusCode || got.LastUseTimeStamp != live.LastUseTimeStamp || got.RetryAfter != live.RetryAfter || got.ConsecutiveAuthErrors != live.ConsecutiveAuthErrors || got.LastAuthErrorTime != live.LastAuthErrorTime {
		t.Fatalf("runtime state was overwritten: got %+v want %+v", got, live)
	}
	assertDirty(t, live.ID)
}

package op

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/vrichv/octopus-pro/internal/db"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/utils/cache"
	"gorm.io/gorm"
)

func resetStatsTestState(t *testing.T) {
	t.Helper()

	statsDailyCacheLock.Lock()
	statsDailyCache = model.StatsDaily{}
	statsDailyCacheLock.Unlock()

	statsTotalCacheLock.Lock()
	statsTotalCache = model.StatsTotal{}
	statsTotalCacheLock.Unlock()

	statsHourlyCacheLock.Lock()
	statsHourlyCache = [24]model.StatsHourly{}
	statsHourlyCacheLock.Unlock()

	statsChannelCacheLock.Lock()
	statsChannelCache = cache.New[int, model.StatsChannel](16)
	statsChannelCacheNeedUpdateLock.Lock()
	statsChannelCacheNeedUpdate = make(map[int]struct{})
	statsChannelCacheNeedUpdateLock.Unlock()
	statsChannelCacheLock.Unlock()

	statsModelCacheLock.Lock()
	statsModelCache = cache.New[int, model.StatsModel](16)
	statsModelCacheNeedUpdateLock.Lock()
	statsModelCacheNeedUpdate = make(map[int]struct{})
	statsModelCacheNeedUpdateLock.Unlock()
	statsModelCacheLock.Unlock()

	statsAPIKeyCacheLock.Lock()
	statsAPIKeyCache = cache.New[int, model.StatsAPIKey](16)
	statsAPIKeyCacheNeedUpdateLock.Lock()
	statsAPIKeyCacheNeedUpdate = make(map[int]struct{})
	statsAPIKeyCacheNeedUpdateLock.Unlock()
	statsAPIKeyCacheLock.Unlock()
}

func assertStatsMetricsEqual(t *testing.T, got model.StatsMetrics, want model.StatsMetrics) {
	t.Helper()
	if got.InputToken != want.InputToken || got.OutputToken != want.OutputToken || got.WaitTime != want.WaitTime || got.RequestSuccess != want.RequestSuccess || got.RequestFailed != want.RequestFailed {
		t.Fatalf("metrics counts = %+v, want %+v", got, want)
	}
	if math.Abs(got.InputCost-want.InputCost) > 1e-9 || math.Abs(got.OutputCost-want.OutputCost) > 1e-9 {
		t.Fatalf("metrics costs = %+v, want %+v", got, want)
	}
}

func TestStatsConcurrentUpdatesAccumulateAllDeltas(t *testing.T) {
	resetStatsTestState(t)

	const goroutines = 64
	const iterations = 400
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				metrics := model.StatsMetrics{
					InputToken:     1,
					OutputToken:    2,
					InputCost:      0.25,
					OutputCost:     0.5,
					WaitTime:       3,
					RequestSuccess: 1,
					RequestFailed:  1,
				}
				if err := StatsChannelUpdate(101, metrics); err != nil {
					t.Errorf("StatsChannelUpdate: %v", err)
				}
				if err := StatsAPIKeyUpdate(202, metrics); err != nil {
					t.Errorf("StatsAPIKeyUpdate: %v", err)
				}
				if err := StatsModelUpdate(model.StatsModel{
					ID:        303,
					Name:      "gpt-test",
					ChannelID: 404,
					StatsMetrics: model.StatsMetrics{
						InputToken:     1,
						OutputToken:    2,
						InputCost:      0.25,
						OutputCost:     0.5,
						WaitTime:       3,
						RequestSuccess: 1,
						RequestFailed:  1,
					},
				}); err != nil {
					t.Errorf("StatsModelUpdate: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()

	totalUpdates := int64(goroutines * iterations)
	want := model.StatsMetrics{
		InputToken:     totalUpdates,
		OutputToken:    totalUpdates * 2,
		InputCost:      float64(totalUpdates) * 0.25,
		OutputCost:     float64(totalUpdates) * 0.5,
		WaitTime:       totalUpdates * 3,
		RequestSuccess: totalUpdates,
		RequestFailed:  totalUpdates,
	}

	assertStatsMetricsEqual(t, StatsChannelGet(101).StatsMetrics, want)
	assertStatsMetricsEqual(t, StatsAPIKeyGet(202).StatsMetrics, want)

	statsModelCacheLock.Lock()
	modelStats, ok := statsModelCache.Get(303)
	statsModelCacheLock.Unlock()
	if !ok {
		t.Fatalf("statsModelCache missing model 303")
	}
	if modelStats.Name != "gpt-test" || modelStats.ChannelID != 404 {
		t.Fatalf("model identity = name %q channel %d, want gpt-test/404", modelStats.Name, modelStats.ChannelID)
	}
	assertStatsMetricsEqual(t, modelStats.StatsMetrics, want)
}

func TestStatsModelUpdatePreservesExistingIdentity(t *testing.T) {
	resetStatsTestState(t)

	statsModelCacheLock.Lock()
	statsModelCache.Set(7, model.StatsModel{ID: 7, Name: "existing-model", ChannelID: 8})
	statsModelCacheLock.Unlock()

	if err := StatsModelUpdate(model.StatsModel{ID: 7, StatsMetrics: model.StatsMetrics{InputToken: 3}}); err != nil {
		t.Fatalf("StatsModelUpdate: %v", err)
	}

	statsModelCacheLock.Lock()
	modelStats, ok := statsModelCache.Get(7)
	statsModelCacheLock.Unlock()
	if !ok {
		t.Fatalf("statsModelCache missing model 7")
	}
	if modelStats.Name != "existing-model" || modelStats.ChannelID != 8 {
		t.Fatalf("model identity = name %q channel %d, want existing-model/8", modelStats.Name, modelStats.ChannelID)
	}
	if modelStats.InputToken != 3 {
		t.Fatalf("InputToken = %d, want 3", modelStats.InputToken)
	}
}

func TestStatsSaveDBFailureRestoresDirtyIDs(t *testing.T) {
	resetStatsTestState(t)
	initTestDB(t)

	statsTotalCacheLock.Lock()
	statsTotalCache = model.StatsTotal{ID: 1}
	statsTotalCacheLock.Unlock()
	statsDailyCacheLock.Lock()
	statsDailyCache = model.StatsDaily{Date: time.Now().Format("20060102")}
	statsDailyCacheLock.Unlock()

	const apiKeyID = 902
	initial := model.StatsAPIKey{APIKeyID: apiKeyID}
	if err := db.GetDB().Create(&initial).Error; err != nil {
		t.Fatalf("create stats api key: %v", err)
	}
	if err := StatsAPIKeyUpdate(apiKeyID, model.StatsMetrics{InputToken: 11}); err != nil {
		t.Fatalf("StatsAPIKeyUpdate: %v", err)
	}

	dbConn := db.GetDB()
	callbackName := fmt.Sprintf("test:fail_stats_api_key_%d", apiKeyID)
	if err := dbConn.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		stats, ok := tx.Statement.Dest.(*model.StatsAPIKey)
		if !ok || stats.APIKeyID != apiKeyID {
			return
		}
		if err := StatsAPIKeyUpdate(apiKeyID, model.StatsMetrics{OutputToken: 13}); err != nil {
			tx.AddError(err)
			return
		}
		tx.AddError(errors.New("forced stats save failure"))
	}); err != nil {
		t.Fatalf("register update callback: %v", err)
	}

	err := StatsSaveDB(context.Background())
	if err == nil {
		t.Fatalf("StatsSaveDB succeeded, want forced save error")
	}

	statsAPIKeyCacheNeedUpdateLock.Lock()
	_, stillDirty := statsAPIKeyCacheNeedUpdate[apiKeyID]
	statsAPIKeyCacheNeedUpdateLock.Unlock()
	if !stillDirty {
		t.Fatalf("dirty map missing api key %d after failed save", apiKeyID)
	}

	if err := dbConn.Callback().Update().Remove(callbackName); err != nil {
		t.Fatalf("remove update callback: %v", err)
	}
	if err := StatsSaveDB(context.Background()); err != nil {
		t.Fatalf("StatsSaveDB retry: %v", err)
	}

	statsAPIKeyCacheNeedUpdateLock.Lock()
	_, stillDirty = statsAPIKeyCacheNeedUpdate[apiKeyID]
	statsAPIKeyCacheNeedUpdateLock.Unlock()
	if stillDirty {
		t.Fatalf("dirty map still has api key %d after successful retry", apiKeyID)
	}

	var saved model.StatsAPIKey
	if err := db.GetDB().First(&saved, "api_key_id = ?", apiKeyID).Error; err != nil {
		t.Fatalf("load saved api key stats: %v", err)
	}
	if saved.InputToken != 11 || saved.OutputToken != 13 {
		t.Fatalf("saved metrics = %+v, want input 11 output 13", saved.StatsMetrics)
	}
}

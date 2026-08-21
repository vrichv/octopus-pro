package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrichv/octopus-pro/internal/helper"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/utils/diff"
	"github.com/vrichv/octopus-pro/internal/utils/log"
	"github.com/vrichv/octopus-pro/internal/utils/xstrings"
)

var ErrModelSyncRunning = errors.New("model sync is already running")

type ModelSyncStatus struct {
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	LastError  string    `json:"last_error,omitempty"`
}

var (
	lastSyncModelsTime   = time.Now()
	lastSyncModelsTimeMu sync.RWMutex
	syncModelsRunning    atomic.Bool
	syncModelsStatusMu   sync.RWMutex
	syncModelsStatus     ModelSyncStatus
)

func dedupeModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	nextModels := make([]string, 0, len(models))
	for _, modelName := range models {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			continue
		}
		if _, ok := seen[modelName]; ok {
			continue
		}
		seen[modelName] = struct{}{}
		nextModels = append(nextModels, modelName)
	}
	return nextModels
}

func syncChannelModels(fetchedModels, excludedModels, customModels []string) (selectedModels []string, nextExcludedModels []string) {
	fetchedSet := make(map[string]struct{}, len(fetchedModels))
	for _, modelName := range fetchedModels {
		fetchedSet[strings.ToLower(modelName)] = struct{}{}
	}

	nextExcludedSet := make(map[string]struct{}, len(excludedModels))
	nextExcludedModels = make([]string, 0, len(excludedModels))
	for _, modelName := range excludedModels {
		if _, ok := fetchedSet[strings.ToLower(modelName)]; !ok {
			continue
		}
		nextExcludedSet[strings.ToLower(modelName)] = struct{}{}
		nextExcludedModels = append(nextExcludedModels, modelName)
	}

	customSet := make(map[string]struct{}, len(customModels))
	for _, modelName := range customModels {
		customSet[strings.ToLower(modelName)] = struct{}{}
	}
	selectedModels = make([]string, 0, len(fetchedModels))
	for _, modelName := range fetchedModels {
		key := strings.ToLower(modelName)
		if _, ok := nextExcludedSet[key]; ok {
			continue
		}
		if _, ok := customSet[key]; ok {
			continue
		}
		selectedModels = append(selectedModels, modelName)
	}
	return selectedModels, nextExcludedModels
}

func addReferencedModels(names []string, seen map[string]struct{}, referenced *[]string) {
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		*referenced = append(*referenced, name)
	}
}

// SyncModelsTask is the scheduler entrypoint. It never blocks the scheduler
// while a model sync performs upstream requests.
func SyncModelsTask() {
	if err := StartModelSync(); err != nil && !errors.Is(err, ErrModelSyncRunning) {
		log.Errorf("failed to start model sync: %v", err)
	}
}

// StartModelSync starts one background synchronization. Callers receive
// ErrModelSyncRunning rather than waiting behind an already-running job.
func StartModelSync() error {
	if !syncModelsRunning.CompareAndSwap(false, true) {
		return ErrModelSyncRunning
	}
	now := time.Now()
	syncModelsStatusMu.Lock()
	syncModelsStatus = ModelSyncStatus{Running: true, StartedAt: now}
	syncModelsStatusMu.Unlock()
	go func() {
		err := runModelSync()
		finishedAt := time.Now()
		syncModelsStatusMu.Lock()
		syncModelsStatus.Running = false
		syncModelsStatus.FinishedAt = finishedAt
		if err != nil {
			syncModelsStatus.LastError = err.Error()
		} else {
			syncModelsStatus.LastError = ""
		}
		syncModelsStatusMu.Unlock()
		syncModelsRunning.Store(false)
	}()
	return nil
}

func GetModelSyncStatus() ModelSyncStatus {
	syncModelsStatusMu.RLock()
	defer syncModelsStatusMu.RUnlock()
	return syncModelsStatus
}

func runModelSync() error {
	log.Debugf("sync models task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("sync models task finished, sync time: %s", time.Since(startTime))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	channels, err := op.ChannelList(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}
	referencedModels := make([]string, 0, 128)
	seenReferencedModels := make(map[string]struct{}, 128)
	for _, channel := range channels {
		oldModels := dedupeModels(xstrings.SplitTrimCompact(",", channel.Model))
		customModels := dedupeModels(xstrings.SplitTrimCompact(",", channel.CustomModel))
		if !channel.AutoSync {
			addReferencedModels(oldModels, seenReferencedModels, &referencedModels)
			addReferencedModels(customModels, seenReferencedModels, &referencedModels)
			continue
		}

		fetchedModels, err := helper.FetchModels(ctx, channel)
		if err != nil {
			log.Warnf("failed to fetch models for channel %s: %v", channel.Name, err)
			addReferencedModels(oldModels, seenReferencedModels, &referencedModels)
			addReferencedModels(customModels, seenReferencedModels, &referencedModels)
			continue
		}
		fetchedModels = dedupeModels(fetchedModels)
		nextModels, nextExcludedModels := syncChannelModels(fetchedModels, dedupeModels(xstrings.SplitTrimCompact(",", channel.ExcludedModel)), customModels)
		deletedModels, addedModels := diff.Diff(oldModels, nextModels)
		nextModelStr := strings.Join(nextModels, ",")
		nextExcludedModelStr := strings.Join(nextExcludedModels, ",")
		if len(deletedModels) > 0 || len(addedModels) > 0 || nextExcludedModelStr != channel.ExcludedModel {
			if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{
				ID:            channel.ID,
				Model:         &nextModelStr,
				ExcludedModel: &nextExcludedModelStr,
			}, ctx); err != nil {
				log.Errorf("failed to update channel %s: %v", channel.Name, err)
				addReferencedModels(oldModels, seenReferencedModels, &referencedModels)
				addReferencedModels(customModels, seenReferencedModels, &referencedModels)
				continue
			}
			channel.Model = nextModelStr
			channel.ExcludedModel = nextExcludedModelStr
		}
		addReferencedModels(nextModels, seenReferencedModels, &referencedModels)
		addReferencedModels(customModels, seenReferencedModels, &referencedModels)
		if len(deletedModels) > 0 {
			keys := make([]model.GroupIDAndLLMName, len(deletedModels))
			for i, m := range deletedModels {
				keys[i] = model.GroupIDAndLLMName{ChannelID: channel.ID, ModelName: m}
			}
			if err := op.GroupItemBatchDelByChannelAndModels(keys, ctx); err != nil {
				log.Errorf("failed to batch delete group items for channel %s: %v", channel.Name, err)
			}
		}
		if len(nextModels) > 0 {
			helper.ChannelAutoGroup(&channel, ctx)
		}
	}
	llmPrice, err := op.LLMList(ctx)
	if err != nil {
		return fmt.Errorf("list model prices: %w", err)
	}
	llmPriceNames := make([]string, 0, len(llmPrice))
	for _, price := range llmPrice {
		llmPriceNames = append(llmPriceNames, price.Name)
	}
	deletedNorm, addedNorm := diff.Diff(llmPriceNames, referencedModels)
	if len(deletedNorm) > 0 {
		if err := helper.LLMPriceDeleteFromDBWithNoPrice(deletedNorm, ctx); err != nil {
			log.Errorf("failed to batch delete models price: %v", err)
		}
	}
	if len(addedNorm) > 0 {
		if err := helper.LLMPriceAddToDB(addedNorm, ctx); err != nil {
			log.Errorf("failed to add models price: %v", err)
		}
	}
	lastSyncModelsTimeMu.Lock()
	lastSyncModelsTime = time.Now()
	lastSyncModelsTimeMu.Unlock()
	return nil
}

func GetLastSyncModelsTime() time.Time {
	lastSyncModelsTimeMu.RLock()
	defer lastSyncModelsTimeMu.RUnlock()
	return lastSyncModelsTime
}

package task

import (
	"context"
	"strings"
	"time"

	"github.com/vrichv/octopus-pro/internal/helper"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/utils/diff"
	"github.com/vrichv/octopus-pro/internal/utils/log"
	"github.com/vrichv/octopus-pro/internal/utils/xstrings"
)

var lastSyncModelsTime = time.Now()

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

func syncChannelModels(fetchedModels []string, excludedModels []string) (selectedModels []string, nextExcludedModels []string) {
	fetchedSet := make(map[string]struct{}, len(fetchedModels))
	for _, modelName := range fetchedModels {
		fetchedSet[modelName] = struct{}{}
	}

	nextExcludedSet := make(map[string]struct{}, len(excludedModels))
	nextExcludedModels = make([]string, 0, len(excludedModels))
	for _, modelName := range excludedModels {
		if _, ok := fetchedSet[modelName]; !ok {
			continue
		}
		nextExcludedSet[modelName] = struct{}{}
		nextExcludedModels = append(nextExcludedModels, modelName)
	}

	selectedModels = make([]string, 0, len(fetchedModels))
	for _, modelName := range fetchedModels {
		if _, ok := nextExcludedSet[modelName]; ok {
			continue
		}
		selectedModels = append(selectedModels, modelName)
	}
	return selectedModels, nextExcludedModels
}

// SyncModelsTask 同步模型任务
func SyncModelsTask() {
	log.Debugf("sync models task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("sync models task finished, sync time: %s", time.Since(startTime))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	channels, err := op.ChannelList(ctx)
	if err != nil {
		log.Errorf("failed to list channels: %v", err)
		return
	}
	totalNewModels := make([]string, 0, 128)
	seenTotalNewModels := make(map[string]struct{}, 128)
	for _, channel := range channels {
		if !channel.AutoSync {
			continue
		}
		fetchModels, err := helper.FetchModels(ctx, channel)
		if err != nil {
			log.Warnf("failed to fetch models for channel %s: %v", channel.Name, err)
			continue
		}
		oldModels := dedupeModels(xstrings.SplitTrimCompact(",", channel.Model))
		fetchedModels := dedupeModels(fetchModels)
		for _, m := range fetchedModels {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			m = strings.ToLower(m)
			if _, ok := seenTotalNewModels[m]; ok {
				continue
			}
			seenTotalNewModels[m] = struct{}{}
			totalNewModels = append(totalNewModels, m)
		}
		nextModels, nextExcludedModels := syncChannelModels(fetchedModels, dedupeModels(xstrings.SplitTrimCompact(",", channel.ExcludedModel)))
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
				continue
			}
			channel.Model = nextModelStr
			channel.ExcludedModel = nextExcludedModelStr
		}
		// 批量删除消失的模型对应的 GroupItem
		if len(deletedModels) > 0 {
			log.Infof("deleted channel %s models: %v", channel.Name, deletedModels)
			keys := make([]model.GroupIDAndLLMName, len(deletedModels))
			for i, m := range deletedModels {
				keys[i] = model.GroupIDAndLLMName{ChannelID: channel.ID, ModelName: m}
			}
			if err := op.GroupItemBatchDelByChannelAndModels(keys, ctx); err != nil {
				log.Errorf("failed to batch delete group items for channel %s: %v", channel.Name, err)
			}
		}

		// 自动分组
		if len(nextModels) > 0 {
			helper.ChannelAutoGroup(&channel, ctx)
		}
	}
	llmPrice, err := op.LLMList(ctx)
	if err != nil {
		log.Errorf("failed to list models price: %v", err)
		return
	}
	llmPriceNames := make([]string, 0, len(llmPrice))
	for _, price := range llmPrice {
		llmPriceNames = append(llmPriceNames, price.Name)
	}

	deletedNorm, addedNorm := diff.Diff(llmPriceNames, totalNewModels)
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
	lastSyncModelsTime = time.Now()
}

func GetLastSyncModelsTime() time.Time {
	return lastSyncModelsTime
}

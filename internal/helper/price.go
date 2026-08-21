package helper

import (
	"context"

	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/price"
)

func LLMPriceAddToDB(modelNames []string, ctx context.Context) error {
	newLLMInfos := make([]model.LLMInfo, 0, len(modelNames))
	updatedLLMInfos := make([]model.LLMInfo, 0, len(modelNames))
	for _, modelName := range modelNames {
		if modelName == "" {
			continue
		}

		calibratedPrice := price.LookupCalibratedPrice(modelName)
		if existing, err := op.LLMGet(modelName); err == nil {
			if existing.Input != 0 || existing.Output != 0 || existing.CacheRead != 0 || existing.CacheWrite != 0 {
				continue
			}
			if calibratedPrice != nil {
				updatedLLMInfos = append(updatedLLMInfos, model.LLMInfo{Name: modelName, LLMPrice: model.LLMPrice{
					Input:      calibratedPrice.Input,
					Output:     calibratedPrice.Output,
					CacheRead:  calibratedPrice.CacheRead,
					CacheWrite: calibratedPrice.CacheWrite,
					MaxContext: existing.MaxContext,
				}})
			}
			continue
		}

		if calibratedPrice != nil {
			newLLMInfos = append(newLLMInfos, model.LLMInfo{Name: modelName, LLMPrice: *calibratedPrice})
		} else {
			newLLMInfos = append(newLLMInfos, model.LLMInfo{Name: modelName})
		}
	}
	if len(newLLMInfos) > 0 {
		if err := op.LLMBatchCreate(newLLMInfos, ctx); err != nil {
			return err
		}
	}
	if len(updatedLLMInfos) > 0 {
		return op.LLMBatchSave(updatedLLMInfos, ctx)
	}
	return nil
}

func LLMPriceDeleteFromDBWithNoPrice(modelNames []string, ctx context.Context) error {
	if len(modelNames) == 0 {
		return nil
	}
	needDeleteModelNames := make([]string, 0, len(modelNames))
	for _, modelName := range modelNames {
		if modelName == "" {
			continue
		}
		modelPrice, err := op.LLMGet(modelName)
		if err != nil {
			return err
		}
		if modelPrice.Input != 0 || modelPrice.Output != 0 || modelPrice.CacheRead != 0 || modelPrice.CacheWrite != 0 {
			continue
		}
		needDeleteModelNames = append(needDeleteModelNames, modelName)
	}
	if len(needDeleteModelNames) > 0 {
		return op.LLMBatchDelete(needDeleteModelNames, ctx)
	}
	return nil
}

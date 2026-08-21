package op

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/vrichv/octopus-pro/internal/db"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/utils/cache"
	"github.com/vrichv/octopus-pro/internal/utils/log"
	"github.com/vrichv/octopus-pro/internal/utils/xstrings"
)

var channelCache = cache.New[int, model.Channel](16)
var channelKeyCache = cache.New[int, model.ChannelKey](16)
var channelKeyCacheNeedUpdate = make(map[int]struct{})
var channelKeyCacheNeedUpdateLock sync.Mutex
var channelKeyRuntimeLock sync.Mutex

type ChannelKeyAuthResult int

const (
	ChannelKeyAuthNone ChannelKeyAuthResult = iota
	ChannelKeyAuthSuccess
	ChannelKeyAuthFailure
)

type ChannelKeyRuntimeUpdate struct {
	ChannelID        int
	KeyID            int
	StatusCode       int
	LastUseTimeStamp int64
	CostDelta        float64
	RetryAfter       int64
	AuthResult       ChannelKeyAuthResult
}

func ChannelList(ctx context.Context) ([]model.Channel, error) {
	channels := make([]model.Channel, 0, channelCache.Len())
	for _, channel := range channelCache.GetAll() {
		channels = append(channels, channel)
	}
	return channels, nil
}

func ChannelCreate(channel *model.Channel, ctx context.Context) error {
	if err := db.GetDB().WithContext(ctx).Create(channel).Error; err != nil {
		return err
	}
	channelCache.Set(channel.ID, *channel)
	for _, k := range channel.Keys {
		if k.ID != 0 {
			channelKeyCache.Set(k.ID, k)
		}
	}
	return nil
}

// ChannelKeyUpdate updates a complete ChannelKey in the in-memory caches for management and compatibility paths.
// Relay runtime updates must use ChannelKeyApplyRuntimeUpdate so concurrent attempts merge deltas instead of overwriting state.
// Enabled changes are written to DB immediately; runtime fields are marked for delayed persistence.
func ChannelKeyUpdate(key model.ChannelKey) error {
	if key.ID == 0 || key.ChannelID == 0 {
		return fmt.Errorf("invalid channel key")
	}

	channelKeyRuntimeLock.Lock()
	defer channelKeyRuntimeLock.Unlock()

	ch, ok := channelCache.Get(key.ChannelID)
	if !ok {
		return fmt.Errorf("channel not found")
	}

	if len(ch.Keys) > 0 {
		keys := make([]model.ChannelKey, len(ch.Keys))
		copy(keys, ch.Keys)
		for i := range keys {
			if keys[i].ID == key.ID {
				if keys[i].Enabled != key.Enabled {
					if err := updateChannelKeyEnabledDB(key.ID, key.Enabled); err != nil {
						return err
					}
				}
				keys[i] = key
				break
			}
		}
		ch.Keys = keys
	}
	channelCache.Set(key.ChannelID, ch)
	channelKeyCache.Set(key.ID, key)
	markChannelKeyCacheNeedUpdate(key.ID)
	return nil
}

func ChannelKeyApplyRuntimeUpdate(update ChannelKeyRuntimeUpdate) (model.ChannelKey, error) {
	if update.ChannelID == 0 || update.KeyID == 0 {
		return model.ChannelKey{}, fmt.Errorf("invalid channel key runtime update")
	}

	channelKeyRuntimeLock.Lock()
	defer channelKeyRuntimeLock.Unlock()

	ch, ok := channelCache.Get(update.ChannelID)
	if !ok {
		return model.ChannelKey{}, fmt.Errorf("channel not found")
	}
	if len(ch.Keys) == 0 {
		return model.ChannelKey{}, fmt.Errorf("channel key not found")
	}

	keys := make([]model.ChannelKey, len(ch.Keys))
	copy(keys, ch.Keys)
	keyIndex := -1
	for i := range keys {
		if keys[i].ID == update.KeyID {
			keyIndex = i
			break
		}
	}
	if keyIndex < 0 {
		return model.ChannelKey{}, fmt.Errorf("channel key not found")
	}

	current := keys[keyIndex]
	nextKey := current
	nextKey.TotalCost = current.TotalCost + update.CostDelta
	nextKey.StatusCode = update.StatusCode
	if update.LastUseTimeStamp > current.LastUseTimeStamp {
		nextKey.LastUseTimeStamp = update.LastUseTimeStamp
	}
	if update.RetryAfter > 0 {
		nextKey.RetryAfter = update.RetryAfter
	}

	switch update.AuthResult {
	case ChannelKeyAuthNone:
	case ChannelKeyAuthSuccess:
		nextKey.ConsecutiveAuthErrors = 0
		nextKey.LastAuthErrorTime = 0
		nextKey.Enabled = true
	case ChannelKeyAuthFailure:
		eventTime := update.LastUseTimeStamp
		if eventTime == 0 {
			eventTime = time.Now().Unix()
		}
		if current.LastAuthErrorTime > 0 && eventTime-current.LastAuthErrorTime >= 300 {
			nextKey.ConsecutiveAuthErrors = 0
			nextKey.LastAuthErrorTime = 0
			nextKey.Enabled = true
		}
		nextKey.ConsecutiveAuthErrors++
		nextKey.LastAuthErrorTime = eventTime
		if nextKey.ConsecutiveAuthErrors >= 3 {
			nextKey.Enabled = false
		}
	default:
		return model.ChannelKey{}, fmt.Errorf("invalid channel key auth result")
	}

	if current.Enabled != nextKey.Enabled {
		if err := updateChannelKeyEnabledDB(update.KeyID, nextKey.Enabled); err != nil {
			return model.ChannelKey{}, err
		}
	}

	keys[keyIndex] = nextKey
	ch.Keys = keys
	channelCache.Set(update.ChannelID, ch)
	channelKeyCache.Set(update.KeyID, nextKey)
	markChannelKeyCacheNeedUpdate(update.KeyID)
	return nextKey, nil
}

func updateChannelKeyEnabledDB(keyID int, enabled bool) error {
	dbConn := db.GetDB()
	if dbConn == nil {
		return fmt.Errorf("db not initialized")
	}
	if err := dbConn.Model(&model.ChannelKey{}).Where("id = ?", keyID).Update("enabled", enabled).Error; err != nil {
		return fmt.Errorf("failed to update key enabled in db: %w", err)
	}
	return nil
}

func markChannelKeyCacheNeedUpdate(keyID int) {
	channelKeyCacheNeedUpdateLock.Lock()
	channelKeyCacheNeedUpdate[keyID] = struct{}{}
	channelKeyCacheNeedUpdateLock.Unlock()
}

func ChannelBaseUrlUpdate(channelID int, baseUrl []model.BaseUrl) error {
	ch, ok := channelCache.Get(channelID)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	// Copy to decouple callers from internal cache storage.
	if baseUrl == nil {
		ch.BaseUrls = nil
	} else {
		cp := make([]model.BaseUrl, len(baseUrl))
		copy(cp, baseUrl)
		ch.BaseUrls = cp
	}
	channelCache.Set(channelID, ch)
	return nil
}

// ChannelKeySaveDB writes runtime key updates using a snapshot that cannot be
// replaced by a concurrent management cache refresh.
func ChannelKeySaveDB(ctx context.Context) error {
	channelKeyRuntimeLock.Lock()
	defer channelKeyRuntimeLock.Unlock()

	channelKeyCacheNeedUpdateLock.Lock()
	keyIDs := make([]int, 0, len(channelKeyCacheNeedUpdate))
	for id := range channelKeyCacheNeedUpdate {
		keyIDs = append(keyIDs, id)
	}
	sort.Ints(keyIDs)
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()

	if len(keyIDs) == 0 {
		return nil
	}

	dbConn := db.GetDB().WithContext(ctx)
	for i, id := range keyIDs {
		k, ok := channelKeyCache.Get(id)
		if !ok {
			continue
		}
		if err := dbConn.Save(&k).Error; err != nil {
			channelKeyCacheNeedUpdateLock.Lock()
			for _, dirtyID := range keyIDs[i:] {
				channelKeyCacheNeedUpdate[dirtyID] = struct{}{}
			}
			channelKeyCacheNeedUpdateLock.Unlock()
			return err
		}
	}
	return nil
}

func ChannelUpdate(req *model.ChannelUpdateRequest, ctx context.Context) (*model.Channel, error) {
	_, ok := channelCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var selectFields []string
	updates := model.Channel{ID: req.ID}

	if req.Name != nil {
		selectFields = append(selectFields, "name")
		updates.Name = *req.Name
	}
	if req.Type != nil {
		selectFields = append(selectFields, "type")
		updates.Type = *req.Type
	}
	if req.Enabled != nil {
		selectFields = append(selectFields, "enabled")
		updates.Enabled = *req.Enabled
	}
	if req.BaseUrls != nil {
		selectFields = append(selectFields, "base_urls")
		updates.BaseUrls = *req.BaseUrls
	}
	if req.Model != nil {
		selectFields = append(selectFields, "model")
		updates.Model = *req.Model
	}
	if req.CustomModel != nil {
		selectFields = append(selectFields, "custom_model")
		updates.CustomModel = *req.CustomModel
	}
	if req.ExcludedModel != nil {
		selectFields = append(selectFields, "excluded_model")
		updates.ExcludedModel = *req.ExcludedModel
	}
	if req.Proxy != nil {
		selectFields = append(selectFields, "proxy")
		updates.Proxy = *req.Proxy
	}
	if req.AutoSync != nil {
		selectFields = append(selectFields, "auto_sync")
		updates.AutoSync = *req.AutoSync
	}
	if req.AutoGroup != nil {
		selectFields = append(selectFields, "auto_group")
		updates.AutoGroup = *req.AutoGroup
	}
	if req.CustomHeader != nil {
		selectFields = append(selectFields, "custom_header")
		updates.CustomHeader = *req.CustomHeader
	}
	if req.ChannelProxy != nil {
		selectFields = append(selectFields, "channel_proxy")
		updates.ChannelProxy = req.ChannelProxy
	}
	if req.ParamOverride != nil {
		selectFields = append(selectFields, "param_override")
		updates.ParamOverride = req.ParamOverride
	}
	if req.MatchRegex != nil {
		selectFields = append(selectFields, "match_regex")
		updates.MatchRegex = req.MatchRegex
	}
	if req.RateLimit != nil {
		selectFields = append(selectFields, "rate_limit")
		updates.RateLimit = *req.RateLimit
	}
	if req.ModelRateLimit != nil {
		selectFields = append(selectFields, "model_rate_limit")
		updates.ModelRateLimit = *req.ModelRateLimit
	}
	if req.KeyMode != nil {
		selectFields = append(selectFields, "key_mode")
		updates.KeyMode = *req.KeyMode
	}
	if req.CircuitBreakerThreshold != nil {
		selectFields = append(selectFields, "circuit_breaker_threshold")
		updates.CircuitBreakerThreshold = req.CircuitBreakerThreshold
	}
	if req.CircuitBreakerCooldown != nil {
		selectFields = append(selectFields, "circuit_breaker_cooldown")
		updates.CircuitBreakerCooldown = req.CircuitBreakerCooldown
	}
	if req.CircuitBreakerMaxCooldown != nil {
		selectFields = append(selectFields, "circuit_breaker_max_cooldown")
		updates.CircuitBreakerMaxCooldown = req.CircuitBreakerMaxCooldown
	}

	// 只有当有字段需要更新时才执行 UPDATE
	if len(selectFields) > 0 {
		if err := tx.Model(&model.Channel{}).Where("id = ?", req.ID).Select(selectFields).Updates(&updates).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update channel: %w", err)
		}
	}

	// 删除 keys
	if len(req.KeysToDelete) > 0 {
		if err := tx.Where("id IN ? AND channel_id = ?", req.KeysToDelete, req.ID).Delete(&model.ChannelKey{}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to delete channel keys: %w", err)
		}
	}

	// 更新 keys（逐条，只更新提供的字段）
	if len(req.KeysToUpdate) > 0 {
		for _, ku := range req.KeysToUpdate {
			updates := map[string]interface{}{}
			if ku.Enabled != nil {
				updates["enabled"] = *ku.Enabled
			}
			if ku.ChannelKey != nil {
				updates["channel_key"] = *ku.ChannelKey
			}
			if ku.Remark != nil {
				updates["remark"] = *ku.Remark
			}
			if ku.KeyProxy != nil {
				updates["key_proxy"] = *ku.KeyProxy
			}
			if len(updates) == 0 {
				continue
			}
			if err := tx.Model(&model.ChannelKey{}).
				Where("id = ? AND channel_id = ?", ku.ID, req.ID).
				Updates(updates).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("failed to update channel key %d: %w", ku.ID, err)
			}
		}
	}

	// 新增 keys
	if len(req.KeysToAdd) > 0 {
		newKeys := make([]model.ChannelKey, 0, len(req.KeysToAdd))
		for _, ka := range req.KeysToAdd {
			newKeys = append(newKeys, model.ChannelKey{
				ChannelID:  req.ID,
				Enabled:    ka.Enabled,
				ChannelKey: ka.ChannelKey,
				Remark:     ka.Remark,
				KeyProxy:   ka.KeyProxy,
			})
		}
		if err := tx.Create(&newKeys).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to create channel keys: %w", err)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 刷新缓存并返回最新数据
	if err := channelRefreshCacheByID(req.ID, ctx); err != nil {
		return nil, err
	}

	channel, _ := channelCache.Get(req.ID)
	return &channel, nil
}

func ChannelEnabled(id int, enabled bool, ctx context.Context) error {
	oldChannel, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.Channel{}).Where("id = ?", id).Update("enabled", enabled).Error; err != nil {
		return err
	}
	oldChannel.Enabled = enabled
	channelCache.Set(id, oldChannel)
	return nil
}

func ChannelDel(id int, ctx context.Context) error {
	ch, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}

	// 开启事务
	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 获取所有受影响的 GroupID，用于刷新缓存
	var affectedGroupIDs []int
	if err := tx.Model(&model.GroupItem{}).
		Where("channel_id = ?", id).
		Pluck("group_id", &affectedGroupIDs).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to get affected groups: %w", err)
	}

	// 删除所有引用该渠道的 GroupItem
	if err := tx.Where("channel_id = ?", id).Delete(&model.GroupItem{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete group items: %w", err)
	}

	// 删除渠道 keys
	if err := tx.Where("channel_id = ?", id).Delete(&model.ChannelKey{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel keys: %w", err)
	}

	// 删除统计数据
	if err := tx.Where("channel_id = ?", id).Delete(&model.StatsChannel{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel stats: %w", err)
	}

	// 删除渠道
	if err := tx.Delete(&model.Channel{}, id).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 删除缓存
	channelCache.Del(id)
	for _, k := range ch.Keys {
		if k.ID != 0 {
			channelKeyCache.Del(k.ID)
		}
	}
	StatsChannelDel(id)
	model.CleanupKeyRRIndex(id)
	// 刷新受影响的分组缓存
	for _, groupID := range affectedGroupIDs {
		if err := groupRefreshCacheByID(groupID, ctx); err != nil {
			log.Warnf("failed to refresh group cache for group %d: %v", groupID, err)
		}
	}

	return nil
}

func ChannelLLMList() []model.LLMChannel {
	models := []model.LLMChannel{}
	for _, channel := range channelCache.GetAll() {
		modelNames := xstrings.SplitTrimCompact(",", channel.Model, channel.CustomModel)
		for _, modelName := range modelNames {
			if modelName == "" {
				continue
			}
			models = append(models, model.LLMChannel{
				Name:        modelName,
				Enabled:     channel.Enabled,
				ChannelID:   channel.ID,
				ChannelName: channel.Name,
			})
		}
	}
	return models
}

func ChannelGet(id int, ctx context.Context) (*model.Channel, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}
	return &channel, nil
}

// ChannelGetByID 从缓存中获取 channel（不查 DB），用于性能敏感路径。
func ChannelGetByID(id int) (*model.Channel, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}
	return &channel, nil
}

func channelRefreshCache(ctx context.Context) error {
	channels := []model.Channel{}
	if err := db.GetDB().WithContext(ctx).
		Preload("Keys").
		Preload("Stats").
		Find(&channels).Error; err != nil {
		log.Warnf("failed to get channels: %v", err)
		return err
	}

	channelKeyRuntimeLock.Lock()
	defer channelKeyRuntimeLock.Unlock()
	channelKeyCache.Clear()
	channelKeyCacheNeedUpdateLock.Lock()
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()
	for _, channel := range channels {
		channelCache.Set(channel.ID, channel)
		for _, k := range channel.Keys {
			if k.ID != 0 {
				channelKeyCache.Set(k.ID, k)
			}
		}
	}
	return nil
}

func channelKeyIsDirty(id int) bool {
	channelKeyCacheNeedUpdateLock.Lock()
	defer channelKeyCacheNeedUpdateLock.Unlock()
	_, ok := channelKeyCacheNeedUpdate[id]
	return ok
}

func mergeChannelKeyRuntime(live, refreshed model.ChannelKey) model.ChannelKey {
	refreshed.TotalCost = live.TotalCost
	refreshed.StatusCode = live.StatusCode
	refreshed.LastUseTimeStamp = live.LastUseTimeStamp
	refreshed.RetryAfter = live.RetryAfter
	refreshed.ConsecutiveAuthErrors = live.ConsecutiveAuthErrors
	refreshed.LastAuthErrorTime = live.LastAuthErrorTime
	return refreshed
}

func channelRefreshCacheByID(id int, ctx context.Context) error {
	var refreshed model.Channel
	if err := db.GetDB().WithContext(ctx).
		Preload("Keys").
		Preload("Stats").
		First(&refreshed, id).Error; err != nil {
		return err
	}

	channelKeyRuntimeLock.Lock()
	defer channelKeyRuntimeLock.Unlock()
	if old, ok := channelCache.Get(id); ok {
		liveByID := make(map[int]model.ChannelKey, len(old.Keys))
		for _, key := range old.Keys {
			liveByID[key.ID] = key
			if key.ID != 0 {
				channelKeyCache.Del(key.ID)
			}
		}
		for i := range refreshed.Keys {
			if live, ok := liveByID[refreshed.Keys[i].ID]; ok && channelKeyIsDirty(live.ID) {
				refreshed.Keys[i] = mergeChannelKeyRuntime(live, refreshed.Keys[i])
			}
		}
	}
	channelCache.Set(refreshed.ID, refreshed)
	for _, key := range refreshed.Keys {
		if key.ID != 0 {
			channelKeyCache.Set(key.ID, key)
		}
	}
	return nil
}

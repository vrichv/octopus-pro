package model

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/llm"
)

type AutoGroupType int

const (
	AutoGroupTypeNone  AutoGroupType = 0 //不自动分组
	AutoGroupTypeFuzzy AutoGroupType = 1 //模糊匹配
	AutoGroupTypeExact AutoGroupType = 2 //准确匹配
	AutoGroupTypeRegex AutoGroupType = 3 //正则匹配
)

// keyRRIndex 用于 RoundRobin 模式下 key 选择的全局轮询索引，用 sync.Map 保证并发安全
var keyRRIndex sync.Map // channelID (int) → *atomic.Int64

const ChannelTypeDoubao llm.APIFormat = "doubao"
const ChannelTypeDeepSeek llm.APIFormat = "deepseek/chat_completions"
const ChannelTypeOpenRouter llm.APIFormat = "openrouter/chat_completions"
const ChannelTypeBailian llm.APIFormat = "bailian/chat_completions"
const ChannelTypeXAI llm.APIFormat = "xai"

type Channel struct {
	ID                        int            `json:"id" gorm:"primaryKey"`
	Name                      string         `json:"name" gorm:"unique;not null"`
	Type                      llm.APIFormat  `json:"type"`
	Enabled                   bool           `json:"enabled" gorm:"default:true"`
	BaseUrls                  []BaseUrl      `json:"base_urls" gorm:"serializer:json"`
	Keys                      []ChannelKey   `json:"keys" gorm:"foreignKey:ChannelID"`
	Model                     string         `json:"model"`
	CustomModel               string         `json:"custom_model"`
	ExcludedModel             string         `json:"excluded_model" gorm:"default:''"`
	Proxy                     bool           `json:"proxy" gorm:"default:false"`
	AutoSync                  bool           `json:"auto_sync" gorm:"default:false"`
	AutoGroup                 AutoGroupType  `json:"auto_group" gorm:"default:0"`
	CustomHeader              []CustomHeader `json:"custom_header" gorm:"serializer:json"`
	ParamOverride             *string        `json:"param_override"`
	ChannelProxy              *string        `json:"channel_proxy"`
	Stats                     *StatsChannel  `json:"stats,omitempty" gorm:"foreignKey:ChannelID"`
	MatchRegex                *string        `json:"match_regex"`
	RateLimit                 string         `json:"rate_limit" gorm:"default:''"`       // key 级默认限流，如 "100/1h"
	ModelRateLimit            string         `json:"model_rate_limit" gorm:"default:''"` // model 级限流，如 "gpt-4=2/1m,claude-3=10/1h"
	KeyMode                   int            `json:"key_mode" gorm:"default:0"`          // 0=Cost, 1=RoundRobin
	CircuitBreakerThreshold   *int           `json:"circuit_breaker_threshold"`          // nil=use global
	CircuitBreakerCooldown    *int           `json:"circuit_breaker_cooldown"`           // nil=use global (seconds)
	CircuitBreakerMaxCooldown *int           `json:"circuit_breaker_max_cooldown"`       // nil=use global (seconds)
}

type BaseUrl struct {
	URL   string `json:"url"`
	Delay int    `json:"delay"`
}

type CustomHeader struct {
	HeaderKey   string `json:"header_key"`
	HeaderValue string `json:"header_value"`
}
type ChannelKey struct {
	ID                    int     `json:"id" gorm:"primaryKey"`
	ChannelID             int     `json:"channel_id"`
	Enabled               bool    `json:"enabled" gorm:"default:true"`
	ChannelKey            string  `json:"channel_key"`
	StatusCode            int     `json:"status_code"`
	LastUseTimeStamp      int64   `json:"last_use_time_stamp"`
	TotalCost             float64 `json:"total_cost"`
	Remark                string  `json:"remark"`
	KeyProxy              string  `json:"key_proxy" gorm:"default:''"`
	ConsecutiveAuthErrors int     `json:"consecutive_auth_errors" gorm:"default:0"`
	LastAuthErrorTime     int64   `json:"last_auth_error_time" gorm:"default:0"` // 上次认证错误时间，用于 5min 窗口重置
	RetryAfter            int64   `json:"retry_after" gorm:"-"`                  // 动态冷却时间（秒），不持久化
}

// ChannelUpdateRequest 渠道更新请求 - 仅包含变更的数据
type ChannelUpdateRequest struct {
	ID                        int             `json:"id" binding:"required"`
	Name                      *string         `json:"name,omitempty"`
	Type                      *llm.APIFormat  `json:"type,omitempty"`
	Enabled                   *bool           `json:"enabled,omitempty"`
	BaseUrls                  *[]BaseUrl      `json:"base_urls,omitempty"`
	Model                     *string         `json:"model,omitempty"`
	CustomModel               *string         `json:"custom_model,omitempty"`
	ExcludedModel             *string         `json:"excluded_model,omitempty"`
	Proxy                     *bool           `json:"proxy,omitempty"`
	AutoSync                  *bool           `json:"auto_sync,omitempty"`
	AutoGroup                 *AutoGroupType  `json:"auto_group,omitempty"`
	CustomHeader              *[]CustomHeader `json:"custom_header,omitempty"`
	ChannelProxy              *string         `json:"channel_proxy,omitempty"`
	ParamOverride             *string         `json:"param_override,omitempty"`
	MatchRegex                *string         `json:"match_regex,omitempty"`
	RateLimit                 *string         `json:"rate_limit,omitempty"`
	ModelRateLimit            *string         `json:"model_rate_limit,omitempty"`
	KeyMode                   *int            `json:"key_mode,omitempty"`
	CircuitBreakerThreshold   *int            `json:"circuit_breaker_threshold,omitempty"`
	CircuitBreakerCooldown    *int            `json:"circuit_breaker_cooldown,omitempty"`
	CircuitBreakerMaxCooldown *int            `json:"circuit_breaker_max_cooldown,omitempty"`

	KeysToAdd    []ChannelKeyAddRequest    `json:"keys_to_add,omitempty"`
	KeysToUpdate []ChannelKeyUpdateRequest `json:"keys_to_update,omitempty"`
	KeysToDelete []int                     `json:"keys_to_delete,omitempty"`
}

type ChannelKeyAddRequest struct {
	Enabled    bool   `json:"enabled"`
	ChannelKey string `json:"channel_key" binding:"required"`
	Remark     string `json:"remark"`
	KeyProxy   string `json:"key_proxy"`
}

type ChannelKeyUpdateRequest struct {
	ID         int     `json:"id" binding:"required"`
	Enabled    *bool   `json:"enabled,omitempty"`
	ChannelKey *string `json:"channel_key,omitempty"`
	Remark     *string `json:"remark,omitempty"`
	KeyProxy   *string `json:"key_proxy,omitempty"`
}

func (c *Channel) GetBaseUrl() string {
	if c == nil || len(c.BaseUrls) == 0 {
		return ""
	}

	bestURL := ""
	bestDelay := 0
	bestSet := false

	for _, bu := range c.BaseUrls {
		if bu.URL == "" {
			continue
		}
		if !bestSet || bu.Delay < bestDelay {
			bestURL = bu.URL
			bestDelay = bu.Delay
			bestSet = true
		}
	}

	return bestURL
}

func (c *Channel) GetChannelKey(modelName string) ChannelKey {
	keys := c.GetChannelKeys(modelName)
	if len(keys) == 0 {
		return ChannelKey{}
	}
	return keys[0]
}

// GetChannelKeys returns all currently available keys in selection order.
// KeyMode=1 keeps round-robin fairness by rotating the first candidate; the
// remaining keys preserve the same relative order for relay to continue trying.
func (c *Channel) GetChannelKeys(modelName string) []ChannelKey {
	if c == nil || len(c.Keys) == 0 {
		return nil
	}

	nowSec := time.Now().Unix()
	available := make([]ChannelKey, 0, len(c.Keys))
	for _, k := range c.Keys {
		if k.ChannelKey == "" {
			continue
		}
		if !k.Enabled {
			continue
		}
		if modelName != "" {
			if isKeyModelCooling(c.ID, k.ID, modelName) {
				continue
			}
		} else if k.StatusCode == 429 && k.LastUseTimeStamp > 0 {
			cooldown := int64(2*time.Minute/time.Second) + int64(k.ID%60)
			if k.RetryAfter > 0 {
				cooldown = k.RetryAfter
			}
			if nowSec-k.LastUseTimeStamp < cooldown {
				continue
			}
		}
		available = append(available, k)
	}
	if len(available) == 0 {
		return nil
	}

	if c.KeyMode == 1 {
		val, _ := keyRRIndex.LoadOrStore(c.ID, new(atomic.Int64))
		idxPtr := val.(*atomic.Int64)
		idx := int(idxPtr.Add(1)-1) % len(available)
		ordered := make([]ChannelKey, 0, len(available))
		ordered = append(ordered, available[idx:]...)
		ordered = append(ordered, available[:idx]...)
		return ordered
	}

	bestIdx := 0
	bestCost := available[0].TotalCost
	for i := 1; i < len(available); i++ {
		if available[i].TotalCost < bestCost {
			bestIdx = i
			bestCost = available[i].TotalCost
		}
	}
	if bestIdx == 0 {
		return available
	}
	ordered := make([]ChannelKey, 0, len(available))
	ordered = append(ordered, available[bestIdx])
	ordered = append(ordered, available[:bestIdx]...)
	ordered = append(ordered, available[bestIdx+1:]...)
	return ordered
}

// CleanupKeyRRIndex 清理 channel 的 RR 轮询索引（channel 删除时调用）。
func CleanupKeyRRIndex(channelID int) {
	keyRRIndex.Delete(channelID)
}

// keyModelCooldownEntry 记录 (channel, key, model) 的冷却状态。
// 429 使用指数退避；慢流/截断流使用临时冷却但不增加 consecutive429s。
type keyModelCooldownEntry struct {
	cooldownUntil   time.Time // zero = 当前未冷却（但历史计数可能仍保留）
	lastPenaltyAt   time.Time // 最近一次 429 / 临时冷却的写入时间，用于计数器衰减清理
	consecutive429s int       // 连续 429 次数，成功时重置
	rateLimited     bool      // 当前冷却由上游或本地限流触发
	mu              sync.Mutex
}

const keyModelCooldownRetention = 30 * time.Minute

// globalKeyModelCooldown 全局限流冷却映射表。
// key 格式: "ch:{channelID}:k:{keyID}:m:{modelName}"
var globalKeyModelCooldown sync.Map

// keyModelCooldownOnce 用于 lazy-init 后台清理 goroutine。
var keyModelCooldownOnce sync.Once

// modelCooldownKey 生成冷却映射表的键。
func modelCooldownKey(channelID, keyID int, modelName string) string {
	return fmt.Sprintf("ch:%d:k:%d:m:%s", channelID, keyID, modelName)
}

// getOrCreateCooldownEntry 获取或创建冷却条目。
func getOrCreateCooldownEntry(key string) *keyModelCooldownEntry {
	if v, ok := globalKeyModelCooldown.Load(key); ok {
		return v.(*keyModelCooldownEntry)
	}
	entry := &keyModelCooldownEntry{}
	actual, _ := globalKeyModelCooldown.LoadOrStore(key, entry)
	return actual.(*keyModelCooldownEntry)
}

// ensureModelCooldownCleanup 启动后台 goroutine 定期清理过期条目。
// 冷却结束后保留历史 30 分钟；若期间未再次触发 429 / 临时惩罚，则清除计数器。
func ensureModelCooldownCleanup() {
	keyModelCooldownOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				now := time.Now()
				globalKeyModelCooldown.Range(func(key, value any) bool {
					entry := value.(*keyModelCooldownEntry)
					entry.mu.Lock()
					remove := entry.lastPenaltyAt.IsZero() || now.After(entry.lastPenaltyAt.Add(keyModelCooldownRetention))
					entry.mu.Unlock()
					if remove {
						globalKeyModelCooldown.Delete(key)
					}
					return true
				})
			}
		}()
	})
}

// RecordKeyModelCooldown 记录指定 (key, model) 的 429 冷却。
// 使用指数退避：连续 429 次数越多，冷却时间越长（上限 30 分钟）。
// retryAfter 为 0 或负值时使用默认冷却（2 分钟 + keyID%60 抖动）。
func RecordKeyModelCooldown(channelID, keyID int, modelName string, retryAfter time.Duration) {
	ensureModelCooldownCleanup()
	key := modelCooldownKey(channelID, keyID, modelName)
	entry := getOrCreateCooldownEntry(key)
	now := time.Now()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if !entry.lastPenaltyAt.IsZero() && now.After(entry.lastPenaltyAt.Add(keyModelCooldownRetention)) {
		entry.consecutive429s = 0
		entry.rateLimited = false
	}
	entry.consecutive429s++
	entry.rateLimited = true
	entry.lastPenaltyAt = now

	base := retryAfter
	if base <= 0 {
		base = 2*time.Minute + time.Duration(keyID%60)*time.Second
	}

	cooldown := base
	shifts := entry.consecutive429s - 1
	if shifts > 0 {
		if shifts > 20 {
			shifts = 20
		}
		cooldown = base << shifts
	}
	if cooldown > keyModelCooldownRetention {
		cooldown = keyModelCooldownRetention
	}
	entry.cooldownUntil = now.Add(cooldown)
}

// RecordKeyModelTemporaryCooldown applies a one-off non-rate-limit cooldown.
// It is used for slow or truncated streams and does not increase the 429 penalty.
func RecordKeyModelTemporaryCooldown(channelID, keyID int, modelName string, cooldown time.Duration) {
	recordKeyModelTemporaryCooldown(channelID, keyID, modelName, cooldown, false)
}

// RecordKeyModelRateLimitCooldown applies a local rate-limit cooldown without
// increasing the upstream 429 backoff counter.
func RecordKeyModelRateLimitCooldown(channelID, keyID int, modelName string, cooldown time.Duration) {
	recordKeyModelTemporaryCooldown(channelID, keyID, modelName, cooldown, true)
}

func recordKeyModelTemporaryCooldown(channelID, keyID int, modelName string, cooldown time.Duration, rateLimited bool) {
	if cooldown <= 0 {
		return
	}
	ensureModelCooldownCleanup()
	key := modelCooldownKey(channelID, keyID, modelName)
	entry := getOrCreateCooldownEntry(key)
	now := time.Now()
	until := now.Add(cooldown)

	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.lastPenaltyAt = now
	entry.rateLimited = rateLimited
	if until.After(entry.cooldownUntil) {
		entry.cooldownUntil = until
	}
}

// ClearKeyModelCooldown 清除指定 (key, model) 的冷却记录（含退避计数器）。
// 导出给 relay 包在请求成功时调用。
func ClearKeyModelCooldown(channelID, keyID int, modelName string) {
	key := modelCooldownKey(channelID, keyID, modelName)
	globalKeyModelCooldown.Delete(key)
}

// GetKeyModelCooldownRemaining returns the remaining cooldown for a key-model
// pair. Expired entries retain their 429 history but no longer block routing.
func GetKeyModelCooldownRemaining(channelID, keyID int, modelName string) time.Duration {
	key := modelCooldownKey(channelID, keyID, modelName)
	v, ok := globalKeyModelCooldown.Load(key)
	if !ok {
		return 0
	}
	entry := v.(*keyModelCooldownEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.cooldownUntil.IsZero() {
		return 0
	}
	remaining := time.Until(entry.cooldownUntil)
	if remaining <= 0 {
		entry.cooldownUntil = time.Time{}
		return 0
	}
	return remaining
}

// isKeyModelCooling checks whether the specified (key, model) pair is cooling.
func isKeyModelCooling(channelID, keyID int, modelName string) bool {
	return GetKeyModelCooldownRemaining(channelID, keyID, modelName) > 0
}

// CooldownStatus 用于 API 查询的冷却状态快照。
type CooldownStatus struct {
	Active          bool   `json:"active"`
	Consecutive429s int    `json:"consecutive_429s"`
	CooldownUntil   string `json:"cooldown_until"`
	RateLimited     bool   `json:"-"`
}

// GetKeyModelCooldownStatus 返回指定 (key, model) 的冷却当前状态。
func GetKeyModelCooldownStatus(channelID, keyID int, modelName string) CooldownStatus {
	key := modelCooldownKey(channelID, keyID, modelName)
	v, ok := globalKeyModelCooldown.Load(key)
	if !ok {
		return CooldownStatus{}
	}
	entry := v.(*keyModelCooldownEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	status := CooldownStatus{
		Consecutive429s: entry.consecutive429s,
		RateLimited:     entry.rateLimited,
	}
	if !entry.cooldownUntil.IsZero() && time.Now().Before(entry.cooldownUntil) {
		status.Active = true
		status.CooldownUntil = entry.cooldownUntil.Format(time.RFC3339)
	}
	return status
}

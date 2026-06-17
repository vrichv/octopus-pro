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

type Channel struct {
	ID             int            `json:"id" gorm:"primaryKey"`
	Name           string         `json:"name" gorm:"unique;not null"`
	Type           llm.APIFormat  `json:"type"`
	Enabled        bool           `json:"enabled" gorm:"default:true"`
	BaseUrls       []BaseUrl      `json:"base_urls" gorm:"serializer:json"`
	Keys           []ChannelKey   `json:"keys" gorm:"foreignKey:ChannelID"`
	Model          string         `json:"model"`
	CustomModel    string         `json:"custom_model"`
	ExcludedModel  string         `json:"excluded_model" gorm:"default:''"`
	Proxy          bool           `json:"proxy" gorm:"default:false"`
	AutoSync       bool           `json:"auto_sync" gorm:"default:false"`
	AutoGroup      AutoGroupType  `json:"auto_group" gorm:"default:0"`
	CustomHeader   []CustomHeader `json:"custom_header" gorm:"serializer:json"`
	ParamOverride  *string        `json:"param_override"`
	ChannelProxy   *string        `json:"channel_proxy"`
	Stats          *StatsChannel  `json:"stats,omitempty" gorm:"foreignKey:ChannelID"`
	MatchRegex     *string        `json:"match_regex"`
	RateLimit      string         `json:"rate_limit" gorm:"default:''"`       // key 级默认限流，如 "100/1h"
	ModelRateLimit string         `json:"model_rate_limit" gorm:"default:''"` // model 级限流，如 "gpt-4=2/1m,claude-3=10/1h"
	KeyMode        int            `json:"key_mode" gorm:"default:0"`          // 0=Cost, 1=RoundRobin
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
	ID             int             `json:"id" binding:"required"`
	Name           *string         `json:"name,omitempty"`
	Type           *llm.APIFormat  `json:"type,omitempty"`
	Enabled        *bool           `json:"enabled,omitempty"`
	BaseUrls       *[]BaseUrl      `json:"base_urls,omitempty"`
	Model          *string         `json:"model,omitempty"`
	CustomModel    *string         `json:"custom_model,omitempty"`
	ExcludedModel  *string         `json:"excluded_model,omitempty"`
	Proxy          *bool           `json:"proxy,omitempty"`
	AutoSync       *bool           `json:"auto_sync,omitempty"`
	AutoGroup      *AutoGroupType  `json:"auto_group,omitempty"`
	CustomHeader   *[]CustomHeader `json:"custom_header,omitempty"`
	ChannelProxy   *string         `json:"channel_proxy,omitempty"`
	ParamOverride  *string         `json:"param_override,omitempty"`
	MatchRegex     *string         `json:"match_regex,omitempty"`
	RateLimit      *string         `json:"rate_limit,omitempty"`
	ModelRateLimit *string         `json:"model_rate_limit,omitempty"`
	KeyMode        *int            `json:"key_mode,omitempty"`

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
	if c == nil || len(c.Keys) == 0 {
		return ChannelKey{}
	}

	nowSec := time.Now().Unix()

	// 筛选可用的 key（Enabled + 未在冷却中）
	available := make([]ChannelKey, 0, len(c.Keys))
	for _, k := range c.Keys {
		if k.ChannelKey == "" {
			continue
		}

		// 认证错误 5min 窗口重置：上次认证错误超过 5min，重置计数
		if k.ConsecutiveAuthErrors > 0 && k.LastAuthErrorTime > 0 && nowSec-k.LastAuthErrorTime >= 300 {
			k.ConsecutiveAuthErrors = 0
			k.LastAuthErrorTime = 0
		}
		if !k.Enabled {
			continue
		}

		// 冷却判定：
		// - 有 modelName（relay 场景）：使用 per-key-per-model 冷却跟踪，各模型独立
		// - 无 modelName（fetch 场景）：回退 key 级 StatusCode 检查（向后兼容）
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
		return ChannelKey{}
	}

	// KeyMode=1: RoundRobin 轮询
	if c.KeyMode == 1 {
		val, _ := keyRRIndex.LoadOrStore(c.ID, new(atomic.Int64))
		idxPtr := val.(*atomic.Int64)
		idx := int(idxPtr.Add(1)-1) % len(available)
		return available[idx]
	}

	// KeyMode=0: Cost 成本优先
	best := available[0]
	bestCost := best.TotalCost
	for _, k := range available[1:] {
		if k.TotalCost < bestCost {
			best = k
			bestCost = k.TotalCost
		}
	}
	return best
}

// CleanupKeyRRIndex 清理 channel 的 RR 轮询索引（channel 删除时调用）。
func CleanupKeyRRIndex(channelID int) {
	keyRRIndex.Delete(channelID)
}

// ---------------------------------------------------------------------------
// Per-Key-Per-Model 429 Cooldown Tracker
//
// 每个 (channel, key, model) 组合独立跟踪 429 冷却状态，避免一个模型的 429 影响同 key
// 下的其他模型。key 级别的 StatusCode 字段仍保留用于 UI 展示，路由决策使用本跟踪器。
// ---------------------------------------------------------------------------

// keyModelCooldownEntry 记录 (channel, key, model) 的冷却截止时间。
type keyModelCooldownEntry struct {
	cooldownUntil time.Time // zero time 表示未冷却
	mu            sync.Mutex
}

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
					if entry.cooldownUntil.IsZero() || now.After(entry.cooldownUntil) {
						globalKeyModelCooldown.Delete(key)
					}
					entry.mu.Unlock()
					return true
				})
			}
		}()
	})
}

// RecordKeyModelCooldown 记录指定 (key, model) 的 429 冷却。
// retryAfter 为 0 或负值时使用默认冷却（2 分钟 + keyID%60 抖动）。
// 该函数导出给 relay 包在收到上游 429 时调用。
func RecordKeyModelCooldown(channelID, keyID int, modelName string, retryAfter time.Duration) {
	ensureModelCooldownCleanup()
	key := modelCooldownKey(channelID, keyID, modelName)
	entry := getOrCreateCooldownEntry(key)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	cooldown := retryAfter
	if cooldown <= 0 {
		// 默认冷却：2 分钟 + keyID % 60 秒抖动（防止所有 key 同时恢复）
		cooldown = 2*time.Minute + time.Duration(keyID%60)*time.Second
	}
	entry.cooldownUntil = time.Now().Add(cooldown)
}

// ClearKeyModelCooldown 清除指定 (key, model) 的冷却记录。
// 导出给 relay 包在请求成功时调用。
func ClearKeyModelCooldown(channelID, keyID int, modelName string) {
	key := modelCooldownKey(channelID, keyID, modelName)
	globalKeyModelCooldown.Delete(key)
}

// isKeyModelCooling 检查指定 (key, model) 是否处于冷却中。
// 如果冷却已过期，自动重置并返回 false。
func isKeyModelCooling(channelID, keyID int, modelName string) bool {
	key := modelCooldownKey(channelID, keyID, modelName)
	v, ok := globalKeyModelCooldown.Load(key)
	if !ok {
		return false
	}
	entry := v.(*keyModelCooldownEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.cooldownUntil.IsZero() {
		// 已被其他路径重置为零值，直接删除死条目避免泄漏
		globalKeyModelCooldown.Delete(key)
		return false
	}
	if time.Now().After(entry.cooldownUntil) {
		globalKeyModelCooldown.Delete(key) // 过期条目直接删除，而非仅重置
		return false
	}
	return true
}

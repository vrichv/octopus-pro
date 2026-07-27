package balancer

import (
	"fmt"
	"sync"
	"time"

	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/utils/log"
)

// CircuitState 熔断器状态
type CircuitState int

const (
	StateClosed   CircuitState = iota // 正常通行
	StateOpen                         // 熔断中，拒绝所有请求
	StateHalfOpen                     // 半开，仅允许单个试探请求
)

// circuitEntry 单个熔断器条目
type circuitEntry struct {
	State               CircuitState
	ConsecutiveFailures int64
	LastFailureTime     time.Time
	TripCount           int // 累计熔断触发次数（用于指数退避）
	mu                  sync.Mutex
}

// 全局熔断器存储
var globalBreaker sync.Map // key: string -> value: *circuitEntry

// circuitKey 生成熔断器键：channelID:channelKeyID:modelName
func circuitKey(channelID, keyID int, modelName string) string {
	return fmt.Sprintf("%d:%d:%s", channelID, keyID, modelName)
}

// getOrCreateEntry 获取或创建熔断器条目
func getOrCreateEntry(key string) *circuitEntry {
	if v, ok := globalBreaker.Load(key); ok {
		return v.(*circuitEntry)
	}
	entry := &circuitEntry{State: StateClosed}
	actual, _ := globalBreaker.LoadOrStore(key, entry)
	return actual.(*circuitEntry)
}

// getThreshold 获取熔断阈值配置（全局默认）
func getThreshold() int64 {
	v, err := op.SettingGetInt(model.SettingKeyCircuitBreakerThreshold)
	if err != nil || v <= 0 {
		return 5
	}
	return int64(v)
}

// getThresholdForChannel 获取熔断阈值，优先使用 channel 级覆盖，否则回退全局。
func getThresholdForChannel(channelID int) int64 {
	ch, err := op.ChannelGetByID(channelID)
	if err == nil && ch.CircuitBreakerThreshold != nil && *ch.CircuitBreakerThreshold > 0 {
		return int64(*ch.CircuitBreakerThreshold)
	}
	return getThreshold()
}

// getCooldown 获取当前冷却时间（带指数退避），使用全局默认值。
func getCooldown(tripCount int) time.Duration {
	base, err := op.SettingGetInt(model.SettingKeyCircuitBreakerCooldown)
	if err != nil || base <= 0 {
		base = 60
	}
	maxCooldown, err := op.SettingGetInt(model.SettingKeyCircuitBreakerMaxCooldown)
	if err != nil || maxCooldown <= 0 {
		maxCooldown = 600
	}
	return computeCooldown(tripCount, base, maxCooldown)
}

// GetCooldownForChannel 获取当前冷却时间，优先使用 channel 级覆盖，否则回退全局。
func GetCooldownForChannel(channelID, tripCount int) time.Duration {
	ch, err := op.ChannelGetByID(channelID)
	base := 0
	maxCooldown := 0
	if err == nil {
		if ch.CircuitBreakerCooldown != nil && *ch.CircuitBreakerCooldown > 0 {
			base = *ch.CircuitBreakerCooldown
		}
		if ch.CircuitBreakerMaxCooldown != nil && *ch.CircuitBreakerMaxCooldown > 0 {
			maxCooldown = *ch.CircuitBreakerMaxCooldown
		}
	}
	if base == 0 {
		v, _ := op.SettingGetInt(model.SettingKeyCircuitBreakerCooldown)
		base = v
	}
	if base <= 0 {
		base = 60
	}
	if maxCooldown == 0 {
		v, _ := op.SettingGetInt(model.SettingKeyCircuitBreakerMaxCooldown)
		maxCooldown = v
	}
	if maxCooldown <= 0 {
		maxCooldown = 600
	}
	return computeCooldown(tripCount, base, maxCooldown)
}

func computeCooldown(tripCount, base, maxCooldown int) time.Duration {
	cooldown := base
	if tripCount > 1 {
		shift := tripCount - 1
		if shift > 20 {
			shift = 20
		}
		cooldown = base << shift
	}
	if cooldown > maxCooldown {
		cooldown = maxCooldown
	}
	return time.Duration(cooldown) * time.Second
}

// IsTripped 检查通道是否处于熔断状态
// 返回 tripped=true 表示该通道应被跳过，remaining 为剩余冷却时间
func IsTripped(channelID, keyID int, modelName string) (tripped bool, remaining time.Duration) {
	key := circuitKey(channelID, keyID, modelName)
	v, ok := globalBreaker.Load(key)
	if !ok {
		return false, 0 // 无记录，视为 Closed
	}
	entry := v.(*circuitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	switch entry.State {
	case StateClosed:
		return false, 0

	case StateOpen:
		cooldown := GetCooldownForChannel(channelID, entry.TripCount)
		elapsed := time.Since(entry.LastFailureTime)
		if elapsed >= cooldown {
			entry.State = StateHalfOpen
			log.Infof("circuit breaker [%s] Open -> HalfOpen (cooldown %v elapsed)", key, cooldown)
			return false, 0
		}
		// 仍在冷却中
		return true, cooldown - elapsed

	case StateHalfOpen:
		// 已有试探请求在进行中，拒绝其他请求
		return true, 0

	default:
		return false, 0
	}
}

// RecordSuccess 记录成功，重置熔断器状态
func RecordSuccess(channelID, keyID int, modelName string) {
	key := circuitKey(channelID, keyID, modelName)
	v, ok := globalBreaker.Load(key)
	if !ok {
		return
	}
	entry := v.(*circuitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.State == StateHalfOpen {
		log.Infof("circuit breaker [%s] HalfOpen -> Closed (probe succeeded)", key)
	}

	// 重置全部状态
	entry.State = StateClosed
	entry.ConsecutiveFailures = 0
	entry.TripCount = 0
}

// RecordFailure 记录失败，可能触发熔断
func RecordFailure(channelID, keyID int, modelName string) {
	key := circuitKey(channelID, keyID, modelName)
	entry := getOrCreateEntry(key)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.LastFailureTime = time.Now()

	switch entry.State {
	case StateClosed:
		entry.ConsecutiveFailures++
		threshold := getThresholdForChannel(channelID)
		if entry.ConsecutiveFailures >= threshold {
			entry.State = StateOpen
			entry.TripCount++
			log.Warnf("circuit breaker [%s] Closed -> Open (failures=%d >= threshold=%d, tripCount=%d, cooldown=%v)",
				key, entry.ConsecutiveFailures, threshold, entry.TripCount, GetCooldownForChannel(channelID, entry.TripCount))
		}

	case StateHalfOpen:
		// 试探失败，重新进入 Open 状态，TripCount 递增（冷却时间翻倍）
		entry.State = StateOpen
		entry.TripCount++
		entry.ConsecutiveFailures = 0 // 重新开始计数
		log.Warnf("circuit breaker [%s] HalfOpen -> Open (probe failed, tripCount=%d, cooldown=%v)",
			key, entry.TripCount, GetCooldownForChannel(channelID, entry.TripCount))

	case StateOpen:
		// 理论上不应该在 Open 状态下接收到失败记录（请求应被拒绝），
		// 但为安全起见仍更新失败时间
	}
}

// CircuitBreakerStatus 用于 API 查询的熔断器状态快照。
type CircuitBreakerStatus struct {
	State               string `json:"state"`
	ConsecutiveFailures int64  `json:"consecutive_failures"`
	TripCount           int    `json:"trip_count"`
	CooldownRemaining   int    `json:"cooldown_remaining_sec"`
}

// GetCircuitBreakerStatus 返回指定 (channel, key, model) 的熔断器当前状态。
// 若无记录则返回 Closed 状态。
func GetCircuitBreakerStatus(channelID, keyID int, modelName string) CircuitBreakerStatus {
	key := circuitKey(channelID, keyID, modelName)
	v, ok := globalBreaker.Load(key)
	if !ok {
		return CircuitBreakerStatus{State: "closed"}
	}
	entry := v.(*circuitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	status := CircuitBreakerStatus{
		ConsecutiveFailures: entry.ConsecutiveFailures,
		TripCount:           entry.TripCount,
	}

	switch entry.State {
	case StateClosed:
		status.State = "closed"
	case StateOpen:
		status.State = "open"
		cooldown := GetCooldownForChannel(channelID, entry.TripCount)
		elapsed := time.Since(entry.LastFailureTime)
		if remaining := cooldown - elapsed; remaining > 0 {
			status.CooldownRemaining = int(remaining.Seconds())
		}
	case StateHalfOpen:
		status.State = "half_open"
	}

	return status
}

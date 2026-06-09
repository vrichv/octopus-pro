package plugins

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/vrichv/octopus-pro/internal/model"
)

// ---------------------------------------------------------------------------
// 限流规格解析
// ---------------------------------------------------------------------------

// ParseRateSpec 解析限流规格 "count/timeUnit"，如 "100/1h"。
// 支持的时间单位：s（秒）, m（分钟）, h（小时）, d（天）。
func ParseRateSpec(spec string) (count int, interval time.Duration, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, fmt.Errorf("empty rate spec")
	}

	parts := strings.SplitN(spec, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid rate spec format: %q (expected count/timeUnit)", spec)
	}

	count, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || count <= 0 {
		return 0, 0, fmt.Errorf("invalid rate count: %q", parts[0])
	}

	unit := strings.TrimSpace(parts[1])
	var unitCount int
	switch {
	case strings.HasSuffix(unit, "s"):
		unitCount, err = parseNum(unit, "s")
		interval = time.Duration(unitCount) * time.Second
	case strings.HasSuffix(unit, "m"):
		unitCount, err = parseNum(unit, "m")
		interval = time.Duration(unitCount) * time.Minute
	case strings.HasSuffix(unit, "h"):
		unitCount, err = parseNum(unit, "h")
		interval = time.Duration(unitCount) * time.Hour
	case strings.HasSuffix(unit, "d"):
		unitCount, err = parseNum(unit, "d")
		interval = time.Duration(unitCount) * 24 * time.Hour
	default:
		return 0, 0, fmt.Errorf("unknown time unit: %q (expected s/m/h/d)", unit)
	}

	if err != nil || interval <= 0 {
		return 0, 0, fmt.Errorf("invalid interval: %q", parts[1])
	}

	return count, interval, nil
}

func parseNum(s, suffix string) (int, error) {
	s = strings.TrimSuffix(s, suffix)
	if s == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid interval value: %q", s)
	}
	return n, nil
}

// ParseModelRateLimit 解析 model 级限流配置。
// 格式：逗号分隔的 "model=spec" 对，以第一个等号为分隔符。
// 示例："gpt-4=2/1m,claude-3:free=10/1h,my:model=5/1d"
func ParseModelRateLimit(spec string) map[string]string {
	result := make(map[string]string)
	if spec == "" {
		return result
	}
	entries := strings.Split(spec, ",")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		eqIdx := strings.Index(entry, "=")
		if eqIdx < 0 {
			continue
		}
		modelName := strings.TrimSpace(entry[:eqIdx])
		rateSpec := strings.TrimSpace(entry[eqIdx+1:])
		if modelName == "" || rateSpec == "" {
			continue
		}
		result[modelName] = rateSpec
	}
	return result
}

// ---------------------------------------------------------------------------
// 滑动窗口限流器
// ---------------------------------------------------------------------------

// rateLimiter 滑动窗口限流器
type rateLimiter struct {
	mu         sync.Mutex
	count      int           // 窗口内允许的最大请求数
	interval   time.Duration // 窗口时长
	window     []time.Time   // 窗口内请求时间戳
	lastAccess atomic.Int64  // 最后访问时间（UnixNano）
}

func newRateLimiter(count int, interval time.Duration) *rateLimiter {
	rl := &rateLimiter{
		count:    count,
		interval: interval,
		window:   make([]time.Time, 0, count),
	}
	rl.lastAccess.Store(time.Now().UnixNano())
	return rl
}

// reconfigure 更新限流参数，若参数不一致则重置窗口。
func (rl *rateLimiter) reconfigure(count int, interval time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.count != count || rl.interval != interval {
		rl.count = count
		rl.interval = interval
		rl.window = make([]time.Time, 0, count)
	}
}

// cleanWindow 移除窗口外的过期条目。必须在持有 mu 时调用。
func (rl *rateLimiter) cleanWindow() {
	if len(rl.window) == 0 {
		return
	}
	cutoff := time.Now().Add(-rl.interval)
	i := 0
	for i < len(rl.window) && rl.window[i].Before(cutoff) {
		i++
	}
	rl.window = rl.window[i:]
}

// Allow 检查是否允许本次请求通过。若未超过限制返回 true，否则返回 false。
func (rl *rateLimiter) Allow() bool {
	rl.lastAccess.Store(time.Now().UnixNano())
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cleanWindow()
	if len(rl.window) >= rl.count {
		return false
	}
	rl.window = append(rl.window, time.Now())
	return true
}

// WaitDuration 计算到下一个可用槽位的等待时间。内部清理过期条目。
func (rl *rateLimiter) WaitDuration() time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cleanWindow()
	if len(rl.window) < rl.count {
		return 0
	}
	return rl.window[0].Add(rl.interval).Sub(time.Now())
}

// TryAllow 原子检查限流：允许通过返回 nil，否则返回 RateLimitedError 含等待时间。
// 合并 Allow + WaitDuration 在一次锁获取中，避免并发下等待时间陈旧。
func (rl *rateLimiter) TryAllow() *RateLimitedError {
	rl.lastAccess.Store(time.Now().UnixNano())
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cleanWindow()
	if len(rl.window) < rl.count {
		rl.window = append(rl.window, time.Now())
		return nil
	}
	wait := rl.window[0].Add(rl.interval).Sub(time.Now())
	return &RateLimitedError{Wait: wait}
}

// ---------------------------------------------------------------------------
// 全局限流器存储
// ---------------------------------------------------------------------------

var (
	limiters    sync.Map // key string → *rateLimiter
	limiterOnce sync.Once
)

func ensureCleanup() {
	limiterOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				now := time.Now().UnixNano()
				limiters.Range(func(key, value any) bool {
					rl := value.(*rateLimiter)
					if now-rl.lastAccess.Load() > int64(30*time.Minute) {
						limiters.Delete(key)
					}
					return true
				})
			}
		}()
	})
}

// getOrCreateLimiter 获取或创建限流器。双检查避免已存在时多余的对象分配。
func getOrCreateLimiter(key string, count int, interval time.Duration) *rateLimiter {
	ensureCleanup()
	if val, ok := limiters.Load(key); ok {
		rl := val.(*rateLimiter)
		rl.reconfigure(count, interval)
		return rl
	}
	rl := newRateLimiter(count, interval)
	val, _ := limiters.LoadOrStore(key, rl)
	loaded := val.(*rateLimiter)
	loaded.reconfigure(count, interval)
	return loaded
}

// ---------------------------------------------------------------------------
// ErrRateLimited 限流错误
// ---------------------------------------------------------------------------

var ErrRateLimited = errors.New("rate limited")

// RateLimitedError 区分限流和真实错误，携带等待时间。
type RateLimitedError struct {
	Key      string
	Limit    int
	Interval time.Duration
	Wait     time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited: %s limit exceeded (%d/%s), wait %v",
		e.Key, e.Limit, e.Interval, e.Wait)
}

func (e *RateLimitedError) Is(target error) bool {
	return target == ErrRateLimited
}

// ---------------------------------------------------------------------------
// RateLimiter Middleware
// ---------------------------------------------------------------------------

// NewRateLimiter 创建限流中间件。
// 在发出上游请求前检查 key 级和 model 级限流。
func NewRateLimiter(ch *model.Channel, keyID int, modelName string) pipeline.Middleware {
	// 预解析限流配置
	var keyLimitCount int
	var keyLimitInterval time.Duration
	var keyHasLimit bool

	if ch.RateLimit != "" {
		if c, dur, err := ParseRateSpec(ch.RateLimit); err == nil {
			keyLimitCount = c
			keyLimitInterval = dur
			keyHasLimit = true
		}
	}

	modelLimits := ParseModelRateLimit(ch.ModelRateLimit)
	modelSpec, modelHasLimit := modelLimits[modelName]

	var modelLimitCount int
	var modelLimitInterval time.Duration

	if modelHasLimit {
		if c, dur, err := ParseRateSpec(modelSpec); err == nil {
			modelLimitCount = c
			modelLimitInterval = dur
		} else {
			modelHasLimit = false
		}
	}

	// 确定限流 key 和参数
	limitKey := ""
	limitCount := 0
	limitInterval := time.Duration(0)

	if modelHasLimit {
		// model 级限流：key = "ch:{channelID}:m:{modelName}"
		limitKey = fmt.Sprintf("ch:%d:m:%s", ch.ID, modelName)
		limitCount = modelLimitCount
		limitInterval = modelLimitInterval
	} else if keyHasLimit {
		// key 级默认限流：key = "ch:{channelID}:k:{keyID}"
		limitKey = fmt.Sprintf("ch:%d:k:%d", ch.ID, keyID)
		limitCount = keyLimitCount
		limitInterval = keyLimitInterval
	}

	return &rateLimiterMiddleware{
		limitKey:      limitKey,
		limitCount:    limitCount,
		limitInterval: limitInterval,
	}
}

type rateLimiterMiddleware struct {
	pipeline.DummyMiddleware
	limitKey      string
	limitCount    int
	limitInterval time.Duration
}

func (m *rateLimiterMiddleware) Name() string {
	return "rate_limiter"
}

func (m *rateLimiterMiddleware) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if m.limitKey == "" {
		return request, nil
	}
	rl := getOrCreateLimiter(m.limitKey, m.limitCount, m.limitInterval)
	if rlErr := rl.TryAllow(); rlErr != nil {
		rlErr.Key = m.limitKey
		rlErr.Limit = m.limitCount
		rlErr.Interval = m.limitInterval
		return nil, rlErr
	}
	return request, nil
}

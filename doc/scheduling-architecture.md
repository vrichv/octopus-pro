# Octopus 调度架构

> 请求从网关进入到上游响应的完整调度链路：分组匹配 → 渠道迭代 → Key 选择 → 故障处理 → 熔断/限流/冷却 → 重试

---

## 1. 请求入口

```
Gin Handler (internal/relay/relay.go:32)
  │
  ├─ parseRequest         — 解析请求体、验证 API 格式
  ├─ modelAllowedByAPIKey — API Key 模型白名单检查
  ├─ op.GroupGetEnabledMap— 根据请求模型名查找匹配的 Group
  ├─ balancer.NewIterator — 构建渠道迭代器（排序 + 粘性）
  └─ relayRun.run()      — 主调度循环
```

### 1.1 Group 匹配

`op.GroupGetEnabledMap(modelName)` 遍历所有启用的 Group，匹配规则：

- Group 的 `Items` 中至少有一个 `GroupItem.ModelName` 匹配请求的模型名
- 若 `Group.MatchRegex` 非空，模型名还需匹配该正则

匹配到多个 Group 时，**取第一个 `ID` 最小的**。匹配不到则返回 404。

### 1.2 模型名匹配逻辑

`GroupItem.ModelName` 支持三种形式：
- `"*"` — 通配：匹配所有模型
- `"gpt-4"` — 精确匹配
- `"gpt-4*"` — 前缀匹配

---

## 2. 渠道迭代器 (`balancer.Iterator`)

文件：`internal/relay/balancer/iterator.go`

```go
type Iterator struct {
    candidates []model.GroupItem  // 已排序的候选列表
    index      int                // 当前位置（-1 = 未开始）
    stickyIdx  int                // 粘性通道在 candidates 中的索引（-1 = 无）
    modelName  string
    attempts   []model.ChannelAttempt  // 所有尝试记录
}
```

### 2.1 候选排序

`NewIterator` 调用 `Balancer.Candidates(items)` 排序，策略由 `Group.Mode` 决定：

| Mode | 常量 | 行为 |
|------|------|------|
| RoundRobin (1) | `GroupModeRoundRobin` | 全局原子计数器轮转，从上次位置开始排列 |
| Random (2) | `GroupModeRandom` | 随机打乱所有 items |
| Failover (3) | `GroupModeFailover` | 按 `Priority` 升序排列（priority 越小越优先，0 = 主通道） |
| Weighted (4) | `GroupModeWeighted` | 按 `Weight` 权重概率排列 |

默认（无 Mode）使用 RoundRobin。

### 2.2 粘性会话 (Sticky Session)

若 `Group.SessionKeepTime > 0`：

1. 查询全局会话表 `balancer.globalSession`（key = `apiKeyID:modelName`）
2. 若存在且未过期（`TTL = SessionKeepTime`），将对应 `(channelID, keyID)` 的候选移到列表**最前面**
3. 请求成功后，`SetSticky()` 写入/更新会话记录

### 2.3 遍历控制

- `Next()` — 移动到下一个候选，返回 false 表示遍历结束
- `Reset()` — 重置索引到 -1，用于 round 2 重试
- `Skip(channelID, keyID, name, msg)` — 记录跳过原因
- `SkipCircuitBreak(channelID, keyID, name)` — 检查熔断；若已熔断则自动 Skip
- `StartAttempt(channelID, keyID, name, keySuffix)` — 开始计时，返回 `AttemptSpan`

---

## 3. 单次尝试 (`relayAttempt`)

文件：`internal/relay/relay.go:272-380`

```go
type relayAttempt struct {
    *relayRun
    outAdapter      transformer.Outbound
    channel         *dbmodel.Channel
    usedKey         dbmodel.ChannelKey
    statusCode      int           // 上游 HTTP 状态码
    retryAfter      time.Duration // 429 响应中 Retry-After
    keyCooldown     time.Duration // 临时冷却时长
    rateLimited     bool          // 本地限流失败
    rateLimitWait   time.Duration // 限流等待时间
    responseWritten bool          // 已向客户端写入 SSE 事件
    upstreamURL     string        // 上游完整 URL
    tryNextKey      bool          // true=试同渠道下一 key; false=切渠道
}
```

### 3.1 `prepareAttempt()` — 准备一次尝试

```
prepareAttempt()
  ├─ op.ChannelGet(id)            — 从 DB 获取渠道（含缓存）
  ├─ channel.Enabled?             — 否 → Skip，nil
  ├─ 上下文窗口预检查              — 超出 → Skip，nil
  ├─ channel.GetChannelKeys(model)— 按 KeyMode 排序 Key
  │   ├─ KeyMode=0 (Cost)         — 按 TotalCost 升序（低成本优先）
  │   └─ KeyMode=1 (RoundRobin)   — 全局原子计数器轮转
  ├─ for each key:
  │   ├─ SkipCircuitBreak?        — 已熔断 → 试下一个 key
  │   ├─ isKeyModelCooling?       — 冷却中 → 试下一个 key
  │   ├─ newOutbound()            — 构建上游适配器
  │   └─ return relayAttempt
  └─ 所有 key 不可用 → Skip("all keys circuit-broken"), nil
```

### 3.2 上下文窗口预检查

`lookupModelMaxContext(modelName)` 查询 `LLMInfo.MaxContext`（来自价格数据库/缓存）。

`estimateTotalTokens(req)` 估算方法：
- `inputEst = len(requestBody) / 3`（保守：3 bytes/token，偏向高估以降低溢出风险）
- `outputBudget = max(max_tokens, max_completion_tokens, 4096)`
- `total = inputEst + outputBudget`

若 `total > MaxContext`，**跳过整个渠道**。

---

## 4. 故障状态码处理

文件：`internal/relay/relay.go:318-379`

`relayAttempt.run()` 中的 `statusCode switch`：

| 状态码 | 行为 | `tryNextKey` | 熔断 | Auth 更新 | 临时冷却 |
|--------|------|:---:|:---:|:---:|:---:|
| **200** (成功) | 记录使用量、清除冷却、记录成功、设置粘性 | — | ✅ RecordSuccess | AuthSuccess | ❌ |
| **本地限流** (`ErrRateLimited`) | 不触发熔断，记录等待时间 | — | ❌ | ❌ | RecordKeyModelTemporaryCooldown |
| **400** Bad Request | 直接返回错误给客户端，不重试 | ❌ | ❌ | AuthNone | ❌ |
| **401/403** Unauthorized/Forbidden | Auth 错误计数+1，连续 3 次禁用 key | ❌ | ✅ RecordFailure | AuthFailure | ❌ |
| **404** Not Found | 模型不支持，切渠道 | ❌ | ✅ RecordFailure | AuthNone | ❌ |
| **429** Too Many Requests | 冷却当前 (key, model)，试同渠道下一 key | ✅ | ❌ | AuthNone | RecordKeyModelCooldown (指数退避) |
| **5xx / 超时 / 流截断** | 记录失败 + 触发熔断器 + 临时冷却 | ✅ | ✅ RecordFailure | AuthNone | applyTemporaryKeyCooldown |

### 4.1 关键决策逻辑

- **`tryNextKey=true`** → 继续试同一渠道的下一个 key（429、5xx、超时）
- **`tryNextKey=false`** → 切到下一渠道（400、401/403、404）
- **`written=true`** → 已向客户端写入了响应，直接终止（不再重试）

### 4.2 临时冷却 (`applyTemporaryKeyCooldown`)

在 5xx / 超时 / 流中断后，对当前 (key, model) 施加临时冷却：

| 场景 | 冷却时长公式 |
|------|-------------|
| 首 token 超时 | `max(FirstTokenTimeOut × 2, 30s)` |
| 流硬超时 | `max(StreamHardTimeOut × 1, 60s)` |
| 流空闲超时 | `max(StreamIdleTimeOut × 2, 60s)` |
| 流读取错误（首 token 前） | `max(30s, keyCooldown)` |
| 流读取错误（首 token 后） | `max(60s, keyCooldown)` |
| 上游内容过滤器 | `max(30s, keyCooldown)` |
| `successShapedError` | `max(30s, keyCooldown)` |

`RecordKeyModelTemporaryCooldown` 不增加 429 计数器，仅设置冷却截止时间。

---

## 5. 熔断器 (Circuit Breaker)

文件：`internal/relay/balancer/circuit.go`

### 5.1 状态机

```
        连续失败 >= threshold
  Closed ───────────────────────→ Open
    ↑                              │
    │   请求成功 (清除记录)          │ 冷却时间到
    │                              ↓
    └──────────────────── HalfOpen ←
                              │
                              │ 试探请求失败
                              └──→ Open (TripCount++, 冷却翻倍)
```

- **Closed**：正常状态，记录连续失败次数
- **Open**：熔断中，所有请求被拒绝，直到冷却到期
- **HalfOpen**：冷却到期后，允许一个试探请求通过；成功→Closed，失败→Open（TripCount 递增）

### 5.2 冷却时间 — 指数退避

```
cooldown = base × 2^(tripCount - 1)
```

| tripCount | 倍数 | base=60s 时 |
|-----------|------|------------|
| 1 | 1× | 60s |
| 2 | 2× | 120s |
| 3 | 4× | 240s |
| 4 | 8× | 480s |
| … | … | … |
| 上限 | — | maxCooldown（默认 600s） |

### 5.3 配置层次

```
Channel 级覆盖 (circuit_breaker_threshold/cooldown/max_cooldown)
  └─ 未设置 → 全局设置 (SettingKeyCircuitBreakerThreshold/Cooldown/MaxCooldown)
       └─ 未设置 → 硬编码默认值 (threshold=5, cooldown=60s, maxCooldown=600s)
```

### 5.4 熔断键

熔断粒度为 `(channelID, keyID, modelName)`，组合键格式：`"channelID:keyID:modelName"`。

---

## 6. 限流 (Rate Limiting)

文件：`internal/relay/plugins/rate_limiter.go`

### 6.1 双层限流

`NewRateLimiter(ch, keyID, modelName)` 创建**两个独立检查**：

1. **Model 级限流** — 来自 `Channel.ModelRateLimit`
   - 格式：`"gpt-4=2/1m,claude-3=10/1h"`
   - Key：`"ch:{channelID}:m:{modelName}"`
   - 共享限流器（同渠道同模型的所有 key 共用）

2. **Key 级限流** — 来自 `Channel.RateLimit`
   - 格式：`"100/1h"`
   - Key：`"ch:{channelID}:k:{keyID}"`
   - 独立限流器（每个 key 单独计数）

**两个限流都独立生效**（v2 修复：此前 model_rate_limit 会覆盖 rate_limit）。

### 6.2 滑动窗口限流器

```go
type rateLimiter struct {
    mu       sync.Mutex
    count    int           // 窗口内允许的最大请求数
    interval time.Duration // 窗口长度
    ring     []time.Time   // 环形缓冲区
    idx      int           // 写入位置
}
```

- `TryAllow()` — 原子检查：允许返回 nil，否则返回 `RateLimitedError{Wait}`（包含等待时间）
- `WaitDuration()` — 计算到下一个可用槽位的等待时间
- 支持 `reconfigure()` 动态更新限流参数

### 6.3 限流规格格式

```
"count/timeUnit"

count = 正整数
timeUnit = Ns | Nm | Nh | Nd
示例: "100/1h" = 100次/小时, "5/30s" = 5次/30秒, "2/1m" = 2次/分钟
```

---

## 7. Key/Model 冷却 (Cooldown)

文件：`internal/model/channel.go:227-397`

### 7.1 两种冷却类型

| 类型 | 触发场景 | 计数器 | 退避 |
|------|---------|:---:|:---:|
| **429 冷却** | 上游返回 429 | consecutive429s++ | 指数退避（base × 2^shifts, 上限 30min） |
| **临时冷却** | 限流等待、流超时/中断 | 不增加 | 固定时长（仅覆盖更长者） |

### 7.2 429 指数退避算法

```
base = retryAfter (来自响应头) 或 2min + keyID%60 秒抖动
cooldown = base × 2^(consecutive429s - 1)
上限 = 30 分钟 (keyModelCooldownRetention)

连续 429 次数越多，冷却时间指数增长；成功请求后清除全部记录。
```

### 7.3 生命周期

- **设置**：`RecordKeyModelCooldown()` / `RecordKeyModelTemporaryCooldown()`
- **检查**：`isKeyModelCooling()` — 在 `prepareAttempt()` 的 key 遍历中调用
- **清除**：`ClearKeyModelCooldown()` — 请求成功时调用
- **后台清理**：每 10 分钟清理超过 30 分钟未活跃的条目

---

## 8. Round-Based 重试

文件：`internal/relay/relay.go:102-206`

### 8.1 两轮重试机制

```
Round 0: 遍历所有渠道/Key
  │
  ├─ 全部成功 → 返回
  ├─ 有 hard error (400/401/403/404) → 不重试，直接返回错误
  └─ 全部为 transient error（本地限流 + 429 + 5xx + 超时）
      │
      ├─ minWait=0（无可用的等待时间）→ 不重试
      ├─ minWait > RateLimitRetryWaitMax → 不重试
      └─ minWait ≤ max → 等待 minWait 后
          │
          Round 1: Reset iterator，重新遍历所有渠道/Key
            （此时冷却/熔断已过期或将要过期的 key 可能可用）
```

### 8.2 `minWait` 的来源

取所有 transient error 中最短的等待时间：

| 错误类型 | 等待时间来源 |
|----------|------------|
| 本地限流 (`rateLimited`) | `rateLimitWait` — 限流器返回的等待时间 |
| 上游 429 / 5xx (`tryNextKey`) | `retryAfter` — HTTP Retry-After 响应头 |

若两者都存在，取更小值。

### 8.3 `RateLimitRetryWaitMax` 控制

- `nil`（未设置）→ 默认 2 分钟
- `0` → 禁用 round 2 重试
- `> 0` → 等待上限（秒）

---

## 9. 流式超时控制

文件：`internal/relay/relay.go:488-551, 749-971`

### 9.1 三个超时维度

| 超时 | 配置字段 | 阶段 | 超时行为 |
|------|---------|------|---------|
| **ResponseHeaderTimeout** | `FirstTokenTimeOut` | HTTP 连接建立 → 响应头就绪 | Go HTTP Transport 层超时，仅影响连接阶段 |
| **First Token Timeout** | `FirstTokenTimeOut` | SSE 流建立 → 首个 token | 切换渠道（tryNextKey） |
| **Stream Idle Timeout** | `StreamIdleTimeOut` | 首个 token 后 → 事件间隔 | 终止流，临时冷却 key |
| **Stream Hard Timeout** | `StreamHardTimeOut` | 流建立 → 任意时刻 | 终止流，临时冷却 key |
| **Upstream Timeout** | `UpstreamTimeOut` | 非流式：完整 HTTP 往返 | 终止请求 |

### 9.2 关键设计决策

- `FirstTokenTimeOut` **不**设置在 `context.Context` 上 —— 否则超时后 Go HTTP Transport 会取消 response body 读取，导致即使首 token 已到达、流正在产生 token 也会被杀掉
- 改为在 `writeStream` 内使用独立 `time.Timer`，并从 Transport 层设置 `ResponseHeaderTimeout` 防止连接阶段无限等待

---

## 10. 日志与可观测性

### 10.1 上游错误日志

`LogUpstreamError()` — 根据错误类型分级：
- `ErrRateLimited` → Debug 级别
- 其他 HTTP 错误 → Warn 级别（含 URL、状态码、body preview）
- 非 HTTP 错误 → Warn 级别

### 10.2 调度状态 API

`GET /api/v1/channel/:id/scheduling-status` 返回每个 (key, model) 的：
- `CircuitBreakerStatus`（state, consecutive_failures, trip_count, cooldown_remaining）
- `CooldownStatus`（active, consecutive_429s, cooldown_until）
- `RateLimitStatus`（key_limit/model_limit: used/capacity/wait）

---

## 附录：配置参考

### Group 配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `mode` | int | 1 (RR) | 负载均衡模式 |
| `first_token_time_out` | int | 0 | 首 token 超时（秒），0=禁用 |
| `upstream_time_out` | int | 0 | 非流式上游超时（秒） |
| `stream_idle_time_out` | int | 0 | 流空闲超时（秒） |
| `stream_hard_time_out` | int | 0 | 流硬超时（秒） |
| `session_keep_time` | int | 0 | 粘性会话 TTL（秒），0=禁用 |
| `rate_limit_retry_wait_max` | int | nil | Round 2 等待上限（秒），nil=120s，0=禁用 |

### Channel 配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `key_mode` | int | 0 | Key 选择模式: 0=Cost 最低优先, 1=RoundRobin |
| `rate_limit` | string | `""` | Key 级限流: `"100/1h"` |
| `model_rate_limit` | string | `""` | Model 级限流: `"gpt-4=2/1m,claude=10/1h"` |
| `circuit_breaker_threshold` | int | nil | 熔断阈值，nil=全局默认 5 |
| `circuit_breaker_cooldown` | int | nil | 熔断基础冷却（秒），nil=全局默认 60 |
| `circuit_breaker_max_cooldown` | int | nil | 熔断最大冷却（秒），nil=全局默认 600 |

### 全局设置 (Setting)

| 键 | 默认值 | 说明 |
|----|--------|------|
| `circuit_breaker_threshold` | 5 | 连续失败多少次触发熔断 |
| `circuit_breaker_cooldown` | 60 | 基础冷却时间（秒） |
| `circuit_breaker_max_cooldown` | 600 | 最大冷却时间上限（秒） |

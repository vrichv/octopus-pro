# FAQ

## 目录

1. [Thinking mode 与 tool_choice 不兼容](#thinking-mode-与-tool_choice-不兼容)
2. [NVIDIA NIM 对接推荐配置](#nvidia-nim-对接推荐配置)

## Thinking mode 与 tool_choice 不兼容

### 错误描述

请求返回 HTTP 400 错误，日志如下：

```
Thinking mode does not support this tool_choice, code: invalid_request_error, type: invalid_request_error
```

### 原因分析

当请求中 `thinking` 参数生效时，某些模型的 API 对 `tool_choice` 取值有限制。OpenCode / Cline 等 coding agent 默认携带的 `tool_choice` 值与 thinking 模式不兼容，导致请求被拒绝。

### 解决方案

在渠道配置的 `param_override` 字段中设置 `tool_choice` 为 `auto`：

```json
{
  "tool_choice": "auto"
}
```

此格式为全局覆盖，所有经过该渠道的请求都会注入 `tool_choice: auto` 参数。

## NVIDIA NIM 对接推荐配置

### 问题描述

对接 NVIDIA NIM 时，部分模型或代理链路可能出现请求长时间无响应、SSE 流建立后迟迟没有首个事件，或流输出中途停止但连接不关闭。

### 推荐配置

下面的数值不是 NVIDIA 官方默认值，也不是 LiteLLM / GPUStack 原样配置，而是参考 LiteLLM 对 `timeout` / `stream_timeout` 的语义、LiteLLM 社区对“首个 chunk 超时”问题的讨论，以及 GPUStack 将 LLM 网关上游 idle timeout 调整到分钟级后的经验，整理出的 NIM 分组保守起点：

```json
{
  "upstream_time_out": 35,
  "first_token_time_out": 30,
  "stream_idle_time_out": 60,
  "stream_hard_time_out": 300
}
```

这组值的取舍是：首字阶段尽快失败并切换候选渠道，流开始后允许短暂停顿，但用 5 分钟硬上限避免连接无限挂住。实际生产环境可以按模型规模、上下文长度和代理链路延迟上调或下调。

### 字段说明

- `upstream_time_out`：上游请求超时，覆盖流建立前的请求阶段，建议 `35` 秒。
- `first_token_time_out`：首字超时，流建立后首个非空 SSE 事件的等待时间，建议 `30` 秒。
- `stream_idle_time_out`：流空闲超时，流开始后两个非空 SSE 事件之间的最大间隔，建议 `60` 秒。
- `stream_hard_time_out`：流硬超时，流建立后的最大持续时间，建议 `300` 秒。

### 注意事项

这些字段默认值均为 `0`，表示禁用。不要把上述值当成全局默认值；建议只为 NIM 分组手动配置，避免误杀慢速但正常的长上下文、本地模型、低速代理链路或图片/视频等长任务。

如果 NIM 渠道需要清理不兼容参数，继续使用渠道配置中的 `param_override`，不要在全局配置中改写 OpenAI 兼容参数。

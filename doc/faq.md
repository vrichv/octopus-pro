# FAQ

## 目录

1. [Thinking mode 与 tool_choice 不兼容](#thinking-mode-与-tool_choice-不兼容)

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

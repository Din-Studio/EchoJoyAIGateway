# 多协议网关非流式 Anthropic 请求丢失写缓存 TTL，导致写缓存不计费

- 发现时间：2026-10-13，Vercel AI Gateway 渠道真实 key 冒烟（PR #16，head `b53b9659`）
- 影响范围：`ProviderMultiProtocolGateway` 的所有渠道（Vercel、New API、GPT-Load 等），Anthropic Messages 非流式请求
- 引入时间：早于 PR #16，不是该 PR 引入的

## 现象

非流式 `POST /v1/messages`，带 `cache_control` 并首次写入缓存时，上游返回：

```json
"usage": {
  "input_tokens": 8, "output_tokens": 4,
  "cache_creation_input_tokens": 4951,
  "cache_creation": { "ephemeral_5m_input_tokens": 4951, "ephemeral_1h_input_tokens": 0 }
}
```

请求日志记录为：

| 字段 | 实际 | 期望 |
|---|---|---|
| `cache_write_5m_tokens` | 0 | 4951 |
| `cache_write_unknown_tokens` | 4951 | 0 |
| `pricing_completeness` | `partial` | `complete` |
| `estimated_cost_nano_usd` | 28000 | 6216750（`anthropic/claude-haiku-4.5`，写缓存 $1.25/M） |

TTL 未知的写缓存按设计不计价（`internal/pricing/quote.go:128-176`），所以这 4951 个 token 没有计费。

同样的场景走**流式**请求时记录正确：`cache_write_5m_tokens=6301`，`pricing_completeness=complete`，金额精确。读缓存（`cache_read_tokens`）在非流式请求中也记录正确。

## 原因

非流式透传路径 `executePassthrough` 收到 `response.PassthroughUsage` 后，调用 `usageEvidenceFromPassthrough`（`internal/execution/bifrost/passthrough.go:377`、`mapping.go:102`）。该函数先把 bifrost 归一化后的 usage 转成 OpenAI 格式（`toOpenAIUsage`），只保留 `prompt_tokens_details.cache_write_tokens` 一个合计值，再交给 `dialect.NewOpenAI().ExtractUsage` 解析。OpenAI dialect 会把 `cache_write_tokens` 记为 `CacheWriteUnknown`（`internal/dialect/openai_usage.go:97-127`）。Anthropic 原生的 5m / 1h 拆分在这一步丢失。

流式路径用 `anthropicWireUsage` 直接解析 Anthropic 原生 usage（`internal/execution/bifrost/anthropic_usage.go`，基于 `dialect.NewAnthropic()`），所以没有这个问题。

## 修复方向

客户端协议为 Anthropic 时，非流式透传应按 Anthropic 原生 usage 解析，与流式路径保持一致：对响应体使用 `dialect.NewAnthropic().ExtractUsage`，或复用 `anthropicWireUsage`。不要经过 OpenAI 格式中转。

## 验证方法

1. 用任一多协议网关渠道（如 Vercel）发非流式 Messages 请求，system 中放一段超过 4096 token 的文本并加 `cache_control: {"type":"ephemeral"}`。
2. 查 `GET /api/logs`，确认 `cache_write_5m_tokens` 等于上游的 `ephemeral_5m_input_tokens`，`pricing_completeness` 为 `complete`。
3. 补一个回归测试：MPG 非流式 Anthropic 响应带 `cache_creation.ephemeral_5m_input_tokens` 时，`UsageEvidence.Normalized.Tokens.CacheWrite5M` 应等于该值，`CacheWriteUnknown` 应为 0。

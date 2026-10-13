# 计划：新增 Vercel AI Gateway 上游渠道

- 分支：`feat/channel-vercel`
- 日期：2026-10-13
- 状态：草案，待确认

## 1. 目标与范围

在内置渠道里新增 `vercel`（Vercel AI Gateway），用户只填 API Key 就能把请求转发到 `https://ai-gateway.vercel.sh`。

**本次范围**：
- OpenAI Chat Completions、OpenAI Responses、Embeddings、Anthropic Messages（含 count_tokens）原生透传。
- 模型发现、探活。
- 按 Models.dev 的 `vercel` 目录自动定价。

**不在范围**：见第 8 节。

## 2. Vercel AI Gateway 能力摘要

调研日期为 2026-10-13，文档页面最近更新于 2026-09-08 至 2026-10-07。

| 能力 | Vercel 端点 | 鉴权 | 本次处理 |
|---|---|---|---|
| OpenAI Chat | `POST /v1/chat/completions` | `Authorization: Bearer` | 原生 |
| OpenAI Responses | `POST /v1/responses` | Bearer | 原生，按无状态处理（`store` 语义未确认） |
| Embeddings | `POST /v1/embeddings` | Bearer | 原生 |
| Anthropic Messages | `POST /v1/messages`、`/v1/messages/count_tokens` | `x-api-key` 或 Bearer | 原生 |
| 模型列表 | `GET /v1/models`（OpenAI 形态，含 pricing/type） | 可不鉴权 | 原生（OpenAI 协议） |
| Images | `POST /v1/images/generations`、`/v1/images/edits` | Bearer | 冒烟通过才声明 |
| Rerank | `POST /v1/rerank`（Cohere 兼容） | Bearer | 冒烟通过才声明 |
| Gemini 原生 | 无 | — | 不声明 |

补充事实：
- **模型 ID**：格式为 `creator/model`，例如 `anthropic/claude-sonnet-4.5`，所有端点通用。
- **路由选项**：放在请求体 `providerOptions.gateway`（`order`、`only`、`models`、`byok` 等），三种协议写法一致。
- **用量与费用**：响应 usage 为各协议的标准格式，响应里不直接返回费用。费用可以异步查 `GET /v1/generation?id=gen_...`。
- **错误码**：
  - 401：key 无效。
  - 402：余额不足，或预算超限（`quota_for_entity_exceeded`）。
  - 403：需要付费或绑卡。
  - 429：限流，部分响应带 `retry-after`。
- **Models.dev**：已有 `vercel` provider，约 402 个模型，模型 key 与上游 ID 完全一致，并带价格。

来源：
- https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-chat-completions
- https://vercel.com/docs/ai-gateway/sdks-and-apis/anthropic-messages-api
- https://vercel.com/docs/ai-gateway/sdks-and-apis/responses
- https://vercel.com/docs/ai-gateway/sdks-and-apis/rest-api
- https://vercel.com/docs/ai-gateway/models-and-providers/provider-options
- https://vercel.com/docs/ai-gateway/faq

## 3. 方案决策

**结论**：`ProviderMultiProtocolGateway`（MPG）配合 `EndpointFixedWithOverride`，固定 base URL 为 `https://ai-gateway.vercel.sh`。

理由：
1. MPG 已经能按客户端协议切换上游 profile，执行层不用改：
   - OpenAI 系走 `Authorization: Bearer` 和 `<root>/v1/...`。
   - Anthropic 走 `x-api-key`、`anthropic-version` 和 `<root>/v1/messages`。
   - 代码见 `internal/execution/bifrost/runtime_manager.go:150`、`internal/execution/bifrost/executor.go:1551`；`internal/execution/bifrost/newapi_runtime_test.go:34` 已有测试证明。
2. 用 `FixedWithOverride` 时用户不用填 URL：
   - 编译器只校验 FixedBaseURL 与可选的 `base_url` 参数（`internal/channel/compiler.go:495`），不限制 ProviderKind。
   - `resolvedTargetConfig` 会把固定地址写成 `{"base_url": ...}`（`internal/channel/channel.go:577`），MPG 正好读这个字段（`runtime_manager.go:169`）。
3. base URL 必须是根地址，不能带 `/v1`。MPG 会自己拼 `/v1/...`，带上 `/v1` 会变成 `/v1/v1/...`。

否决的方案：

| 方案 | 否决原因 |
|---|---|
| `ProviderOpenAICompatible` 预设（参考 Moonshot） | 只有 Chat 是原生的，Responses 和 Anthropic 都要经过 Chat 转换，Vercel 原生 Anthropic 端点的保真度（thinking、cache_control）就丢了。 |
| 新增 `ProviderVercel` | Bifrost v1.9.0 没有 Vercel provider（`capabilities.go:21` 会拒绝），要像 Cline（#778）那样自写约 1000 行执行器，现有需求不需要。 |

**副作用**：
- MPG 没有转换路径（`converted.go:478`），Vercel 也没有 Gemini 原生端点，所以这个渠道不接 Gemini 入站。
- 网关的 Gemini 路径本来就拒绝带 `/` 的模型（`internal/gateway/router.go:189`），所以这个副作用没有额外损失。

## 4. 渠道声明（`internal/channel/modules/vercel.go`）

```go
func Vercel() spec.Module {
	return spec.Module{
		Definition: spec.Definition{
			ID:          spec.Vercel,
			Name:        "Vercel AI Gateway",
			Mark:        "VC",
			Icon:        "vercel",
			SearchTerms: []string{"ai gateway"},
			Description: "Managed API preset",
			Connection: spec.Connection{
				Type:            spec.ConnectionAPIKey,
				CredentialInput: "entries",
			},
			Params: []spec.Field{{
				Key: "base_url", Label: "Base URL", InputKind: spec.InputURL,
				Normalizer: spec.NormalizeBaseURL,
			}},
			Credentials: []spec.Field{{
				Key: "api_key", Label: "API Key", InputKind: spec.InputSecret,
				Required: true, Sensitive: true, Normalizer: spec.NormalizeNonEmpty,
			}},
			Provider: spec.ProviderBinding{
				ProviderKind:      spec.ProviderMultiProtocolGateway,
				CatalogProviderID: "vercel",
				EndpointPolicy:    spec.EndpointFixedWithOverride,
				FixedBaseURL:      "https://ai-gateway.vercel.sh",
			},
			Routes: []spec.Route{
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationChatCompletion, execution.RouteNative),
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationListModels, execution.RouteNative),
				spec.NewRoute(protocol.OpenAICompletions, execution.OperationProbe, execution.RouteNative),
				spec.NewResponsesCreateRoute(execution.RouteNative, spec.ResponsesStoreHandlingStateless),
				spec.NewRoute(protocol.OpenAIResponses, execution.OperationProbe, execution.RouteNative),
				spec.NewRoute(protocol.OpenAIEmbeddings, execution.OperationEmbeddingsCreate, execution.RouteNative),
				spec.NewRoute(protocol.OpenAIEmbeddings, execution.OperationProbe, execution.RouteNative),
				spec.NewRoute(protocol.Anthropic, execution.OperationChatCompletion, execution.RouteNative),
				spec.NewRoute(protocol.Anthropic, execution.OperationCountTokens, execution.RouteNative),
				spec.NewRoute(protocol.Anthropic, execution.OperationProbe, execution.RouteNative),
				// Images / Rerank：第 7 节冒烟通过后追加。
			},
		},
	}
}
```

声明要点：
- **Responses 用 `Stateless`**：Vercel 的 `store` 和 `previous_response_id` 是否真的持久化没有确认，做法与 OpenRouter 一致（`internal/channel/modules/openrouter.go:41`）。如果误声明成 `UpstreamManaged`，会启用 response 绑定和 `require_stored` 调度。
- **不声明 Anthropic ListModels**：Vercel 的 `/v1/models` 返回 OpenAI 形态，不是 Anthropic 形态。
- **不声明 Responses Compact**：Vercel 的 compact 只支持 OpenAI 模型。
- **搜索词避开 `compatible`**：`registry_test.go:46` 断言搜索 `compatible` 只能命中 OpenAICompatible。
- **`providerOptions` 原样透传**：`stripNativeControlFields`（`internal/execution/bifrost/passthrough.go:225`）只删除 `provider`、`api_key` 等字段。代价是 Chat 顶层的 `provider:{sort}` 简写会被删掉，客户端需要改用 `providerOptions.gateway.sort`。

## 5. 改动清单

| # | 文件 | 改动 | 必须 |
|---|---|---|---|
| 1 | `internal/channel/spec/definition.go:15` | 新增 `Vercel ID = "vercel"` | 是 |
| 2 | `internal/channel/channel.go:47` | 再导出 `Vercel = spec.Vercel` | 是 |
| 3 | `internal/channel/modules/vercel.go` | 新建，内容见第 4 节 | 是 |
| 4 | `internal/channel/builtins.go:29` | 在 `modules.Groq()` 之后注册 `modules.Vercel()` | 是 |
| 5 | `web/src/frontends/modern/assets/channels/vercel.svg` | 渠道图标；缺失时回退显示 Mark | 建议 |
| 6 | `README.md`、`README_CN.md`、`README_JP.md`（约 :144、:151） | 渠道清单和 Embeddings 支持说明 | 建议 |

不需要改的部分：
- `container.go`、`capabilities.go`、`converted.go`、`sdk_defaults.go`
- `catalog/official.json`、`price_priority.go`
- 数据库迁移、`dialect/*`、`gateway/*`
- 前端逻辑：渠道名、图标、默认 URL 都来自后端 descriptor。

## 6. 测试计划

**必须同步的现有测试**：
1. `internal/channel/registry_test.go:17`：在 `wantIDs` 的 `Groq` 后面插入 `Vercel`。
2. `internal/channel/registry_test.go:829`：在 `TestEveryResponsesCreateChannelDeclaresStoreHandling` 的映射里加 `Vercel: Stateless`。
3. `internal/channel/final_provider_contract_test.go:10`：把 `Vercel` 加进批准列表。
4. `internal/channel/route_golden_test.go:44`：重算 `wantDigest`，测试失败时输出的就是新值。
5. `internal/channel/embeddings_module_test.go:15`：把 `Vercel` 加进 `supported`。
6. `internal/channel/rerank_module_test.go:18`：只有声明 Rerank 时才需要加。

**新增测试**：
1. `internal/channel/vercel_module_test.go`，仿照 `newapi_module_test.go`，断言以下三点：
   - descriptor 的 `default_base_url` 是 `https://ai-gateway.vercel.sh`，`base_url` 不是必填；
   - `Resolve(vercel, nil)` 的 `TargetConfig` 是 `{"base_url":"https://ai-gateway.vercel.sh"}`，用户覆盖 URL 时生效；
   - 路由模式表符合第 4 节，且没有 Gemini 路由。
2. `internal/execution/bifrost/vercel_runtime_test.go`，仿照 `newapi_runtime_test.go:24`，用 httptest 断言四点：
   - OpenAI Chat 走 `/v1/chat/completions`，带 `Authorization: Bearer`；
   - Anthropic 走 `/v1/messages`，带 `X-Api-Key` 和 `Anthropic-Version`；
   - 请求体里的 `providerOptions` 原样到达上游；
   - `model: "anthropic/claude-sonnet-4.5"` 不被改写。

   第一项要覆盖 MPG 配合 FixedWithOverride 的组合，这个组合目前还没有先例。

**命令**（本地验证即合并门禁）：
```bash
make test-deps
go test ./internal/channel/... ./internal/execution/... ./internal/provideradapter/... ./internal/control/... ./internal/container/...
make check
```

## 7. 真实 key 冒烟（xlqy 测试环境）

部署到 `~/projects/EchoJoyAIGateway` 后，用一个真实的 Vercel key 建分组，逐项确认：

1. 发现模型能列出 `anthropic/...`、`openai/...`，且价格自动匹配到 `vercel` 目录。
2. Chat 的非流式和流式请求都正常；请求日志里 input/output/cached tokens 都被正确记录。这一项用来补上文档没写清楚的流式 usage 格式。
3. Responses 的非流式和流式请求都正常；带 `store:true` 加 `previous_response_id` 时的实际表现要记录下来，作为以后是否改成 `UpstreamManaged` 的依据。
4. Anthropic Messages 用 Claude Code 指向网关测试：thinking、tool use、`cache_control` 正常，count_tokens 正常。
5. Embeddings 正常；Images（`openai/gpt-image-*`）和 Rerank（`cohere/rerank-*`）也测一下，通过的话就在第 4 节把对应路由加上，并更新相应测试。

## 8. 不做的事（YAGNI）

- **不接 Gemini 入站**：Vercel 没有 Gemini 原生端点，MPG 也没有转换路径。
- **不读取 `/v1/generation`、`/v1/credits`**：不做费用对账和余额展示，费用按目录价格计算。
- **不做 BYOK 和 `providerOptions` 的 UI 或配置**：客户端请求体里带上即可透传。
- **不对 402、403 做 Vercel 专属的凭证状态映射**：走现有通用错误处理。冒烟时观察即可，确实有问题再单独提。
- **不做视频、语音、realtime、`/v1/responses/compact`。**

## 9. 未决与风险

1. **Responses 的有状态语义未确认**：先按 Stateless 处理，第 7 节第 3 项会给出依据。
2. **Images、Rerank 的请求格式兼容性未实测**：由第 7 节第 5 项决定是否声明。
3. **Models.dev 的 `vercel` 目录属于外部数据**：如果将来缺失，自动定价会退回 `price_priority.go` 的兜底顺序，可能命中 openrouter 的价格。

## 工作量估计

| 内容 | 估时 |
|---|---|
| 代码与测试（第 5、6 节） | ~1.5 h |
| 冒烟（第 7 节） | ~30 min |

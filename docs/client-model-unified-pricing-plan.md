# 修改计划：对外模型统一定价，费用与上游费用分开计算

- 调研日期：2026-10-19，基于提交 `5491f162`
- 状态：第 2 版，已实施并在本地验证（2026-10-19，分支 `feat/client-model-unified-pricing`），见第 9 节
- 决策：第 3 节的 A、B、C 三项均采用推荐方案（2026-10-19 用户确认）；D 项是审查中新增的，同样采用推荐方案

本文说明三件事：为什么把价格挂到对外模型（ClientModel）上，结算时怎样分别算出"费用"和"上游费用"，以及每一层要改什么。第 7 节记录第 1 版中被删掉或改正的内容及理由。

## 1. 需求与结论

从需求出发，只有两条必须满足：

- **R1**：调用方被收取的费用只由 ClientModel 的统一价、用量和密钥倍率决定，与路由到哪个分组、渠道、上游模型无关。
- **R2**：每个请求同时得到"费用"和"上游费用"两笔金额，两者分别计价、分别落库。

满足这两条的最小方案：

1. 新增 `client_model_prices` 表，为每个 ClientModel 存一份统一价。现有的 `model_prices`（渠道 × 上游模型）保持不变，展示时改称**上游价格**。
2. 两组价格放进同一个 `pricing.Table`，网关在同一个价格快照上一次算出两笔钱：
   ```
   费用     = Quote(client_model 统一价, usage, mode) × 密钥倍率
   上游费用 = Quote((channel, upstream_model) 上游价格, usage, mode) × 分组倍率
   ```
3. **费用**沿用现有的 `estimated_cost_*` 列，密钥额度、密钥用量、用量报表这些使用方都不用改。**上游费用**只新增 3 个金额与状态列和 1 个计价明细列。
4. 用访问密钥登录时，控制面不返回上游价格和上游费用。
5. 服务尚未上线，历史数据不回填。

## 2. 现状

1. **价格挂在哪**：`model_prices` 以 `(channel_id, model_id)` 为唯一键（`internal/storage/models/model_price.go:52`）。`GET /api/models` 按 `client_model` 聚合，但价格挂在 `upstream_models[].price` 上（`internal/control/project_model_collection.go:70-75`）。
2. **ClientModel 的来源**：分组模型启用别名时取别名，否则取上游模型 ID（`internal/control/project_model_collection.go:255-257`）。网关把同一个名字作为 `ExternalModel` 交给调度器（`internal/gateway/handler.go:766`），所以它可以作为全局唯一的定价主体。
3. **结算**：`quoteFrozenAttempt`（`internal/gateway/request_log.go:593`）按 `(channel, upstream_model)` 查价，再乘分组倍率和密钥倍率，得到唯一一笔 `EstimatedCostNanoUSD`。这笔钱用于：
   - 密钥额度扣减（`internal/gateway/handler.go:588` → `estimatedCostNano()`）
   - 日志与 `usage_stats`
   - 订阅账号卡片的参考费用
4. **问题**：
   - 同一个 ClientModel 走不同分组时，调用方被扣的钱不一样，违反 R1。
   - 收费与成本混在同一笔钱里，违反 R2。

## 3. 决策

| # | 问题 | 结论 | 理由 |
|---|---|---|---|
| A | 两种倍率分别作用于哪笔钱 | 分组倍率只作用于上游费用，密钥倍率只作用于费用 | 分组倍率如果留在费用里，会直接违反 R1。改到上游费用不需要新代码，但分组倍率的含义会从"收费倍率"变为"上游成本倍率"，分组页的说明文案要随之修改 |
| B | ClientModel 还没设统一价 | 按未定价处理：费用为 0，`cost_state=unpriced`；上游费用照常计算 | 与现在缺少上游价格时的行为一致，不会在不知情时退回按上游价收费 |
| C | auto 模型按哪个模型收费 | 按 auto 解析出的目标模型的统一价收费；判断费用保持现状单独计算 | auto 只是别名，实际被调用的是目标模型 |
| D | 统一价是否支持模式价（fast、ultrafast） | 支持。服务端接受任意合法模式，不与模型目录比对；编辑器提供 fast、ultrafast 两个可选项 | 如果不支持，fast 请求会按标准价收费，而上游按 fast 价计成本，每次都会少收。统一价没有对应的目录可比，现有"模式集合必须与目录一致"的约束（`internal/control/model_price.go:393`）对它不适用 |

## 4. 契约

### 4.1 两笔钱

| | 费用 | 上游费用 |
|---|---|---|
| 价格来源 | `client_model_prices`，按路由模型名查找 | `model_prices`，按 `(channel_id, upstream_model)` 查找 |
| 倍率 | 密钥倍率（分组倍率固定为 1） | 分组倍率（密钥倍率固定为 1） |
| 金额与状态列 | `estimated_cost_nano_usd`、`estimated_cost_nano`、`cost_state`、`pricing_completeness`，均沿用现有列 | `upstream_cost_nano_usd`、`upstream_cost_state`、`upstream_pricing_completeness`，均为新增列 |
| 计价明细 | `request_log_attempts.pricing_receipt`，含义改为费用的明细 | `request_log_attempts.upstream_pricing_receipt`，新增列 |
| 本币金额 | 落库（额度与报表需要） | 不落库，读取时用该行冻结的 `exchange_rate_micros` 换算 |
| 使用方 | 密钥额度、密钥用量、用量报表、分组用量、账号卡片、日志 | 日志 |
| 用访问密钥登录时 | 返回 | 不返回 |

- "固定为 1"已有先例：auto 判断费用就是以 `Group: pricing.DefaultPriceMultiplier` 调用同一个函数（`internal/automodel/decision.go:194-197`）。
- **路由模型名**的取法：有 auto 选择结果时取其 `TargetModel`，否则取客户端请求的模型名。现在的转发循环已经用这个规则计算 `routeModel`（`internal/gateway/handler.go:1000-1003`），把它抽成 `requestRecorder` 上的一个方法，两处共用。
- 不能从单次尝试的 `observations.Model` 中取模型名。分组配置了参数覆盖时，这个字段会被清空（`internal/gateway/handler.go:1031-1040`）。

### 4.2 统一价的数据结构

`client_model_prices` 表：

| 列 | 说明 |
|---|---|
| `id` | 主键 |
| `client_model` | varchar(255)，唯一 |
| `input/output/cache_read/cache_write_price_nano_usd_per_million_tokens` | 可为空，空表示该项未定价 |
| `context_price_tiers`、`mode_price_schedules` | json，格式与 `model_prices` 相同 |
| `created_at_ms`、`updated_at_ms` | 时间戳 |

- 价格按美元存储。HTTP 边界负责与系统币种互相换算，复用 `internal/control/model_price_currency.go`。
- 不设 `is_manual` 列：统一价只能手动设置。
- 没有对应行即表示"待定价"。只提供写入接口，不提供删除接口，也不接受四项基础价格全部为空的写入。

### 4.3 模块职责

| 模块 | 负责 | 不负责 |
|---|---|---|
| `internal/pricing` | 两组规则的校验、存储和计价，共用一套计价算法 | 不知道什么是分组、访问密钥、路由 |
| `internal/gateway` | 为每个请求决定两笔钱各用哪条规则、哪个倍率（只在 `quoteFrozenAttempt` 一处） | 不实现计价算法 |
| `internal/requestlog` | 校验计价明细与金额是否一致，然后落库 | 不重新计价 |
| `internal/control` | 统一价的读写，以及按登录身份决定返回哪些字段 | 写入统一价时不依赖分组或模型聚合 |

## 5. 分层改动

### 5.1 存储（新迁移 `0022_client_model_prices`）

1. 新建 `client_model_prices`（4.2 节）和对应的 `models.ClientModelPrice`，复用 `NormalizeContextPriceTiers` 和 `NormalizeModePriceSchedules`。
2. `request_logs` 新增 `upstream_cost_nano_usd`、`upstream_cost_state`、`upstream_pricing_completeness`，并新增一条与 `chk_request_log_usage_pricing_state` 形状相同的 CHECK，约束 `usage_state` 与上游费用状态的组合。
3. `request_log_attempts` 新增 `upstream_pricing_receipt json`。

### 5.2 `internal/pricing`

1. 从 `QuoteForModeWithReceipt` 中把查价之后的计算抽成私有函数 `quoteRule(rule, receiptRule, result, mode)`，乘倍率的逻辑同样基于它实现。
2. `Table` 增加 `clientRules map[string]Rule`，`NewTable` 同时接收两组规则。
3. 新增 `QuoteClientModel(clientModel, result, mode, accessKey)`：只接收密钥倍率，分组倍率在明细里固定为 1。上游费用继续使用现有的 `QuoteForModeWithMultipliers`。
4. `ReceiptRule` 增加 `client_model`。计价明细的 schema 升到 7：费用明细只填 `client_model`，上游明细只填 `channel_id` 和 `model_id`。`validateReceiptRule` 按版本分别校验。
5. `loadPriceTable`（`internal/control/pricing_runtime.go:39`）同时读取两张表。控制面写入走 `withControlTransaction`，会推进集群配置版本，其他节点通过 `cluster_reload.go:88` 调用同一个 `loadPriceTable` 重新加载，所以 `PriceRuntime`、配置发布和集群重载都不用改。

### 5.3 网关（`internal/gateway`）

1. `quoteFrozenAttempt` 在 `frozen.table` 这同一个快照上算出两笔钱：
   - 费用：路由模型名 + `{Group: 1, AccessKey: 密钥倍率}`
   - 上游费用：`(channel, upstream_model)` + `{Group: 分组倍率, AccessKey: 1}`
2. `telemetry.PricingObservation` 增加 `UpstreamCostState`、`UpstreamPricingCompleteness`、`UpstreamCostNanoUSD`、`UpstreamReceiptJSON`。
3. `estimatedCostNano()` 不改，密钥额度继续扣"费用 + 判断费用"。
4. 不给 `frozenAttemptPricing` 增加字段：路由模型名在整个请求中不变，结算时从 `requestRecorder` 取即可。

### 5.4 落库（`internal/requestlog/mapper.go`）

1. 费用明细的校验（`mapper.go:235-240`）由"规则等于 `(channel, upstream_model)`"改为"规则等于路由模型名"。
2. 上游明细新增一条同样形状的校验：规则等于 `(channel, upstream_model)`，且总额等于 `UpstreamCostNanoUSD`。
3. 写入 3 个上游费用列和 `upstream_pricing_receipt`。

### 5.5 控制面 API（`internal/control`）

1. `GET /api/models`：
   - `ProjectModelDTO` 增加 `price`（统一价，未设置时为 `null`）。
   - 用访问密钥登录时，去掉 `upstream_models[].price`。
2. `PUT /api/client-model-prices`：
   - 请求体为 `{client_model, prices, context_tiers, mode_schedules}`，价格按系统币种填写。
   - 新建或覆盖，四项基础价格全为空时拒绝。
   - 经 `auditMutation` 记审计，在 `withControlTransaction` 中写入，然后发布价格表。
   - ClientModel 名称可能含有 `/`，所以放在请求体里，不放在路径参数里。
3. 日志接口（`request_logs.go`）：
   - 管理员可以拿到上游费用、它的本币换算值（读取时按冻结汇率计算）和上游计价明细。
   - 用访问密钥登录时（`request_logs.go:302`）不返回这三项。

### 5.6 前端（`web/src/frontends/modern`）

1. 模型页：
   - `ModelCard` 头部展示统一价，未设置时显示"待定价"。点击后用 `ModelPriceEditor` 编辑，模式价可选 fast、ultrafast。
   - 上游来源行的价格列改称"上游价格"。
2. 日志页：
   - 新增"上游费用"列。
   - 详情页的"费用与计价"中，用 `LogPricingReceipt` 分别展示两份计价明细。
3. 分组页：价格倍率的说明改为"上游成本倍率，只影响上游费用"。中、英、日三种语言同步修改。

### 5.7 文档

`docs/openapi.yaml`、`README_CN.md`、`README.md`：补充统一价接口、两笔钱的定义、倍率归属，以及用访问密钥登录时哪些字段不可见。

## 6. 消融验证

### 6.1 机制消融

每个机制都试着去掉，看 R1、R2 是否还能满足。"保留"表示去掉后需求不成立，或者需要的代码更多。

| 机制 | 去掉后的结果 | 结论 | 依据 |
|---|---|---|---|
| `client_model_prices` 表 | 没有地方存储与渠道无关的价格，R1 不成立。存进分组模型 JSON 会让每个分组各存一份，同样违反 R1 | 保留 | ClientModel 没有自己的实体表，只能从分组模型 JSON 派生 |
| 复用 `model_prices`，用一个固定的 `channel_id` 存统一价 | 价格目录同步、待定价补齐（`internal/control/price_reconcile.go:31`）、引用计数都会把这些行当作上游价格处理，每处都要加特殊判断 | 不采用 | 特殊情况会散落到多个模块，耦合度更高 |
| `Table` 中的第二组规则 | 统一价需要单独的运行时和发布路径，要改 4 处发布与重载代码 | 保留 | `internal/control/service.go:384`、`:525`，`cluster_reload.go:88`，`group_idempotency.go:180` |
| 从费用中去掉分组倍率 | 同一个模型走不同分组，收费不同，R1 不成立 | 保留 | 消融测试 T1 |
| 把分组倍率用于上游费用 | 去掉也能满足需求，但分组倍率会变成任何地方都不用的配置 | 保留，且不需要新代码 | 冻结信息里已有分组倍率（`internal/gateway/handler.go:163`） |
| 缺少统一价时不回退到上游价 | 回退会让未定价的模型按上游价收费，R1 不成立 | 保留 | 消融测试 T2 |
| 用 auto 的目标模型作为路由模型名 | 会去查 auto 别名的价格，实际是查不到，费用变成 0 | 保留 | `internal/gateway/auto_model.go:264` 返回的元数据已改写为目标模型；消融测试 T3 |
| 从 recorder 取路由模型名，不从单次尝试的元数据取 | 分组配置参数覆盖后 `Model` 为空，查不到统一价 | 保留 | `internal/gateway/handler.go:1031-1040`；消融测试 T4 |
| 统一价的模式价 | fast 请求按标准价收费，上游却按 fast 价计成本 | 保留（决策 D） | `internal/pricing/quote.go:90-100`：请求的模式在规则中不存在时回退到标准价 |
| 上游费用的 3 个列 | 没有地方保存上游费用，R2 不成立 | 保留 | — |
| 上游计价明细列 | 金额无法追溯到当时的价格和倍率，落库时也无法做"明细与金额一致"的校验 | 保留 | `internal/requestlog/mapper.go:235-240` 的校验模式 |
| 上游费用的本币列 | 单行可以用冻结汇率精确换算 | 删除 | `request_logs.exchange_rate_micros` |
| `usage_stats` 中的上游费用汇总 | R1、R2 都只要求按请求计价，汇总属于报表需求 | 删除 | 第 7 节 |
| 用访问密钥登录时隐藏上游字段 | 新增的上游字段会让访问密钥持有人看到上游成本 | 保留 | `internal/control/request_logs.go:302`、`project_model_collection_http.go:15` |

### 6.2 消融测试

以下测试在实现阶段编写。每个测试都在对应机制被去掉或写错时失败，从而证明这个机制必不可少：

| # | 场景 | 断言 | 去掉哪个机制时失败 |
|---|---|---|---|
| T1 | 同一个 ClientModel 分别走分组倍率为 1 和 2 的两个分组 | 两次的费用相同，上游费用相差一倍 | 费用中仍乘分组倍率 |
| T2 | 有上游价格、没有统一价 | 费用为 unpriced 且为 0，上游费用为 priced | 回退到上游价 |
| T3 | auto 请求选中目标模型 X | 费用明细的 `client_model` 等于 X | 按 auto 别名查价 |
| T4 | 分组配置了参数覆盖 | 费用仍按统一价计算 | 从单次尝试的元数据取模型名 |
| T5 | 在节点 A 修改统一价 | 节点 B 重新加载后，按新价格收费 | `loadPriceTable` 没有读取统一价 |
| T6 | 用访问密钥登录，读取日志和 `/api/models` | 响应中没有上游价格和上游费用字段 | 隐藏逻辑 |
| T7 | 设置了费用额度的密钥 | 额度扣减的是费用，不是上游费用 | `estimatedCostNano()` 改错了数据源 |

## 7. 相对第 1 版的修改

### 7.1 改正

1. **第 1 版说"复用编辑器就能免费得到模式价"，这不成立。** 现有校验要求模式集合与模型目录一致，统一价没有目录可比，所以新增决策 D，并为统一价单独定义校验规则。
2. **第 1 版漏改了明细校验。** 落库时会校验明细规则等于 `(channel, upstream_model)`（`internal/requestlog/mapper.go:235-240`），费用明细改按 ClientModel 计价后，这条校验必须同步修改。
3. **第 1 版打算给 `frozenAttemptPricing` 增加 `clientModel` 字段，这既不必要也不可靠。** 路由模型名在一个请求中不变，可以从 recorder 取；而单次尝试的元数据在参数覆盖时不含模型名。

### 7.2 删除（不影响 R1、R2）

| 删除项 | 理由 |
|---|---|
| `upstream_cost_nano` 本币列 | 可以按冻结汇率在读取时精确换算 |
| `usage_stats` 中的 3 个上游费用列，以及账号卡片、分组用量改用上游费用 | 属于报表需求。这些使用方继续使用费用，含义依然成立 |
| `DELETE` 统一价接口 | 删除后恢复"待定价"，相当于免费，风险大于用处 |
| 毛利展示、"从上游价格复制"按钮、待定价筛选、`summary` 新计数 | 便利功能，不属于两条需求 |
| 首页、用量页展示上游费用，日志按上游费用排序和筛选 | 报表与检索需求，可以在需要时再加 |

## 8. 实施步骤与估时（合计约 5.5 小时）

| 步骤 | 内容 | 估时 | 验证方式 |
|---|---|---|---|
| 1 | 5.1 存储迁移，5.2 `pricing` 包 | 约 1 小时 | 迁移测试；`pricing` 包两组规则的计价测试，以及 schema 7 明细的校验测试 |
| 2 | 5.3 网关，5.4 落库 | 约 1.5 小时 | 消融测试 T1 至 T4、T7；mapper 测试 |
| 3 | 5.5 控制面 API | 约 1 小时 | 消融测试 T5、T6；统一价写入的 HTTP 测试 |
| 4 | 5.6 前端 | 约 1.5 小时 | 类型检查与单测；本地启动后核对模型页、日志页、分组页 |
| 5 | 5.7 文档，本地全量验证 | 约 0.5 小时 | 全量后端测试与前端检查；在测试服务器 `xlqy` 上用真实请求核对两笔钱 |

步骤 1 到 3 必须按顺序进行；前端步骤 4 在步骤 3 的接口确定后即可开始。

## 9. 实施记录（2026-10-19）

### 9.1 与计划的差异

| 计划 | 实际 | 理由 |
|---|---|---|
| 日志接口新增扁平字段 `upstream_cost_*` | 新增一个对象 `upstream_cost: {cost_state, pricing_completeness, cost_nano_usd, cost_nano}`，访问密钥身份下整组省略 | 一组相关字段放在一起，可见性也由一个字段控制 |
| 用访问密钥登录时去掉 `upstream_models[].price` | 保留价格对象，只清空金额、阶梯、模式价和定价方式 | 前端靠价格对象里的渠道信息渲染来源行，结构不变就不用改前端校验 |
| `request_logs` 上游费用的 CHECK 与现有费用列形状相同 | 只保留"金额 ≥ 0"和"用量 × 状态 × 完整度组合"两条约束 | 组合约束已逐一列出合法取值，单列的 `IN` 约束是多余的；GORM 也不支持同一字段写两个 `check` 标签 |
| 历史数据不回填 | 迁移时把已有行的上游费用设为原费用 | 不回填会违反新增的组合约束；而统一定价之前的费用本来就是按上游价格算的 |

### 9.2 消融实验结果

逐一破坏机制，确认对应测试失败，然后还原：

| 被破坏的机制 | 失败的测试 |
|---|---|
| A1 费用仍乘分组倍率 | `TestClientModelChargeDoesNotDependOnTheRoutedGroup`（T1） |
| A2 缺少统一价时回退到上游价 | `TestClientModelWithoutUnifiedPriceIsUnpricedWhileUpstreamCostIsPriced`（T2） |
| A3 按 auto 别名计价 | `TestAutoModelIsChargedByItsTargetModel`（T3） |
| A4 从单次尝试的元数据取模型名 | `TestParameterOverrideGroupStillChargesTheClientModel`（T4）；不配置参数覆盖时仍能通过，与 7.1 节的判断一致 |
| A5 `loadPriceTable` 不加载统一价 | `TestExternalClusterClientModelPricePropagation`（T5）和统一价 HTTP 测试 |
| A6 用访问密钥登录时不隐藏 | 模型列表与日志各有一个测试失败（T6） |
| A7 额度扣上游费用 | `TestHandlerFreezesPriceMultipliersAndAccountsTheSameEstimate` 等 2 个测试 |

### 9.3 本地端到端验证

用 clusterbench 的假上游（每次请求输入 3 个 token、输出 2 个 token）启动网关：上游价为输入 1000、输出 2000 USD/百万 token，分组倍率 0.5；统一价为输入 3000、输出 6000，密钥倍率 2。

- 费用为 0.042 USD，计价明细 v7 的规则为 `client_model: public-mini`；上游费用为 0.0035 USD，计价明细 v6 的规则为 `openai / gpt-4o-mini`。
- 密钥额度两次请求共扣 0.084 USD，扣的是费用。
- 在界面上把统一价改为 3500 / 7000 后，下一次请求的费用立即变为 0.049 USD，上游费用不变。
- 用访问密钥登录时，能看到统一价，看不到上游金额和上游费用；写统一价返回 403。


### 9.4 代码审查（奥卡姆剃刀、高内聚低耦合）

审查后做了以下修改：

| 问题 | 修改 |
|---|---|
| 统一价报价函数接收完整的倍率组，调用方必须记得把分组倍率固定为 1 | 改为 `QuoteClientModel(…, accessKey)`，从类型上排除分组倍率；倍率合法性校验只在 `applyQuoteMultipliers` 中保留一处 |
| 读取明细和映射响应时，各自按版本号区间判断是不是统一价明细（`< 7`、`!= 7`） | 统一用 `Rule.ClientModel` 判断；只有 `pricing` 包按版本号区分两种明细 |
| `ClientModelPriceDTO` 和 `ModelPriceDTO` 各写一遍价格字段和币种换算 | 两者内嵌同一个 `PriceSchedulesDTO`，换算只保留一份；JSON 字段不变 |
| 日志前端分别为费用和上游费用写了两套明细查找和金额显示 | 合并为 `attemptReceipt(key)` 和 `costCell`；前端不再解析未使用的 `cost_nano_usd` |

保留不改的：

- `pricing.Rule` 与 `ClientRule` 共用一个调度结构：涉及 98 处字面量，改动面大于收益。
- 处理器和服务各校验一次请求：与现有 `UpdateModelPrice` 的做法一致。
- 事务前先编译一次单条统一价：消融 A8 证明它负责把无效模型名报为 400，去掉后变成 500。

消融补充（均已还原）：

| 被破坏的机制 | 失败的测试 |
|---|---|
| A8 去掉事务前的统一价预编译 | `TestClientModelPriceHTTPRejectsInvalidRequests` |
| A9 读取时对 v7 明细也校验渠道身份 | `TestPriceMultipliersSurvivePersistenceAndAllCostQueries` |
| A10 响应映射对 v7 明细查渠道注册表 | `TestRequestLogDetailReturnsChargeAndUpstreamReceipts`（新增） |
| A11 去掉倍率合法性校验 | `TestInvalidMultipliersMakeBothQuotesUnavailable`（新增；覆盖所有计价项都未定价、但仍带明细的情况） |
| A12 响应里丢掉明细的 `client_model` | `TestRequestLogDetailReturnsChargeAndUpstreamReceipts` |

### 9.5 价格显示尾数

在 3001 实例上验证时发现：系统币种为 CNY、汇率 7.3 时，输入 ¥30000 后显示为 ¥30000.000000001。原因是价格按 nano USD 存储，30000 ÷ 7.3 除不尽，显示时又按精确值换算回来。上游价格也有同样问题，来自系统币种功能。

修复：新增 `Currency.PriceFromNanoUSD`。在所有能换算回同一个 nano USD 的本币值中，取小数位最少的那个显示；模型价格、auto 模型价格和日志计费明细的单价都改用它。这样输入值能原样读回，原样再保存时存储值不变。费用金额仍按 `Settle` 精确结算，不受影响。消融：让它直接返回精确换算值，`TestPriceFromNanoUSDReadsBackEnteredPrices` 失败，并复现同样的 `30000.000000001`。

### 9.6 详情页内直接编辑价格

按反馈把价格编辑从"切换到单独的编辑视图"改成在模型详情页内直接修改：

- 管理员在详情页看到的统一价和上游价格都是输入框（`ModelPriceForm`）。表单只负责草稿与校验；保存由详情页底部的「保存」按钮一次提交所有改过的价格（原「编辑价格」按钮），标记为不计费的确认框也由详情页弹出。
- 阶梯价通过页面内的「添加阶梯」插入 `ModelPriceTierEditor` 组件；四个价格输入抽成 `ModelPriceSlotFields`，标准档、模式档与阶梯档共用。
- 保存成功、放弃修改或恢复自动价格后，表单按服务端数据重建。非管理员仍看到只读价格表。
- 删除了原来的 `ModelPriceEditor`、`ModelPriceEditorIdentity` 以及不再使用的文案键 `edit`、`editClientPrice`、`cancel`。

# 修改计划：对接灵影网关文本计费回调（查余额、上报用量）

- 调研日期：2026-10-19，基于提交 `9fa1fffc`
- 状态：第 2 版，经过对抗性审查（第 6 节）；已实施并在本地联调（2026-10-19，分支 `feat/lingying-billing-callback`），见第 10 节
- 决策：第 3 节的 A、B、C 三项均采用推荐方案（2026-10-19 用户确认）
- 上游契约：灵影网关 `docs/aigw-billing-callback-api.md`（`lingying-gateway` 仓库），服务端实现见灵影 `internal/api/aigw_billing.go`

文本请求（`/v1/chat/completions`、`/v1/messages`、`/v1/responses`）已经由本网关处理，计费账户仍在灵影网关。本文说明以下内容：

- 需求从哪来，最小方案是什么；
- 灵影相关代码怎样集中在 `internal/integration/ly` 一个模块里；
- 第 1 版方案中哪些机制被审查推翻；
- 每个保留的机制怎样用消融实验证明是必需的。

## 1. 需求与结论

需求只从上游契约推出，共五条：

- **R1 准入**：灵影用户的 key 发起文本请求时，转发上游之前必须查余额；`usable=false` 或额度不足时拒绝。
- **R2 上报**：通过准入的请求结束后，无论成功或失败都要上报一次。上报内容是用量和原始成本，单位人民币元，不乘角色倍率。
- **R3 可靠**：上报要持久化，重启后不丢；5xx 和网络错误要用同一个 `request_id` 重试数小时。
- **R4 身份**：`user_id` 在准入时确定，不在上报时用 key 反查，因为用户可能已经删除了 key。
- **R5 隔离**：非灵影的 key、不计费的端点，行为与现在完全一致。

满足这五条的最小方案由三个机制组成：

1. **准入闸门**：只拦截被声明为计费端点、且 key 名称以 `ly:` 开头的请求。闸门放在模型解析之后、auto 判断之前，调用余额接口；拒绝时直接返回客户端。余额接口返回的 `user_id`、`key_id` 由 recorder 带到请求结束。
2. **同步入队**：请求结束、`emit` 请求日志的同一处，把上报内容同步 `INSERT` 到 outbox 表。现有的密钥额度结算也在这里同步写 Redis，做法一致。
3. **投递循环**：每个副本运行一个循环。每次用一条 `UPDATE … RETURNING` 同时完成两件事：领取一条到期记录，并把它的下次重试时间排好。然后发送 POST，按返回码删除这一行，或把它停放。

所有灵影相关代码都放在 `internal/integration/ly`。网关只依赖它的两个方法，`requestlog`、`control` 不感知它。

## 2. 现状

1. **鉴权**：`authenticate` 用 key 明文的哈希查内存快照 `AccessKeysByHash`（`internal/gateway/auth.go:32-35`），不读数据库也不读 Redis。明文只在 `extractClientKey`（`auth.go:56-95`）里出现，之后不再保存。
2. **key 的元数据**：`AccessKeyView` 有 `Name`，没有 `Note`（`internal/state/snapshot.go:174-186`）。灵影同步 key 时写入 `name = "ly:<key_id>"`、`note = <user_id>`，key 的值与灵影个人 API Key 的明文相同（灵影 `internal/api/userkeys.go:196`）。
3. **HTTP 准入顺序**（`internal/gateway/handler.go`）：
   1. 鉴权（中间件）
   2. 生成请求 ID（:546）
   3. 创建 recorder，注册 deferred 收尾（:568-593）
   4. 密钥额度检查（:599）
   5. RPM 限流（:615）
   6. 读请求体（:671）
   7. `InspectRequest` 解析模型（:701），`setClientModel`（:721）
   8. auto 模型判断（:746，这一步会调用判断模型，产生费用）
   9. 调度与候选循环
4. **websocket**：每轮的模型在入队时就已解析（`websocket_turn.go:81`）。`executeTurn` 里先查额度（:151），再限流（:160）。
5. **请求结束**：所有出口最终都调用 `requestRecorder.emit()`（`internal/gateway/request_log.go:129`），并且只调用一次：
   - HTTP 由 deferred 收尾调用（`handler.go:583-596`）；
   - websocket 由 `finish` 调用（`websocket_turn.go:108-123`）。

   deferred 中先同步调用 `completeAccessQuota`（写 Redis），再调用 `emit`。
6. **费用口径**：`recorder.estimatedCostNano()`（`request_log.go:189-203`）等于"统一价费用 + auto 判断费用"，按请求开始时冻结的币种折算为本币 nano 单位。密钥额度扣的就是这个数。
7. **事件内容**：`telemetry.RequestEvent`（`internal/telemetry/requestlog.go:145-170`）有：
   - `RequestID`、`CompletedAt`、`DurationMs`
   - `Protocol`、`ClientModel`、`UpstreamModel`
   - `Status`（取值 `success`、`error`、`incomplete`、`canceled`）、`ErrorCode`、`ErrorSummary`
   - `Usage`、`Currency`

   没有开始时间，没有 `user_id`、`project_id`。
8. **灵影怎样使用可选字段**（灵影 `aigw_billing.go:404-508`）：
   - `upstream` 只写入日志的 `SelectedSupplier` 和 `SupplierModelKey`；
   - `prices` 只写入订单明细，用于展示；
   - `protocol` 只写入 `InputData`；
   - 以上都不校验。`model` 是必填字段，为空时返回 400，属于永久失败。
9. **ALB**：公网和内网两个 ALB 上，新网关的规则都只匹配 `/gateway/v1/chat/completions`、`/gateway/v1/messages*`、`/gateway/v1/responses*`（`deploy/aws/gateway.yaml:2-7`）。所以灵影的 key 能到达的端点里，还包括 `/v1/messages/count_tokens` 和 responses 的子资源。同一域名下的其他路径，例如 `/internal/aigw/*`，走旧网关的整域名规则。
10. **基础设施**：数据库连接池每个副本 10 个连接（`internal/platform/config/config.go:25`）；仓库里没有 outbox 表，也没有用过 `SKIP LOCKED`。

## 3. 决策

| # | 问题 | 结论 | 未采用的备选 | 理由 |
|---|---|---|---|---|
| A | 怎样判断一个 key 走灵影计费 | `Name` 以 `ly:` 开头 | 给 access key 加显式字段 | `Name` 已在快照里，灵影已固定这个格式。加字段要改迁移、快照、控制面和灵影的同步请求 |
| B | 系统货币不是 CNY 时怎么办 | 对灵影的 key 返回 503，打错误日志 | 控制面禁止把币种改离 CNY | 契约要求 `cost` 为人民币。币种为 USD 时没有人民币汇率，无法换算。在准入阶段拦住只需一个判断 |
| C | 可选字段 `prices`、`upstream` | 不传 `prices`；`upstream` 传 `UpstreamModel` | 从冻结价格表取四项单价，从快照取渠道名 | 两者都只用于展示和记录（第 2 节第 8 条）。对账可以用 `request_id` 关联本网关的 `request_logs`，那里有完整的渠道、价格和计价明细。补齐它们要新增两条数据通道 |

## 4. 契约

### 4.1 模块边界

```
internal/integration/ly/          灵影网关对接，唯一知道灵影契约的地方
  client.go      HTTP 客户端：Balance、ReportUsage；按 HTTP 状态码分类结果
  admission.go   Admit：生效判断（ly: 前缀）、币种检查、余额判定 → Verdict + Admission
  report.go      由 Admission + RequestEvent + 费用生成上报请求体；金额格式化
  outbox.go      outbox 表的入队、领取、完成、停放
  deliverer.go   投递循环 Run(ctx)
  service.go     New(config.LingyingBillingConfig, *sql.DB)：组装以上部分，对外只暴露 Admit、Record、Run
```

**依赖方向**：

- `ly` 只依赖 `telemetry`（`RequestEvent`）、`protocol`、`pricing`（`Currency`）和标准库（`net/http`、`database/sql`），不依赖 `gateway`、`requestlog`、`control`、`state`、`storage` 和 gorm（10.1 第 1 条）。
- 网关在使用方一侧定义一个接口：

```go
type externalBilling interface {
    Admit(ctx context.Context, in ly.AdmitInput) (*ly.Admission, ly.Verdict)
    Record(admission *ly.Admission, event telemetry.RequestEvent, chargeNano int64)
}
```

- `AdmitInput` 包含 `KeyName`、`Plaintext`、`ProjectID`、`Currency`（币种代码）。不是灵影的 key 时，返回的 `Admission` 为 `nil`。网关只在 `billing_gate.go` 中使用这些类型（10.8）。
- `Verdict` 有五种取值：`Allow`、`InvalidKey`、`Unusable(reason)`、`Insufficient`、`Unavailable`。把它映射成客户端看到的错误由网关负责（`reason.go`），因为错误格式归网关管。

**放在 `ly` 以外的部分**（遵循仓库现有约定）：

- 迁移放在 `internal/storage/migrations`，统一注册；
- 环境变量在 `internal/platform/config` 中统一读取，以普通字段交给 `container` 组装；
- 投递循环由 `internal/app/app.go` 启停，与 `requestLogs` 并列。

### 4.2 生效范围

两个条件同时满足才查余额、才上报：

1. 路由在 `dataPlaneEndpointCatalog`（`internal/gateway/router.go:44-118`）上声明了 `ExternalBilling: true`。只给以下路由加：
   - `POST /v1/chat/completions`
   - `POST /v1/messages`
   - `POST /v1/responses`
   - `/v1/responses` 的 websocket 每一轮
2. key 的 `Name` 以 `ly:` 开头。这个前缀常量定义在 `ly` 内部。

`count_tokens`、responses 的查询和取消子路由不加标记。原因：对 `status=success, cost=0` 的上报，灵影会写一对 `hold(0) + refund(0)` 订单。Claude Code 会频繁调用 `count_tokens`，如果这些请求也上报，用户的订单列表里会出现大量 0 元订单。

### 4.3 准入

- **位置**：
  - HTTP：放在 `setClientModel` 之后（`handler.go:721`）、auto 判断之前（:746）；
  - websocket：放在限流之后（`websocket_turn.go:160`）。
- **明文 key**：鉴权时把明文存进现有的 `dataPlaneRequestContext`。HTTP 和 websocket 都经过同一个鉴权中间件，所以都能从这里取到。
- **项目 ID**：取请求头 `X-Project-Id`，没有时为 `null`。灵影把它写进日志的 `ProjectID`。
- **放行条件**：`usable && available > 0`，余额接口超时 3 秒。

| 情况 | Verdict | 返回客户端 | reason code |
|---|---|---|---|
| 未配置灵影计费，或系统货币不是 CNY | `Unavailable` | 503 | `billing_unavailable`，打错误日志 |
| 灵影返回 404 | `InvalidKey` | 401 | `invalid_access_key`（沿用） |
| `usable=false` | `Unusable` | 403 | `billing_account_unusable`，message 带灵影的 `reason` |
| `available <= 0` | `Insufficient` | 402 | `insufficient_balance` |
| 灵影返回其他非 200 状态码，或网络错误、超时 | `Unavailable` | 503 | `billing_unavailable`，打错误日志 |

放行后，`Admission` 挂在 recorder 上，内容是 `user_id`、`key_id`、`project_id`。

### 4.4 上报

`emit` 构造完 `RequestEvent` 后，如果 recorder 上有 `Admission`，就调用 `Record(admission, event, recorder.estimatedCostNano())`。`Record` 生成上报请求体，然后同步 `INSERT` 到 outbox：超时 5 秒，主键冲突时忽略。插入失败时打错误日志，日志里带完整请求体，以便人工补报。

| 上报字段 | 来源 |
|---|---|
| `request_id` | `event.RequestID` |
| `user_id` / `key_id` / `project_id` | `Admission` |
| `model` | `event.ClientModel` |
| `upstream` | `event.UpstreamModel`（决策 C） |
| `protocol` | `openai-completions` → `openai`；`anthropic` → `anthropic`；`openai-responses` → `responses` |
| `status` | `success` → `success`；其余三种 → `failed` |
| `error_code` / `error_message` | `event.ErrorCode` / `event.ErrorSummary` |
| `usage.input_tokens` | 未命中缓存的输入 token |
| `usage.output_tokens` | 输出 token |
| `usage.cache_read_tokens` | 读缓存 token |
| `usage.cache_write_tokens` | 写缓存 token 三项之和：5m、1h、TTL 未知 |
| `cost` | `chargeNano`，也就是密钥额度扣的同一个数，从 nano-CNY 四舍五入到 6 位小数，与请求日志的显示一致（见 10.6） |
| `started_at` | `event.CompletedAt - event.DurationMs`，RFC 3339 毫秒精度 |
| `duration_ms` | `event.DurationMs` |

### 4.5 outbox 与投递

表 `ly_usage_reports`：

| 列 | 类型 | 说明 |
|---|---|---|
| `request_id` | `varchar(36)` 主键 | 幂等键 |
| `payload` | `jsonb` | 完整上报请求体，入队时定稿，重试时不重新计算 |
| `attempts` | `int` | 已投递次数 |
| `next_attempt_at_ms` | `bigint`，可为空 | 下次投递时间；`NULL` 表示已停放，不再投递 |
| `last_error` | `text` | 最近一次失败原因 |

另建部分索引：`(next_attempt_at_ms) WHERE next_attempt_at_ms IS NOT NULL`。

领取用一条语句完成，不开显式事务，HTTP 调用期间也不占用数据库连接：

```sql
UPDATE ly_usage_reports
SET attempts = attempts + 1,
    next_attempt_at_ms = :now + :timeout + :backoff(attempts)
WHERE request_id = (
  SELECT request_id FROM ly_usage_reports
  WHERE next_attempt_at_ms <= :now
  ORDER BY next_attempt_at_ms LIMIT 1
  FOR UPDATE SKIP LOCKED)
RETURNING request_id, payload, attempts
```

- **领取即安排重试**：领取时就把下次重试时间写好。副本在投递途中崩溃，这一行到期后会被任意副本重新领取，不需要额外的租约机制。
- **退避**：`backoff` 依次为 1s、2s、4s……，最长 5 分钟。
- **时间下限**：下次重试时间至少比现在晚一个请求超时（20 秒；灵影处理一次上报最长 15 秒），所以投递还在进行时，这一行不会被别的副本领走。（10.9 改为 30 秒）
- **节奏**：投递循环逐条处理，直到没有到期记录，然后休眠 1 秒。（10.9 改为批量领取、批量上报）

投递结果的处理：

| 灵影返回 | 处理 |
|---|---|
| 200（含 `duplicate=true`） | 删除这一行 |
| 400、422 | 把 `next_attempt_at_ms` 置为 `NULL` 并写入 `last_error`，打错误日志 |
| 其他非 200 状态码、网络错误、超时 | 写入 `last_error`。下次重试时间在领取时已经写好，这里不用再改 |

可重试的错误不设放弃上限。401、403 表示配置错误，修正后积压的记录会自动补报。

**正确性依据**：灵影按 `request_id` 幂等，所以重复投递不会重复扣费。`SKIP LOCKED` 和"时间下限"只是用来减少重复投递的流量，正确性并不依赖它们（第 7.1 节）。

## 5. 分层改动

| 层 | 文件 | 改动 |
|---|---|---|
| 灵影模块 | `internal/integration/ly/*`（新） | 按 4.1 的六个文件实现，每个文件配一个测试文件 |
| 配置 | `internal/platform/config/config.go`、`.env.example`、README 的环境变量表 | 新增 `LINGYING_BILLING_BASE_URL`、`LINGYING_BILLING_TOKEN`；只配置一个时启动报错 |
| 存储 | `internal/storage/migrations/0023_ly_usage_reports.go`、`internal/storage/migration.go` | 建表与部分索引；在迁移列表中注册 |
| 组装 | `internal/container`、`internal/app/app.go` | 构造 `ly.Service` 并注入网关；启停投递循环 |
| 网关路由 | `internal/gateway/router.go` | 路由结构加 `ExternalBilling bool`，并在 4.2 列出的路由上设置 |
| 网关鉴权 | `internal/gateway/http_routes.go` | 鉴权成功时把明文存进 `dataPlaneRequestContext` |
| 网关闸门 | `internal/gateway/billing_gate.go`（新） | 定义 `externalBilling` 接口；HTTP 与 websocket 共用的调用函数；把 Verdict 映射为 reason |
| 网关接入点 | `internal/gateway/handler.go:721`、`websocket_turn.go:160` | 各调用一次闸门 |
| 网关原因 | `internal/gateway/reason.go` | 新增 `billing_account_unusable`（403）、`insufficient_balance`（402）、`billing_unavailable`（503） |
| 网关记录 | `internal/gateway/request_log.go` | recorder 增加一个回调 `recordBilling`，由闸门在准入时设置；`emit` 末尾调用它 |
| 部署 | `deploy/aws/gateway.yaml`、`scripts/put-secrets.sh`、`OPERATIONS.md` | 环境变量、SSM Secret 和 IAM 资源行；由人工输入 token（与灵影的 `AI_GATEWAY_CALLBACK_TOKEN` 相同）；补充积压和停放记录的排查 SQL |

`requestlog`、`telemetry`、`state`、`control` 都不改。

## 6. 对抗性审查：第 1 版的问题

| # | 第 1 版的做法 | 问题 | 第 2 版的修正 |
|---|---|---|---|
| 1 | 闸门放在读请求体之前 | 请求体非法时 `model` 为空。这样的请求已经通过准入，必须上报，但灵影会以 `model is required` 返回 400 永久拒收，R2 不成立 | 闸门移到 `setClientModel` 之后。模型解析失败的请求不会进入闸门，也不会上报 |
| 2 | 只说"在 RPM 之后" | 没有约束闸门必须在 auto 判断之前。auto 判断会调用判断模型并产生费用，余额不足的用户也会触发这笔费用 | 明确放在 auto 判断（`handler.go:746`）之前 |
| 3 | outbox 写在 `requestlog` 的 `writeBatch` 事务里 | 1）通用的请求日志模块会依赖灵影，耦合度高；2）内存队列有约 1 秒的丢失窗口，批量写库失败时整批丢失，R3 不成立 | 由 `ly` 在 `emit` 时同步插入，和现有密钥额度结算的同步写入一致。`requestlog` 不改，第 1 版的决策 B 随之取消 |
| 4 | `RequestEvent` 新增 `StartedAt`、`Billing` 字段 | 没有必要：`started_at` 可以由 `CompletedAt − DurationMs` 精确得到；计费上下文由 recorder 持有，`emit` 时一并传给 `Record` | 不改 `telemetry` |
| 5 | `cost = EstimatedCostNano + DecisionCostNano`，在映射里重新求和 | 与 `recorder.estimatedCostNano()` 重复（违反 DRY），两处可能计算出不同的结果 | 直接传入 `estimatedCostNano()`，保证额度扣的数和上报的数是同一个 |
| 6 | 未配置灵影计费时不启用闸门 | `ly:` 开头的 key 在配置缺失时会被免费放行 | 返回 `Unavailable`（fail-closed） |
| 7 | outbox 先领取再推迟 30 秒租约，另有 `state` 列 | 两步操作加一个状态列；"租约"和"退避"其实是同一个时间 | 用一条 `UPDATE … RETURNING` 完成领取并写好重试时间；用 `NULL` 表示停放 |
| 8 | 把 `SKIP LOCKED` 当作正确性保证 | 正确性来自灵影的幂等。`SKIP LOCKED` 只是减少重复投递的手段 | 保留这个子句，但在 7.1 中如实标注 |
| 9 | 从冻结价格表取四项单价，从快照取渠道名 | 为两个只用于展示的可选字段新增两条数据通道 | 决策 C：不传 `prices`，`upstream` 传 `UpstreamModel` |
| 10 | 灵影相关代码分散在 `gateway`、`requestlog` 和一个新包里 | 契约知识分散在多处，修改时要跨模块 | 集中在 `internal/integration/ly`；网关只依赖两个方法 |

## 7. 消融验证

### 7.1 机制消融

逐个去掉机制，检查 R1–R5 是否还成立。

| # | 机制 | 去掉后的结果 | 结论 | 消融测试 |
|---|---|---|---|---|
| A1 | 准入闸门 | 余额为负的用户照样能调用，R1 不成立 | 保留 | T1、T2 |
| A2 | 闸门在模型解析之后 | 请求体非法的请求会被上报且被永久拒收，R2 不成立 | 保留 | T3 |
| A3 | 闸门在 auto 判断之前 | 余额不足的请求也会产生判断费用 | 保留 | T4 |
| A4 | 路由标记 `ExternalBilling` | `count_tokens` 等请求也会上报，产生大量 0 元订单，R5 不成立 | 保留 | T5 |
| A5 | `ly:` 前缀判断 | 非灵影 key 会去查余额，得到 404 后被拒绝，R5 不成立 | 保留 | T6 |
| A6 | 未配置或灵影不可用时 fail-closed | 故障期间可以免费使用，R1 不成立 | 保留 | T7 |
| A7 | websocket 每轮也过闸门 | websocket 请求完全不计费，R1、R2 不成立 | 保留 | T8 |
| A8 | CNY 检查 | 美元金额会被当成人民币上报，少收约 7 倍 | 保留 | T9 |
| A9 | `user_id` 取自余额接口的返回 | 只能在上报时用 key 反查，key 已删除时会失败，R4 不成立；改用 `note` 又要改快照 | 保留 | T10 |
| A10 | `cost` 取自 `estimatedCostNano()` | 如果改为只用统一价费用，auto 请求会漏掉判断费用 | 保留 | T11 |
| A11 | outbox 持久化 | 灵影返回 5xx 或网关重启时，上报就丢了，R3 不成立 | 保留 | T12 |
| A12 | 按返回码分类处理 | 全部重试：400 会无限重试；全部放弃：遇到 5xx 就漏扣费 | 保留 | T13 |
| A13 | 领取时写好重试时间 | 投递途中崩溃的那一行再也不会被投递，R3 不成立 | 保留 | T14 |
| A14 | `SKIP LOCKED` 与时间下限 | 多个副本会重复投递同一行。灵影是幂等的，所以金额仍然正确 | 保留：都只有一个子句或一个加数，能减少重复流量 | T15 只断言金额正确，不把投递次数当作正确性依据 |
| A15 | `RequestEvent.StartedAt` | 可以从 `CompletedAt − DurationMs` 精确得到 | 删除 | — |
| A16 | `prices`、渠道名 | 都是可选字段，只用于展示，对账另有途径 | 删除（决策 C） | — |
| A17 | 余额缓存 | 一次内网调用约 10ms，相对 LLM 请求可以忽略；没有测量依据 | 不做 | — |

### 7.2 消融测试

| # | 场景 | 断言 | 去掉或写错哪个机制时失败 |
|---|---|---|---|
| T1 | `ly:` key，模拟灵影返回 `available="0"` | 返回 402；上游没有被调用；outbox 为空 | A1 |
| T2 | 模拟灵影返回 `usable=false, reason=disabled` | 返回 403，message 中带 `disabled` | A1 |
| T3 | `ly:` key，请求体不是合法 JSON | 返回 400；余额接口没有被调用；outbox 为空 | A2 |
| T4 | `ly:` key 请求 auto 模型，余额不足 | 返回 402；判断模型没有被调用 | A3 |
| T5 | `ly:` key 调用 `/v1/messages/count_tokens` | 余额接口没有被调用；outbox 为空 | A4 |
| T6 | 非 `ly:` key 正常请求 | 余额接口没有被调用；outbox 为空；响应与现在一致 | A5 |
| T7 | 模拟灵影超时；另一个用例未配置灵影计费 | 都返回 503 `billing_unavailable`；上游没有被调用 | A6 |
| T8 | 同一个 websocket 连接发两轮 | 余额接口被调用两次；outbox 有两行，`request_id` 不同 | A7 |
| T9 | 系统货币为 USD | 返回 503；上游没有被调用 | A8 |
| T10 | 余额接口返回 `user_id=u1, key_id=k1`，请求带 `X-Project-Id: p1` | payload 中三个字段等于 `u1`、`k1`、`p1` | A9 |
| T11 | auto 请求成功 | payload 中的 `cost` 等于密钥额度扣减额换算后的值 | A10 |
| T12 | 灵影先返回两次 503，再返回 200；中途换一个新的投递循环实例 | 最终收到同一个 `request_id`，outbox 中这一行被删除 | A11 |
| T13 | 灵影分别返回 400、422、401、500 | 400、422 后这一行停放；401、500 后会再次投递 | A12 |
| T14 | 领取后不完成，相当于模拟崩溃；把时间推进到重试时间之后 | 这一行被再次领取 | A13 |
| T15 | 两个投递循环并发处理 100 行 | 每个 `request_id` 都入账，并且只入账一次；模拟灵影本身实现幂等 | A14 |
| T16 | payload 格式（表驱动） | `cost` 最多 6 位小数且四舍五入；`status`、`protocol` 映射正确；`started_at` 为毫秒精度 | 4.4 的映射 |

### 7.3 实验协议

实现阶段对 A1–A14 逐一做实验：

1. 先在代码中破坏这个机制，例如删掉闸门调用、交换闸门位置、去掉路由标记、去掉 `SKIP LOCKED`。
2. 运行对应的测试，确认它失败。
3. 还原代码。

每个机制的结果都记入本文的实施记录。如果破坏后测试仍然通过，说明测试没有覆盖到这个机制，或者这个机制是多余的；先修正测试或删掉机制，再继续。

## 8. 风险与待验证项

1. **网络路径**：本网关的 ECS 任务能否访问 `https://lingying-gateway.echojoy.cn/internal/aigw/*`，并被 alb-internal 转给旧网关。需要在测试环境和生产环境各用 curl 实测一次。
2. **未定价模型**：统一价没有设置时，`cost` 为 `"0"`，灵影只写日志不扣费。上线前要确认对灵影开放的模型都已经设置了统一价。
3. **密钥倍率**：`estimatedCostNano()` 包含 access key 的价格倍率。灵影的 key 必须保持倍率为 1，否则会和灵影的角色倍率叠加。灵影创建 key 时不设倍率，这一点符合要求。
4. **outbox 插入失败**：PostgreSQL 不可用时，这条上报会丢，只留下带完整 payload 的错误日志。这时请求日志同样写不进去，属于整体故障。
5. **并发透支**：灵影不做预扣，并发请求可能把余额扣成负数。灵影已经确认可以接受。

## 9. 实施步骤与估时（合计约 6 小时）

| # | 内容 | 估时 |
|---|---|---|
| 1 | `ly` 模块的 `client`、`report`、`admission`，加上配置项和单元测试（T2、T7 的灵影侧部分，T9、T10、T16） | ~1.5h |
| 2 | 迁移 `0023` + `outbox` + `deliverer` + pgtest 集成测试（T12–T15） | ~1.5h |
| 3 | 网关的路由标记、明文保存、闸门、reason、recorder 接入（HTTP 和 websocket）+ 网关测试（T1–T8、T11） | ~2h |
| 4 | 按 7.3 做消融实验，结果记入本文 | ~0.5h |
| 5 | 部署配置和文档；在测试服务器 xlqy 上用模拟灵影跑端到端验证 | ~0.5h |

## 10. 实施记录（2026-10-19）

### 10.1 与计划的差异

1. **outbox 改用 `database/sql`，不用 gorm。** 现有架构测试 `TestDataPlaneDependencyGraph`（`internal/gateway/dependency_test.go`）禁止数据面依赖 gorm、`storage` 和 `control`。outbox 本来就是手写 SQL，换成标准库只需改占位符，`ly` 仍是一个包，不需要新增接口。容器用 `gorm.DB.DB()` 取出连接池传给 `ly.New`。数据面因此会在请求结束时写一次数据库，这正是 4.4 选定的同步入队方案。
2. **`authenticate` 改为接收明文。** 明文由 `authenticateDataPlaneRequest` 用 `extractClientKey` 取出一次，同时传给 `authenticate` 并存入 `dataPlaneRequestContext.clientKey`。如果鉴权后再提取一次，`?key=` 查询参数已经被删掉，就取不到了。
3. **路由标记在 `prepareDataPlaneRequest` 中写入 `route.ExternalBilling`。** 端点目录 `dataPlaneEndpoint.externalBilling` 是声明处，`resolve` 函数不用改。
4. **网关测试处理器补注册了 Anthropic 协议。** 第一次跑消融时，A4（去掉路由标记）没有让测试失败。原因是测试处理器只注册了 OpenAI 协议，`count_tokens` 在路由阶段就返回 404，根本没走到闸门。修正后，测试还会断言 `count_tokens` 确实被转发，A4 随之失败。
5. **websocket 每轮都过闸门，不读路由标记。** websocket 只在 `/v1/responses` 上建立，每一轮都是 `response.create`，所以不需要再判断。

### 10.2 文件

| 层 | 文件 |
|---|---|
| 灵影模块 | `internal/integration/ly/{service,client,admission,report,outbox,deliverer}.go`，测试 `{admission,report,outbox}_test.go` |
| 网关 | `billing_gate.go`（新）、`router.go`、`http_routes.go`、`auth.go`、`handler.go`、`request_log.go`、`websocket.go`、`websocket_turn.go`，测试 `billing_gate_test.go`（新） |
| 存储 | `migrations/0023_ly_usage_reports.go`、`migration.go`，三处迁移顺序断言 |
| 配置与组装 | `platform/config/config.go`、`container/container.go`、`app/app.go` |
| 文档与部署 | `.env.example`、三语 README、`deploy/aws/{gateway.yaml,scripts/put-secrets.sh,OPERATIONS.md}` |

### 10.3 消融实验结果

用脚本逐一破坏机制，运行对应测试，然后还原：

| 被破坏的机制 | 结果 | 失败的测试 |
|---|---|---|
| A1 去掉 HTTP 准入闸门 | 失败 ✔ | `TestExternalBillingRejectionsStopBeforeTheUpstream`（4 个子用例） |
| A2 闸门移到读请求体之前 | 失败 ✔ | `TestExternalBillingSkipsRequestsThatAreNotGenerations/invalid_body` |
| A3 闸门移到 auto 判断之后 | 失败 ✔ | `TestExternalBillingAndAutoModelDecision/rejected_before_the_decision` |
| A4 去掉路由标记（所有转发路由都计费） | 失败 ✔ | `TestExternalBillingSkipsRequestsThatAreNotGenerations/count_tokens` |
| A5 去掉 `ly:` 前缀判断 | 失败 ✔ | `TestAdmitLetsOtherKeysThroughWithoutCallingLingying` |
| A6a 未配置时放行 | 失败 ✔ | `TestAdmitFailsClosed/not_configured` |
| A6b 灵影不可达时放行 | 失败 ✔ | `TestAdmitFailsClosed/timeout` |
| A7 去掉 websocket 每轮闸门 | 失败 ✔ | `TestExternalBillingAdmitsEveryWebsocketTurn` |
| A8 去掉 CNY 检查 | 失败 ✔ | `TestAdmitFailsClosed/system_currency_is_not_CNY` |
| A9 `user_id` 改由 key 名推出 | 失败 ✔ | `TestAdmitAllowsAPositiveBalanceAndKeepsTheAccount` |
| A10 `cost` 只用答复费用 | 失败 ✔ | `TestExternalBillingAndAutoModelDecision/charge_includes_the_decision` |
| A11 不落 outbox，请求结束时直接发送 | 失败 ✔ | `TestDeliveryRetriesTheSameReportUntilAccepted` |
| A12a 400、422 也重试 | 失败 ✔ | `TestDeliveryClassifiesLingyingResponses`（400、422） |
| A12b 所有失败都停放 | 失败 ✔ | `TestDeliveryClassifiesLingyingResponses`（401、403、500）、`TestDeliveryRetriesTheSameReportUntilAccepted` |
| A13 领取时不安排重试 | 失败 ✔ | `TestClaimedReportIsRetriedAfterAReplicaDies` |
| A14a 去掉 `SKIP LOCKED` | 通过（符合预期） | 金额仍然正确；4 个副本并发投递 100 条，投递次数从 100 次增加到 123 次 |
| A14b 去掉时间下限 | 失败 ✔ | `TestClaimedReportIsRetriedAfterAReplicaDies`（投递进行中被重复领取） |

A14a 的结果与 7.1 的判断一致：正确性来自灵影的幂等，`SKIP LOCKED` 只减少重复投递。

### 10.4 本地联调

环境：灵影后端 `localhost:8080`，本网关 docker compose 跑在 `localhost:3001`，通过 `host.docker.internal:8080` 访问灵影；系统货币 CNY，汇率 7.3；上游模型 `deepseek-flash`（统一价已设置）。在灵影新建测试账户 `aigw-e2e-10191600`（余额 1 元，角色 admin），并创建个人 Key，同步到本网关后名称为 `ly:59e012f1-…`。

| 场景 | 结果 |
|---|---|
| 非流式 `/v1/chat/completions`，带 `X-Project-Id: proj-e2e` | 200。本网关记录费用 46997 nano CNY；灵影写入同 ID 的调用日志：`project_id=proj-e2e`、`protocol=openai`、`supplier=deepseek/deepseek-v4.1-flash`、37/5 token、扣费 `0.000047`；余额从 1 变为 0.999953 |
| 流式 `/v1/chat/completions` | 200。42004 nano 当时向上取整为 `0.000043`，与灵影扣费一致，但比本网关显示的 ¥0.000042 多 1 微元（10.6 已改为四舍五入）；没带请求头时 `project_id` 为空 |
| `/v1/messages`（Anthropic） | 200。灵影记录 `protocol=anthropic`，扣费 `0.000043` |
| 余额调成负数 | 402 `insufficient_balance` |
| 账户被禁用 | 403 `billing_account_unusable`，message 为 `Billing account cannot be used: disabled.` |
| 流式请求进行中停掉灵影 | 请求正常结束；上报留在 outbox，`attempts=1`，`last_error` 为连接被拒，约 20 秒后重试 |
| 灵影停机期间发新请求 | 503 `billing_unavailable` |
| 重启灵影 | 11 秒内自动补报，outbox 清空；灵影记录输出 301 token、扣费 `0.000645`，与本网关的 644999 nano 一致 |

### 10.5 测试

- `go vet ./...`、`gofmt` 无问题。
- 新增测试：`internal/integration/ly` 共 20 个（含 PostgreSQL 集成测试），`internal/gateway/billing_gate_test.go` 共 7 个，配置测试 1 个。
- 全量 `go test . ./internal/... ./tools/...`：除 8 个 `TestExternal*` 用例外全部通过。这 8 个失败的原因是共享测试库 `gpt_load` 被另一个分支 `feat/user-team-casdoor-support` 迁移过（`schema_migrations` 中有 `0020_teams`），与本次改动无关。在全新的数据库上重跑 `internal/control` 和 `internal/requestlog`，全部通过。
- 部署脚本：`shellcheck`、`bash -n`、`cfn-lint` 通过。

### 10.6 修复：本网关显示的费用与灵影扣费差 1 微元

- **现象**：请求 `d7b1dec2-…` 在本网关显示 ¥0.000481，灵影扣了 0.000482。
- **原因**：本网关记录的精确金额是 481004 nano CNY。请求日志显示时四舍五入到 6 位小数（`logMoney`：`(nano + 500) / 1000`），计价模块也都四舍五入（`scaleRoundHalfUp`）；而 4.4 让上报向上取整，所以只要尾数不为 0，灵影就会多扣 1 微元。
- **修复**：`formatYuan` 改为四舍五入，与显示一致，在微元粒度上两边金额相同。向上取整原本是为了不少收，但单次差额不到 0.5 微元，四舍五入也没有系统性偏差。
- **验证**：`TestFormatYuanRoundsHalfUpToSixDecimals` 新增 481004 → `0.000481`、499 → `0`、500 → `0.000001` 等边界；临时改回向上取整后，481004、499、1499 三个用例失败，与截图一致。
- **范围**：只影响修复后上报的请求；已入账的订单不变。灵影按结算单位再取整的规则不变：结算单位是 0.01 元时，两边仍会因为灵影的向上取整而不同，这是契约规定的。

### 10.7 灵影侧修复：预扣与补扣在订单列表中顺序颠倒

问题在灵影仓库，`internal/api/aigw_billing.go`。`aigwBookOrders` 让预扣和补扣共用同一个 `time.Now()`，而订单列表只按 `created_at` 倒序排列，时间相同时 MongoDB 按写入顺序返回，结果预扣显示在补扣上面。现在由 `aigwOrderTimes` 计算两个时间：预扣取请求开始时间（与调用日志的 `created_at` 相同），补扣或退款取入账时刻；上报方时钟偏快时，预扣时间改为补扣前 1 毫秒。灵影的真实 MongoDB e2e 测试已断言两者的先后；改回共用一个时间后，该测试失败。

### 10.8 第二轮对抗性审查（2026-10-19）

审查分两路：一个独立的审查代理对 diff 做对抗性审查，我从 R1–R5 出发逐个机制复核。审查只看主流程。

**采纳并修改**

| # | 发现 | 级别 | 修改 |
|---|---|---|---|
| 1 | `/v1/responses` 不强制要求 `model`。灵影的 key 不带 `model` 时会通过闸门，上报里 `model` 为空，灵影返回 400，这条上报被永久停放 | 中 | 闸门放行计费请求前，要求客户端模型不为空，否则返回 400 `invalid_protocol_request`，不上报。没有采用"模型为空时跳过闸门"，因为那等于开了一个绕过计费的口子 |
| 2 | recorder 同时持有 `ExternalBilling` 和 `*ly.Admission`，请求日志代码因此依赖 `ly` | 低 | 合并为一个回调 `recordBilling`，由闸门在准入时设置。`internal/gateway` 中只剩 `billing_gate.go` 导入 `ly` |
| 3 | `ly.Config` 与 `config.LingyingBillingConfig` 是同一个结构，容器里逐字段拷贝 | 低 | 删除 `ly.Config`，`ly.New` 直接接收 `config.LingyingBillingConfig` |
| 4 | `AdmitInput.Currency` 是完整的 `pricing.Currency`，但只用到币种代码 | 低 | 收窄为 `pricing.CurrencyCode` |
| 5 | outbox 的 `created_at_ms` 列及其检查约束没有需求支撑，只有运维 SQL 读它 | 低 | 从迁移 `0023` 中删除（迁移未合并、服务未上线）；运维 SQL 改为读上报内容里的 `started_at` |
| 6 | 没有测试覆盖"准入后上游失败也要上报"，也没有测试覆盖 `App` 启停投递循环 | 中 | 新增 `TestExternalBillingReportsRequestsThatFailAfterAdmission`、`TestExternalBillingRejectsABilledRequestWithoutAModel`、`TestAppRunsUsageReportsUntilStop` |

**不采纳**

- 合并 `route.ExternalBilling` 与端点的 `externalBilling`：网关处理请求时只拿得到解析后的路由，拿不到端点，无论放在哪里都要拷贝一次，合并减不掉东西。
- 删除 `Admission.KeyID`：它是上报字段，取自余额接口；删掉反而要从 key 名推算。
- 网关的 `ExternalBilling` 接口使用 `ly` 的类型：目前只有一个计费后端，另造一套网关自有类型只会多一层转换。出现第二个后端时再抽象。

**消融实验**（共 21 项，脚本逐一破坏、运行对应测试、还原）

A1–A13、A14b 的结果与 10.3 相同，都让对应测试失败。A8、A10 的匹配文本随重构更新后，重跑仍然失败。本轮新增：

| 被破坏的机制 | 结果 | 失败的测试 |
|---|---|---|
| A15 去掉"计费请求必须有模型" | 失败 ✔ | `TestExternalBillingRejectsABilledRequestWithoutAModel` |
| A16 只上报成功的请求 | 失败 ✔ | `TestExternalBillingReportsRequestsThatFailAfterAdmission` |
| A17 `App` 不启动投递循环 | 失败 ✔ | `TestAppRunsUsageReportsUntilStop` |
| A18 `Stop` 不等待投递循环退出 | 失败 ✔ | `TestAppRunsUsageReportsUntilStop`。第一版测试没发现这一项，因为假对象在取消后立刻退出；改为取消后先阻塞，再断言 `Stop` 在它退出前不返回 |
| A14a 去掉 `SKIP LOCKED` | 通过（符合预期） | 金额仍然正确，100 条上报用了 125 次投递 |

A18 保留的理由：不等待并不会丢上报（领取时已写好重试时间，灵影幂等），但 `Stop` 随后会关闭数据库，投递循环可能在数据库关闭后还在访问它；而且仓库对控制面运行时的约定也是等它退出。

### 10.9 吞吐改造：批量投递、积压告警、余额缓存（2026-10-21）

依据：[2026-10-20 压测报告](load-test-2026-10-20-report.md) 第 2.1、2.2 节，[批量上报调研](lingying-batch-usage-research.md)。逐条上报时，灵影对每条开一个 DocumentDB 事务，同一用户的事务在余额文档上冲突，每秒只能提交约 50 个，压测积压了 23.3 万条。

| 项 | 原设计 | 现在 |
|---|---|---|
| 上报接口 | `POST /internal/aigw/usage`，每条一次请求 | `POST /internal/aigw/usage/batch`，请求体 `{"reports": [payload…]}`，响应按条返回 `status` |
| 4.5 节奏 | 逐条领取、逐条上报 | 一条语句领取最多 `LINGYING_USAGE_DELIVERY_BATCH_SIZE`（默认 100，最大 100）条，一次请求发出，接受的用一条 `DELETE … = ANY($1)` 删除。每个副本同一时刻最多一个投递请求 |
| 请求超时与时间下限 | 20 秒 | 30 秒，大于灵影整批 25 秒的截止时间 |
| 结果处理 | 按 HTTP 状态 | 整体 200 时按每条的 `status` 处理，规则同 4.5 的表；响应里缺少的条目、整体非 200 和网络错误都按领取时写好的时间重试 |
| A17 余额缓存 | 不做 | 做：每个副本按凭证缓存"允许"的余额 5 秒，并发未命中合并为一次调用；拒绝类结果不缓存，`ProjectID` 取自当前请求 |
| 监控 | 无 | PG 主机上报 `LyUsageReportsPending`、`LyUsageReportOldestDueSeconds`，告警 `gateway-ly-usage-backlog`（超过 300 秒持续 5 分钟） |

上线顺序：灵影先上线批量接口，再发布网关；网关先于灵影上线时每批都会收到 404 并整批重试，积压告警会响，但不会丢失或重复记账。

**审查与消融（2026-10-21）**

按高内聚、低耦合审查后做了 4 处调整：

- "允许"的判定只保留一处：`allows(status, account)`，`Admit` 和余额缓存共用。原先缓存另写了一份，规则改动时两边容易不一致。
- 缓存的 map 和 singleflight 共用同一个键 `cacheKey(credential)`，不再一处用结构体、一处用字符串。
- `reportUsageBatch` 只返回 `([]batchResult, error)`，非 200 的状态码和响应体由客户端整理成错误；投递循环不再解读整体的 HTTP 状态码。
- `settle` 改名为 `warnIfUpdateFailed`，名字与它只记日志的行为一致。

消融：脚本逐项破坏机制，运行对应测试后还原。

| 被破坏的机制 | 结果 | 失败的测试 |
|---|---|---|
| B1 每轮只领 1 条 | 失败 ✔ | `TestDeliveryPostsDueReportsInOneRequest`、`TestDeliveryClaimsAtMostTheBatchSize` |
| B2 忽略批量上限 | 失败 ✔ | `TestDeliveryClaimsAtMostTheBatchSize` |
| B3 响应缺少的条目当作已接受 | 失败 ✔ | `TestDeliveryHandlesEachResultOfABatch` |
| B4 单条 400/422 不停放 | 失败 ✔ | `TestDeliveryHandlesEachResultOfABatch`、`TestDeliveryClassifiesLingyingResponses` |
| B5 整批失败时删除 | 失败 ✔ | `TestDeliveryRetriesAFailedBatch` |
| B6 接受的条目不删除 | 失败 ✔ | `TestDeliveryPostsDueReportsInOneRequest` |
| B7 领取时不加请求超时下限 | 失败 ✔ | `TestDeliveryRetriesTheSameReportUntilAccepted` 等 3 个 |
| B8 每次唤醒只投一批 | 失败 ✔ | `TestRunDeliversEveryDueBatchAtOnce`。第一轮没有测试发现，补了这个测试 |
| B9 不读缓存 | 失败 ✔ | `TestAdmitReusesAnAllowingBalance` |
| B10 拒绝结果也缓存 | 失败 ✔ | `TestAdmitDoesNotReuseARejection` |
| B11 不合并并发查询 | 失败 ✔ | `TestAdmitJoinsConcurrentLookups` |
| B12 缓存永不过期 | 失败 ✔ | `TestAdmitReusesAnAllowingBalance` |
| B13 缓存键不区分 key 与 user | 失败 ✔ | `TestAdmitCachesEachCredentialApart` |
| B14 等待中的请求不响应自己的 ctx | 失败 ✔ | `TestAdmitFailsClosed/timeout`。原先只是晚 3 秒通过，补了耗时断言 |
| B15 共享调用随发起者取消 | 失败 ✔ | `TestAdmitKeepsASharedLookupWhenItsCallerGivesUp`。第一轮没有测试发现，补了这个测试 |
| B16 批量大小不设上限 100 | 失败 ✔ | `TestLoadLingyingDeliveryBatchSize` |
| B17 指标脚本不容忍积压查询失败 | 失败 ✔ | `local-integration.sh` 新增的检查"积压表不存在时 PostgresUp=1 照常上报"。第一轮没有覆盖，补了这项检查 |

B8 保留的理由：不连续投递时，每个副本每秒最多只能投一批（100 条），跟不上压测的请求速度。B15 保留的理由：同一个 key 的并发请求共用一次余额查询，如果发起查询的客户端断开会取消这次查询，其他请求就会一起收到 503。

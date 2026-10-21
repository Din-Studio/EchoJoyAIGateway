# 调研：灵影用量上报改为批量接口（2026-10-21）

## 1. 结论

**可以做，而且应该做。** 用量上报的吞吐上限在灵影侧，不在网关。灵影现在每条上报一个 DocumentDB 事务，同一用户的事务会在余额文档上互相冲突。批量接口把同一用户的多条上报合并成一个事务、只更新一次余额，能同时减少事务数和冲突。

调研同时发现：`ly-billing-p0-throughput` 方案的任务 2（每个副本并发投递 32 条）**不能按原样实施**。它会让写冲突更严重，还会超过 DocumentDB 实例打开事务数的上限。第 6 节给出修订建议。

| 问题 | 结论 |
|---|---|
| 现在的上限在哪 | 灵影的 DocumentDB（1 × db.t3.medium）：每秒提交约 50 个事务，中止率 50–75%，CPU 63–97% |
| 网关加并发有没有用 | 没有用。4 个副本时每秒提交约 45 个，2 个副本时约 50 个，并发越高越差 |
| 批量接口能解决什么 | 同一用户一批 N 条只需 1 个事务、1 次余额更新，事务数降为原来的 1/N，同用户的冲突只剩副本之间 |
| 改动范围 | 灵影新增一个接口，原单条接口保留；网关把投递改为一批一次请求 |
| 工作量估计 | 灵影约 1–1.5 天，网关约 0.5 天（在 P0 方案的批量领取基础上） |

---

## 2. 现状：灵影如何记一条上报

代码：`lingying-gateway/internal/api/aigw_billing.go:303-415`，`internal/billing/task_money.go`。

| 步骤 | 位置 | 对 DocumentDB 的操作 |
|---|---|---|
| 读余额 | 事务外，`GetBalance` | 1 次读 |
| 查角色倍率 | 事务外，`aigwRoleRatio` | 1 次读 |
| hold(0) | 事务内，`HoldInSession`：读余额 → 插入 hold 订单（金额为 0，不更新余额） | 1 次读、1 次插入 |
| settle | 事务内，`ChargeInSession`：读余额 → 插入 settle 订单 → 用 CAS 更新余额文档 | 1 次读、1 次插入、1 次更新 |
| 请求日志 | 事务内，`Audit.InsertInSession` | 1 次插入 |
| 用量分析 | 事务外，`Usage.Record`，只是把记录放进 ClickHouse 队列，不阻塞 | 无 |

- **幂等**：订单上的 `(type, request_log_id)` 唯一索引（`internal/billing/repository.go:46-49`），以及 `request_logs._id = request_id`。
- **并发**：灵影原方案明确依赖"并发提交引起的冲突会表现为 WriteConflict，由驱动在事务内自动重试"（`plans/2026-10-19-aigw-text-billing.md:172`）。重试会把整个事务回调重新执行一遍。

结果是：同一用户的每条上报都要读三次、写一次同一个余额文档。并发上报越多，冲突和重做就越多。

---

## 3. 证据

### 3.1 DocumentDB 实测（2026-10-20，每 10 分钟汇总）

实例：`gateway-logs`，**db.t3.medium，只有 1 个实例**，引擎 8.0.1。21:30 之后事务数为 0，所以这些事务都来自用量上报。

| 时段 | 网关投递副本数 | 提交 / 秒 | 中止 / 秒 | 中止率 | CPU 平均 / 峰值 |
|---|---|---|---|---|---|
| 19:50–20:10（压测 + 积压） | 4 | ~45 | ~134 | ~75% | 80% / 97% |
| 20:20–21:10（积压消化） | 2 | ~50 | ~48 | ~50% | 63% / 75% |
| 21:30 之后 | — | 0 | 0 | — | 15% |

- 积压消化速度（网关侧测得每秒约 42 条）≈ 每秒提交的事务数，所以消化速度就是灵影的事务吞吐。
- 副本数从 4 降到 2，提交数反而略有上升：并发主要换来了被中止的事务。
- CPU 积分在这段时间从 553 降到 457，t3 实例在持续高负载下会消耗积分。

### 3.2 DocumentDB 的事务限制（官方文档，2026-10-21 查阅）

| 限制 | 值 | 来源 |
|---|---|---|
| 每个实例同时打开的事务数（8.0） | **t3/t4g.medium：50**；r5/r6g/r8g.large：200；xlarge：400 | [Quotas and limits](https://docs.aws.amazon.com/documentdb/latest/developerguide/limits.html#limits.instance) |
| 单个事务执行时长 | 1 分钟 | [Transactions → Limitations](https://docs.aws.amazon.com/documentdb/latest/developerguide/transactions.html#transactions-limitations) |
| 单个事务日志大小 | 32 MB | 同上 |
| 文档写锁超时 | 1 分钟，不可配置 | 同上 |
| 事务内 insert | 支持，但集合必须已经存在 | 同上 |
| 可重试写 | 8.0.2 起支持；当前 8.0.1 必须 `retryWrites=false` | [Retryable writes](https://docs.aws.amazon.com/documentdb/latest/developerguide/retryable-writes.html) |

文档**没有说明**的：同一文档的冲突写是立即失败还是阻塞等待；事务内 `insertMany` 遇到重复键时的行为。设计中避开了这两点（见 4.2 节）。

---

## 4. 方案

### 4.1 接口约定

新增 `POST /internal/aigw/usage/batch`，原 `/internal/aigw/usage` 保留不变。

**请求**：

```json
{ "reports": [ { 与单条接口相同的 aigwUsageRequest }, … ] }
```

- 每批最多 100 条，请求体上限 1 MB（单条接口是 64 KB）。
- 同一批里 `request_id` 不能重复。网关的 outbox 以 `request_id` 为主键，天然满足这一点。

**响应**：整体 200 时，`results` 按请求顺序逐条返回，网关按每条的 `status` 处理，规则和现在单条的返回码完全一致：

```json
{ "results": [
  { "request_id": "…", "status": 200, "charged": "0.02", "balance_after": "9.98", "bonus_balance_after": "0", "duplicate": false },
  { "request_id": "…", "status": 422, "error": "user has no billing account" },
  { "request_id": "…", "status": 503, "error": "role ratio unavailable" }
] }
```

| 单条 `status` | 含义 | 网关处理（不变） |
|---|---|---|
| 200 | 已记账，或重复上报（`duplicate=true`） | 删除 |
| 400 | 这一条参数非法 | 停放 |
| 422 | 用户没有计费账户 | 停放 |
| 500 / 503 | 这一条所在的用户组记账失败，可以重试 | 按领取时写好的时间重试 |

整体返回非 200（401、403、请求体非法、超时、5xx）时，网关把整批都当作可重试。

### 4.2 灵影侧处理

1. **逐条校验**：复用 `aigwUsageRequest.validate()`。不合法的条目标记为 400，不进入事务。
2. **按 `user_id` 分组**，组与组之间**顺序处理**。每个副本同一时刻最多只有一个打开的事务，不会触碰 50 个的上限。
3. **每组的处理**：
   1. 事务外 `GetBalance`：没有账户时，整组标记 422。
   2. 事务外 `aigwRoleRatio`：出错时整组标记 503。
   3. 逐条调用 `billing.SettleCost` 算出实际费用。
   4. 一个事务内完成：
      - **先去重**：用 `request_id` 列表查 `request_logs._id $in`，已存在的标记为重复，不再写入。先查后写就不会在事务内撞重复键，避开了 3.2 中文档没有说明的行为。
      - **余额只读一次**，在内存里按顺序对每条依次执行 `applyBonusFirst`，算出每张订单的 `BonusAmount`、`BalanceAfter`、`BonusBalanceAfter`。规则和现在逐条调用 `HoldInSession`、`ChargeInSession` 完全一样。
      - 一次 `InsertMany` 写入全部订单（每条一张 hold(0)，加一张 settle 或 refund(0)），一次 `InsertMany` 写入全部 `request_logs`。
      - 一次 CAS `UpdateOne` 写入最终余额。总费用为 0 时跳过这一步。
   5. 提交后，对每条已记账的记录调用 `Usage.Record`。对重复的条目，用一次 `$in` 查询读出原来的扣费金额。
4. **新增仓库方法**：`billing.Repository.BookUsageBatchInSession(sc, userID, items)`，和现有的 `*InSession` 方法放在一起，同样调用 `requireSession`。

**必须保持不变的结果**：同一组上报，批量记账和逐条记账产生的订单、request_logs、最终余额完全一致（订单 ID 和时间戳除外）。灵影侧用一个等价性测试证明这一点：同样的输入分别走两条路径，比较结果。

### 4.3 网关侧改动

在 `ly-billing-p0-throughput` 方案任务 2 的基础上：

- 保留：批量领取（`LIMIT $n`），以及成功后批量删除。
- 改为：一批只发**一次** `POST /internal/aigw/usage/batch`，不再批内并发；按 `results` 逐条执行删除、停放或重试。
- 批量上限默认改为 100，和灵影的单批上限一致，仍然可以用 `LINGYING_USAGE_DELIVERY_BATCH_SIZE` 调整。
- 投递循环仍然是每个副本一个，每个副本同一时刻最多一个请求在途。

### 4.4 预期效果（推算，需要压测验证）

| 场景 | 现在 | 批量后 |
|---|---|---|
| 单个热点用户，积压消化时每批 100 条 | 每条 1 个事务，约 8 次操作，中止率 50–75% | 每 100 条 1 个事务，约 7 次操作；冲突只发生在副本之间 |
| 灵影事务数 | 等于上报条数 | 约为 上报条数 ÷ 每批中该用户的条数 |
| 打开的事务数 | 等于在途投递数，P0 原方案下 2 副本 × 32 = 64，超过 50 | 不超过副本数 |

按现在每秒约 50 个事务的能力，即使每批只有 10 条属于同一用户，也能支撑每秒约 500 条。前提是单个事务的耗时不会随批量线性增长，这一点需要实测。

---

## 5. 风险与待验证

| 风险 | 处理 |
|---|---|
| 批内余额快照的计算和逐条记账不一致 | 4.2 的等价性测试，复用 `applyBonusFirst`，不另写一套规则 |
| 跨副本仍会冲突（两个副本同时提交同一用户的批次） | 驱动仍会自动重试；每次重试的代价变大，但次数大幅减少。压测时观察 `TransactionsAborted` |
| 提交结果不确定（错误 251，或者回复丢失） | 网关重试，灵影按 `request_id` 去重返回 `duplicate`，金额不会重复 |
| 单个事务过大或超过 1 分钟 | 每批上限 100 条，约 300 个文档、远小于 1 MB；压测时记录单批耗时 |
| DocumentDB 规格偏小（t3.medium，单实例，消耗 CPU 积分） | 和批量改造独立：短期可以升级到 r6g.large（打开事务上限 200，非突发型），需要你确认成本 |
| 灵影的 ClickHouse 队列满了会丢记录 | 行为和现在一致：丢了靠修复扫描补齐（`internal/analytics/sink.go:116-120`） |

上线前需要在测试环境实测：

1. 同一用户 100 条一批的事务耗时。
2. 2 个副本同时投递同一用户时的中止率。
3. 等价性测试在副本集 MongoDB 上通过（`MONGO_TEST_URI`，参照 `internal/api/aigw_billing_e2e_test.go:41`）。

---

## 6. 对 `ly-billing-p0-throughput` 方案的影响

| 方案中的项 | 调整 |
|---|---|
| 任务 1：`LINGYING_USAGE_DELIVERY_BATCH_SIZE` | 保留；默认值从 32 改为 100，含义改为"每次请求的条数" |
| 任务 2：批量领取 + 批内并发 + 批量删除 | **取消批内并发**，改为一批一次批量请求（4.3）；依赖灵影先上线批量接口 |
| 任务 3：余额缓存 | 不受影响 |
| 任务 4：积压指标和告警 | 不受影响，而且更有必要：灵影的容量是第一道限制 |
| Risks 中的"灵影单用户写冲突" | 由批量接口解决，改为验证项 |

**上线顺序**：

1. 灵影上线批量接口（新增接口，不影响现有单条接口）。
2. 网关上线批量投递。
3. 回滚时网关退回旧镜像，继续走单条接口。

在灵影的批量接口上线之前，网关**不要提高投递并发**，保持现在每个副本串行投递一条。

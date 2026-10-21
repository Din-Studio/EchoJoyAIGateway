# dev 架构不变量（上游适配参考）

dev 自 fork 点 `e90c1725` 起的设计约束。每条写成：**不变量** → 上游常见写法 → 适配方式。dev 是架构权威，上游只提供意图和行为。

符号名比行号稳定，引用以符号为准；若符号已改名，用 `grep` 重新定位后再判断，不要凭记忆套用。

## 0. 总纲

- 只有一种部署形态：N 个网关副本 + PostgreSQL + Redis（单实例 = 一副本集群）。没有单节点模式、没有本地文件、没有 SQLite/MySQL、没有 classic 前端、只发布容器镜像。
- 判断任何上游改动的第一问：**放到 3 个副本后面，行为还对吗？** 进程内状态、本地文件、每副本各跑一次的后台任务，都会在这里出错。

## 1. 集群共享状态（Redis）

**不变量**：影响跨请求正确性的运行时状态放 Redis，由 `internal/cluster/*` 的具体类型实现，消费方包里定义接口，经 `internal/container/container.go` 构造注入。旧包（`accessquota`、`ratelimit`、`affinity`）只剩纯类型与决策函数，不再持有状态。

现有共享状态（新增同类状态时照抄最近的那个）：

| 状态 | 实现 | Redis 失败时 |
|---|---|---|
| AccessKey RPM | `cluster.AccessKeyRPM` | fail-closed → 503 |
| AccessKey 费用额度 | `cluster.AccessQuota`（缺失键从 PG checkpoint 重建） | Check/Admit fail-closed；Complete 尽力 |
| 凭据健康（冷却/黑名单/失败数） | `cluster.CredentialHealth` | fail-open，回退本地 registry 变更 |
| 凭据并发 | `cluster.CredentialConcurrency`（ZSET 租约，Redis `TIME` 时钟，30s TTL + 续约） | fail-closed → 503 |
| 软亲和 | `cluster.Affinity` | fail-open，无偏好调度 |
| Responses 归属 | `cluster.ResponseBindings` | fail-closed |
| Models.dev 目录 | `cluster.CatalogStore` | 软失败，下轮重试 |
| 管理员登录失败锁定 | `cluster.AuthFailures` | 回退本地限流器 |
| 后台任务认领 | `cluster.JobLease` | 跳过本轮 |
| Token 刷新 | `cluster.RefreshLease` | fail-closed，放弃刷新 |

共同约定：
- 键用 `Client.Key(...)` 生成，同一实体的键带 hash tag（`{ak:<id>}`、`{cred:<id>}`）以落在同一 Cluster slot；原子操作写 Lua 脚本；都设 TTL。
- 数据面调用包 `context.WithTimeout(ctx, stateTimeout)`（200ms）；控制面用 `commandTimeout`；响应后结算用 `context.WithoutCancel` + 有界超时。
- 网关门控类调用都收 `ctx`、返回 `error`，失败统一走 `completeLimitStateFailure` → `limitStateFailureReason`：`ErrStaleRules` → 503 `configuration_changed`，其他 → 503 `cluster_state_unavailable` + `Retry-After: 1`，并记 `limit_state.unavailable` 日志。WebSocket 路径（`websocket_turn.go`）必须同样处理。
- 协作者在构造时必填：`gateway.SharedState` 每个字段必填，`app.AppParams.ClusterClient` 无 `optional` tag。没有 `nil` 分支、没有“没配 Redis 就走本地”。

允许保留在副本内的状态（仅限这些类别）：调度 ledger（刻意每副本独立，启动相位随机，`NewStaggeredCredentialRegistry` + `scheduler/fair.go` 中的 `ledger.Stagger`）、`health.StatsStore` 滚动窗口、配置快照/价格表/目录等**从 PG 或 Redis 派生的镜像**、本副本自身持有的租约/脏集合等记账。

上游 → 适配：
- 进程内 `map`/计数器/信号量/带正确性含义的 TTL 缓存（如上游 #792 的 `ratelimit.Concurrency` 用 `sync.Mutex` 计数）→ 新建 `internal/cluster/<name>.go`，按上表最相近的实现写 Redis 版；消费方定义接口；容器注入；需要时给 `gateway.SharedState` 加必填字段；显式选择 fail-closed / fail-open。语义里出现“全局上限”时，先确认是集群级还是每副本级（这属于产品决策，见 SKILL.md 决策门）。
- `if store == nil`、`optional:"true"`、单节点回退 → 删掉分支，参数改必填。
- 主路径直接改本地 registry 健康（`SetCooldown`/`IncrFailure`/`SetBlacklisted`）→ 先调 `SharedCredentialHealthStore`，本地变更只作为 Redis 失败回退（照 `handler.go` 中 `applySharedDecisionEffect` 的用法）。
- 旧的进程内费用额度运行时（`accessquota.Runtime`/`Reconcile`/`NewRuntime`）→ 规则语义放 `accessquota` 纯函数，状态转换放 `cluster.AccessQuota` 脚本。
- 共享/持久化调度进度 → 不做；ledger 保持每副本独立。

## 2. 控制面配置传播

**不变量**：改配置的写入都走 `Service.withControlTransaction`。它在同一个 PG 写事务里调用 `bumpClusterConfigRevision`（`_internal.cluster.config_revision`），提交后 `publishClusterConfigChange` 通过 Pub/Sub 广播；各副本的 `ClusterConfigSync` 收到事件或每 30s 轮询发现 revision 变大，就执行 `reloadCommittedConfigLocked`。reload 保留本地运行态：凭据健康、调度 ledger、额度计数、亲和。

- `withBookkeepingTransaction` 不 bump、不广播，只用于对端永不 reload 的状态（操作进度、sweep）。
- 其他包（如 `subscription`）提交配置用 committer 接口，由 `Service.CommitCredentialState` 实现，通过 `SetConfigCommitter` 注入（仅因依赖环而延迟绑定）。

上游 → 适配：
- 直接 `db.Transaction`/`db.Save` 写配置 → 改走 `withControlTransaction`；不要自己 `Publish`。
- 新增从 PG 配置派生的内存结构 → 必须在 `reloadCommittedConfigLocked`（或 `CompileInput`）里重建，并显式保留本地运行态。

## 3. 后台任务与租约

- 周期性、集群可见的任务（DB/Redis 维护、远程同步、清理）：每副本都起 ticker，函数体包在 `JobLease.RunOncePerPeriod(ctx, "<job>", period, fn)` 中，且必须可重复执行；从 `control.Runtime.Run` 启动以共享运行时 context 与关停。纯本地清理（如 `ExpireModelCooldowns`）可以每副本跑。
- 刷新/轮换一次性或会轮换的密钥：持 `RefreshLease.Acquire` 包住，提交走 `CommitCredentialState` 且以 `secret_version` 为条件，auth state 通过共享健康发布。
- 上游新增“每个进程启动一个 goroutine 去做 X”且 X 有外部副作用 → 套 `JobLease`。

## 4. 启动、配置与本地文件

- 必填环境变量：`DATABASE_DSN`（只接受 PostgreSQL URL）、`REDIS_ADDRS`、`AUTH_KEY`、`ENCRYPTION_KEY`。已删除：`DATA_DIR`、密钥文件、`authkey` 自动生成、`securefile`、`DB_FILE`、Windows 服务。
- 进程**从不写本地文件**；`.env` 只读。持久状态进 PG，临时共享状态进 Redis。
- 新环境变量只在 `config.Load` 里解析并校验（非法即报错），写进 `.env.example` 和三份 README 的环境变量表；`deploy/aws/gateway.yaml`、`docker-compose*.yml` 如需同步一并改。
- `/api/system/info` 只返回 `version`、`deployment.database`、`auth_key.source`、`encryption.source`。

上游 → 适配：写 `DATA_DIR`/缓存文件/checkpoint/密钥文件 → 改为 PG 或 Redis（目录缓存的先例：`cluster.CatalogStore`）；启动/关停时的 checkpoint 刷写钩子直接丢弃。上游 `*_checkpoint.go`（如 `requestlog/rpm_checkpoint.go`）一律按本节重新设计。

## 5. 存储与迁移（PostgreSQL only）

**迁移注册表**（`internal/storage/migration.go`）：
- ID 形如 `NNNN_snake_name`，注册表第 N 项必须是编号 N（连续），`schema_migrations` 账本按 ID 排序逐项比对——已应用的迁移不能重排、不能改号。
- 条目只有 `{ID, Up, Validate[, ValidateCurrent]}`，没有 `ValidateRecoverable` 字段、没有方言参数。`Up` 以 `return ValidateNNNN(db)` 结尾；每步用 `HasColumn`/`HasTable`/`HasConstraint`/`HasIndex` 保护以保证幂等。
- 每次启动对所有已应用迁移跑 `ValidateCurrent`（若有）否则 `Validate`。后续迁移改变了早期 Validate 检查的东西 → 给早期条目加 `ValidateCurrent`（先例：`ValidateCurrent0001`、`ValidateCurrent0002`）。
- 每个迁移文件定义自己的冻结结构体（`xxxNNNN` + `TableName()`），不引用运行时 `models.*`。加列可复用 `columnSet0021`/`addColumnSet0021`。
- 项目规则（未上线）：**删表/删列直接改创建它的迁移**（通常是 `0001`），不写删除迁移；**加列/加表写新迁移**。

**移植上游迁移的配方**：
1. 编号：追加到 dev 下一个空号，保持上游相对顺序；上游编号 → dev 编号写进台账（`docs/upstream-merge-ledger.md`），后续上游提交再改该迁移时按台账找到 dev 文件。
2. 改名：文件名、`IDNNNN`/`UpNNNN`/`ValidateNNNN` 及所有 `...NNNN` 类型和常量全部换成 dev 编号（同包命名空间，撞号会编译失败）；更新注册表和 `storage/migration_test.go` 的 `wantIDs`；移植来的测试里按索引切片注册表（`migrations[:N]`）的地方重新对齐。
3. 剥离：`ValidateRecoverable` 注册字段（`Up` 需要时保留为私有 helper）、`switch db.Dialector.Name()`、`unsupported driver` 守卫、MySQL `longtext`/`DROP CHECK`、SQLite `logger.Discard` 与 `#building` 标记、索引查询里的 mysql/sqlite 分支。
4. 对齐 dev 列：0021 之后金额列是 `*_nano_usd`（USD）+ `*_nano`（冻结币种）+ `currency`；`limit_nano_usd`/`used_nano_usd` 已改名为 `limit_nano`/`used_nano`；`usage_stats` 与 `auto_decision_usage_stats` 的主键/唯一键都含 `currency`。上游“复制表重建”式迁移改为原地加列 + 重建含 `currency` 的主键，不要从 USD-only 列复制。
5. 上游的 `migration.go` 框架改动（迁移检查、事务模式标志等）多为方言相关，dev 不跟；确有价值时只移植 PostgreSQL 部分。

**事务与并发写**：
- `dbtx.Run` 写事务固定 `READ COMMITTED`，报表读用 `ReadSnapshot`（`REPEATABLE READ`）。
- 共享计数一律在库内自增：`incrementAssignments`（溢出写 -1 哨兵，依赖列上的 `CHECK (col >= 0)`）；不在 Go 里读-加-写。绝对值写入只允许带版本守卫（如 `snapshot_version < ?`）或单写者键。
- upsert 前对键排序（`sortedUsageStatKeys`），保证多实例行锁顺序一致。
- 幂等操作提交失败时按 `idempotency_key` 重放已提交结果；阶段推进是 CAS，对端已推进更远时采纳。

上游 → 适配：`sqlitetest`/`:memory:`/`gorm.io/driver/sqlite` → `pgtest`；`AssignmentColumns` 写共享计数 → `incrementAssignments` + CHECK；未排序的 upsert 循环 → 先排序。`go.mod` 里 `glebarez/sqlite`、`gorm.io/driver/mysql` 及其间接依赖不得回来（`go mod tidy` 后确认）。

## 6. 请求日志与用量聚合

- `usage_aggregation_journal` 已从 0001 删除。`WriteBatch` 在一个写事务里插日志、auto-decision 用量、attempts，然后直接从新行聚合 `usage_stats` 与 `credential_attempt_stats`。没有 journal 的暂存、重放、保留清理。
- 小时报表读 `usage_stats` UNION `auto_decision_usage_stats`；5 分钟报表直接读 `request_logs`（`usageRequestLogProjection`）。新增费用/聚合维度必须同时加到统计表、日志投影、UNION 两侧列表。
- 重复提交的日志批次按 ID 去重（`prepareNewRequestLogRows`）。

上游 → 适配：任何涉及 journal 的 hunk（worker、retention、`models.UsageAggregationJournal`）丢弃；上游 worker 新增的写入步骤（如审计用量、请求体记录）改写到 dev 的 `writeRequestLogBatch` 流程里。

## 7. 计费与币种

- 系统币种 `currency`（USD/CNY）+ `exchange_rate`（1 USD = X；USD 时为 null）。请求准入时冻结币种和汇率（`snapshot.Settings.Currency` → `RequestEvent.Currency`），日志同时存 USD 金额和本币金额，之后改汇率不重写历史。
- 命名：`*_nano_usd` 永远是 USD；`*_nano` 是冻结币种金额。API 金额字段不带 `_usd` 后缀，日志与用量响应同时返回 USD 值用于对账。
- 计费价 vs 上游成本：`quoteFrozenAttempt` 在同一冻结价格表上算两次。**收费**按客户端模型统一价（`client_model_prices`）× AccessKey 倍率，写 `estimated_cost_*`，额度和用量都用它；无统一价即未定价，不回退上游价。**上游成本**按渠道模型价 × 分组倍率，写 `upstream_cost_*`，只给管理员看（`sanitizeAccessKeyRequestLog` 对 AccessKey 清零）。收据 v6 = 上游、v7 = 客户端模型。
- 价格表存 USD，API 进出按系统币种换算；价格修改在 `withControlTransaction` 里保存并重新发布价格表。
- 费用额度规则以规则自身币种计价（`LimitNano` + `Currency`）；切换系统币种时在设置事务内按 旧汇率 → USD → 新汇率 转换并重置状态。

上游 → 适配：
- 新的成本/价格/日志金额列：保留 USD 列，加 `*_nano` 本币孪生列（`CHECK >= 0`），映射时用 `event.Currency.Settle(...)` 填；加进 `columnAmounts()` 和两侧报表投影。
- 新聚合表：加 `currency` 列，并进主键/唯一索引和 upsert `Columns`。
- 新的网关付出成本：照 `upstream_cost_*` 处理，仅管理员可见，扩展 `sanitizeAccessKeyRequestLog`。
- 按 USD 比较或汇总费用额度（如上游按 `EstimatedCostNanoUSD` 求和）→ 改用 `EstimatedCostNano` + 规则币种。
- 新收据类型：在 `pricing/types.go` 加 schema 版本，并给 `ValidateReceipt` 加分支。

## 8. 网关数据面与渠道

- 凭据并发：`concurrency.Acquire` 放在候选凭据选定之后，`Forward` 返回后立即释放；满的凭据跳过且不消耗转发次数；全满返回 429 `credential_concurrency_limited`。Bifrost worker 池（`execution/bifrost/runtime.go`）调大到不成为吞吐瓶颈——上游改回小池子不跟。
- `ResponseScrub` 由渠道声明（`spec.Definition.ResponseScrub`），原生直通、原生流、Embeddings 三个出口经 `scrubHeaders` + `nativeResponseRewrite` 应用；`bifrost` 里不写渠道特判。SSE 改写统一走 `responsealias.TransformSSE`（别名与 scrub 共用分帧）。
- 性能：redactor 正则在包级编译一次；SSE 不对每个分片做完整 JSON 解码。上游在 `New()` 或每请求里 `regexp.MustCompile` → 提到包级。
- `/v1/models` 的 `owned_by` = `gateway_name` 设置（`modelOwner(snapshot)`），不硬编码 `gpt-load`。
- Vercel AI Gateway 渠道是 dev 独有，上游同名改动以 dev 为准。

上游 → 适配：
- 新渠道预设：API key 渠道声明 `CredentialInput: "entries"`；会泄露网关/计费元数据的声明 `ResponseScrub`；在 `builtins.go` 注册，补注册表与契约测试，三份 README 同步。
- 凭据导入：dev 的契约是 `credentials: [{fields, concurrency_limit}]`，每批 1–500 条，按指纹去重，同一凭据给出两个不同上限时报 `conflicting_value`。上游的文本块（`batch_text`）、字符串列表导入 → 改成 entries。
- 改原生直通/流路径时保留 `scrubHeaders` 与 `nativeResponseRewrite`；改 `rewriteSSEEvent`/`aliasRewriter` → 在 `TransformSSE` 上重新表达。

## 9. 控制面 API

- 路由在 `internal/control/http_routes.go` 用 `controlRoute(...)` 声明；修改类路由经 `s.auditMutation(newMutationDescriptor(...))`；带凭据的修改使用 `Idempotency-Key`。`container/route_contract_test.go`、`app/http_routes_test.go` 钉住路由表，需同步。
- `access_key` 角色只能 GET 白名单路径（见 `docs/openapi.yaml` 头部）；新暴露给 AccessKey 的 GET 需扩白名单并更新 openapi 文本。
- `docs/openapi.yaml` 是手工维护的契约，覆盖 AccessKey、币种、模型、模型价格、客户端模型价、分组模型与发现。**没有测试校验漂移**：范围内的端点改动必须在同一次移植中更新它。
- `access_keys` 元数据读取走单一列清单 `accessKeyMetadataColumns` + `accessKeyMetadataRow`；上游新增 `access_keys` 列时扩这份清单、行结构、映射和创建/更新路径，而不是内联新的 `Select`。
- 新设置项走 `state/runtime_settings.go`（键常量、`IsRuntimeSettingKey`、解析与默认值）+ 设置页；涉及金额的设置走币种换算。

## 10. 前端（Vue，modern only）

- 只有 `web/src/frontends/modern/`；classic、界面切换器、`gpt-load.frontend.v2` 偏好、Tailwind、`@internationalized/date` 都已删除。页面路由清单只有 `internal/webui/page_routes.json`。
- i18n 三语（`zh-CN`、`en-US`、`ja-JP`），按 feature 拆分；新文案三语齐全。
- 业务页面用 `components/ui` 原语（`check:styles` 强制）。没有前端单测；门禁是 lint、format、type-check、build。新增 `execution.Operation` 需同步到 `modern/api/logs.ts` 的 `logOperations`（`ui_api_contract_test.go`）。

上游 → 适配：`web/src/frontends/classic/**`、classic 测试、切换器、`modern_page_routes.json` → 丢弃（含 git 无冲突新增的 classic 文件）；功能有价值时在 modern 中实现。已删依赖的升级 → 丢弃。上游 vitest 等前端测试 → 不引入。

## 11. 构建、测试、部署

- 验证入口 `make check`（gofmt、`go mod tidy -diff`、`go vet`、web lint/format/build、`go build`、全量 `go test`、`git diff --check`）；依赖 `make test-deps` 起的 PG（55432）和 Redis（56379）。本地验证即合并门禁。
- 测试用 `pgtest`：缺 `GPT_LOAD_DATABASE_TEST_DSN`/`GPT_LOAD_REDIS_TEST_ADDR` 时失败而非跳过；每个包 `TestMain` 调 `pgtest.Run`；共享外部库的测试不加 `t.Parallel()`。
- `.github/workflows/*.yml`、`release-assets.txt` 被 `internal/webui/workflow*_test.go` 当契约钉住（action SHA、runner、只构建 linux 镜像二进制等）；`docker-compose*.yml` 被 `container_contract_test.go` 钉住；Dockerfile 的 OpenSSL pin 有测试。
- 只发布容器镜像。Windows/安装包/原生多平台二进制/Render 部署都已删除。`deploy/aws/**`、`docs/aws-*`、`CLAUDE.md`、`.claude/**`、`docs/TODO/**` 是 dev 独有，不被上游覆盖。

上游 → 适配：
- 引入 Windows/原生二进制/Render/`DATA_DIR`/卷挂载/已删许可证（Inno、MPL）→ 丢弃。
- action SHA、dependabot 类 workflow 升级 → 可以跟，但同步更新 `workflow_test.go` 里钉住的常量。
- Dockerfile 只跟版本 pin 升级，同步 pin 测试。
- 自更新二进制、版本检查指向上游 GitHub release 之类的分发逻辑 → 与“只发布容器镜像”冲突，到达时走决策门。
- README 改动：三份 README（`README.md`、`README_CN.md`、`README_JP.md`）同步；重新引入密钥文件、原生二进制安装说明的段落丢弃。

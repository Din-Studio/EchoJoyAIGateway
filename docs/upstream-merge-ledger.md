# 上游合并台账

`/upstream-merge` 跨提交共享的状态。每个上游提交合并前读一遍，合并时在同一个 merge commit 里更新。

- 上游：`upstream` = tbphp/gpt-load，`main` 分支。fork 点为 `e90c1725`。
- 已经合到哪个上游提交，以 `git merge-base HEAD upstream/main` 为准，台账不另记。
- 每个提交的适配和丢弃明细写在 merge commit 的说明里，用 `git log --merges --grep '^merge(upstream)'` 查。台账只记跨提交仍然有效的内容。

## 迁移编号映射

dev 迁移注册表要求编号连续，dev 已用到 `0024`。上游从 `0019` 起和 dev 撞号，必须重新编号，见 ARCHITECTURE.md §5。

| 上游迁移 | dev 迁移 | 来源提交 | 备注 |
|---|---|---|---|
| `0019_auto_decision_attribution` | `0023_auto_decision_attribution` | 8eceed2a #703 | 复制表重建改为原地加 `group_id`/`channel_id`/`credential_id`，主键保留 `currency` |
| `0020_client_model_overrides` | `0024_client_model_overrides` | 1c280a27 #706 | 去掉 MySQL `longtext`、方言分支和 `ValidateRecoverable` |

跨分支冲突：`main` 上有 dev 没有的 `0023_ly_usage_reports`（灵影计费，dev #27），和这里的 `0023` 撞号，`0024` 在同步时随之后移。共享测试库跑过 main 系分支后，本分支的 `TestExternal*` 会报 `non-contiguous migration`，需要换一个干净库。dev 同步 main 时必须统一重新编号：保留已部署那一侧的编号，另一侧整体后移，台账的映射表一起改。

## 长期决定

前面某次合并已经定下、后续提交直接沿用的映射。

| 上游概念 | dev 的处理 | 定于 |
|---|---|---|
| classic 前端（`web/src/frontends/classic/**`） | 一律丢弃，包括无冲突新增的文件；有价值的功能在 modern 里实现 | dev #13 |
| SQLite/MySQL 方言分支、`sqlitetest` | 只保留 PostgreSQL 分支，测试改用 `pgtest` | dev #9 |
| `usage_aggregation_journal` 相关代码 | 丢弃 | dev #11 |
| 运行时 checkpoint 文件（`*_checkpoint.go`、`DATA_DIR`） | 丢弃，持久状态进 PG，共享状态进 Redis | dev #8 |
| 凭据文本块导入（`batch_text`） | 改成 `credentials: [{fields, concurrency_limit}]` | dev #13 |
| 自动模型决策调用（auto-model decisions） | 走渠道/分组/凭据路由，占用 `cluster.CredentialConcurrency` 槽并跳过饱和凭据；共享状态失败时决策按尽力而为处理，落到 fallback 预设，不返回 503 | 8eceed2a #703 |
| 决策费用 | 按决策模型的客户端模型统一价（`client_model_prices`）计价，不用渠道价，也不设单独的决策价格设置 | 8eceed2a #703 |
| 决策的上游成本（`Decision.ProviderCostUSD`） | 只给管理员看，`sanitizeAccessKeyRequestLog` 清零 | 8eceed2a #703 |
| 共享别名的模型轮询游标（`SchedulingLedger.ModelCursors`） | 和调度 ledger 一样留在副本内，不持久化、不放 Redis；新建游标时用 `staggerModelOrder` 按副本随机旋转初始顺序，防止各副本先选中同一个上游模型。上游对游标 checkpoint 的改动一律丢弃 | cca2316a #717 |
| 上游版本检查（`internal/releasecheck`、前端检查更新提示） | fork 不跟进：它查的是 tbphp/gpt-load 的 GitHub release，和 fork 的容器镜像发布无关。后续改版本检查的上游提交（如 dec6dc7f #728）整体用 `-s ours` 跳过；dev 现有的 releasecheck 是删掉还是改成查 fork 自己的版本，另开任务决定 | 68681794 #714 |

## 已知重叠点

下面是事前预判，没有经过验证。到达对应提交时仍按 SKILL.md 第 2 步重新分诊；处理完就把条目删掉，长期有效的结论移到“长期决定”。

| 上游提交 | 预判 |
|---|---|
| 7b57fa94 #727 JEV 审计（U0021） | `request_audit_usages`、`audit_cost_nano_usd` 只有 USD，要补本币孪生列和 `currency`；审计成本对 AccessKey 是否可见要确认（§7） |
| dd8fa169 #739 RPM 统计（U0022） | `requestlog/rpm_checkpoint.go` 按 session 做绝对值 upsert，是进程内统计；改成集群语义，可以复用 `cluster.AccessKeyRPM`（§1、§4、§5） |
| 84896129 #792 数据面并发（U0023） | 进程内 `ratelimit.Concurrency`（全局、AccessKey、分组三级）。要改成 Redis 租约，照 `cluster.CredentialConcurrency`；“全局上限”是集群级还是副本级要走决策门（§1） |
| addd6987 #804（U0024）+ 3cb66939 #811（U0027） | U0024 加列，U0027 删掉同样的列。#804 照常重编号移植。到 #811 时按“删列改创建它的迁移”的规则，把 #804 移植来的迁移改成不加这两列，编号保留不删，不写删除迁移（§5） |
| 02e1879e #805 代理管理（U0025） | 控制面写入要走 `withControlTransaction`；代理目录是 PG 配置，要在 reload 里重建（§2） |
| 36d1b976 #812 客户端 IP（U0028） | 网关在 ALB 后面，要核对可信代理头的配置；新环境变量走 `config.Load`（§4） |
| b88b395c #825 分组优先级（U0029） | 改了 `scheduler/fair.go`，要保留 `ledger.Stagger`（§1、§8） |
| ab655129 #851 请求体记录（U0030） | 写入要并进 dev 的 `writeRequestLogBatch`；`access_keys` 新列要加进 `accessKeyMetadataColumns`（§6、§9） |
| 3745fbab #842 CLI update 命令 | 二进制自更新，和“只发布容器镜像”冲突，倾向整体丢弃 `update` 命令，要走决策门（§11） |
| 36b40124 #752、b8cadb40 #889 发布验收 | `scripts/release*`、Playwright、Makefile 目标，和 dev 的发布方式不一定一致，要核对 workflow 契约测试（§11） |

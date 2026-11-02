# 多个 upstream commit 合并后的核心流程梳理指南

## 1. 用途与边界

本文用于指导 Agent 审查**一批上游提交合并后的整体代码**。目标不是逐文件复述 diff，而是确认多个改动组合后，核心流程仍然完整，模块职责没有重复，集群约束没有丢失。

- 源码基线：`dev`，`2a37ea2b80d0262f1404b39af5806c60b192745b`。
- 编写日期：2026-11-02。
- 分析方法：读取实现、生产依赖装配和代表性测试。
- 验证边界：本文描述源码事实，不是该版本的运行验收报告。编写本文时没有运行业务测试或部署。
- 适用分支：以 dev 核心为主。审查 main 时，另外纳入灵影调用链和独立迁移线，不假定本文覆盖这些扩展。

配套入口：

- [上游合并流程](../.agents/skills/upstream-merge/SKILL.md)：决定如何合并单个提交。
- [架构不变量](../.agents/skills/upstream-merge/ARCHITECTURE.md)：决定哪些上游行为必须适配或丢弃。
- [dev 同步到 main](dev-to-main-merge-guide.md)：决定分支间如何同步。
- [API 契约](openapi.yaml)：维护其覆盖范围内的外部字段与权限。

本文不替代上述文件。**架构约束说明预期，当前代码说明实际，测试结果说明本轮验证范围。三者不得互相冒充。**

默认只读分析。修改代码、提交、推送、部署、重启、恢复数据库、使用真实付费上游，必须取得本次任务对应的授权。不得沿用历史授权。

### 1.1 术语

| 术语 | 本文含义 |
| --- | --- |
| 逻辑请求 | 客户端的一次 HTTP 请求，或 WS 的一次业务轮次 |
| attempt | 网关对一个指定候选的执行尝试，不等于整个逻辑请求 |
| 控制面 | 修改配置、凭据、价格及查询管理数据的 `/api` 服务 |
| 数据面 | 接收客户端协议、准入、调度、转发和结算的网关服务 |
| 配置快照 | 从数据库编译后发布到本进程的只读配置视图 |
| 共享状态 | 影响多个副本共同决策的 Redis 状态 |
| 下游提交 | 网关向客户端实际写出响应，不是收到上游响应头 |
| 收费 | 按客户端模型统一价计算的费用，供 AccessKey 额度使用 |
| 上游成本 | 按渠道模型价计算的成本，不是客户端收费 |
| 源码确认 | 通过当前实现和调用方确认，不代表测试通过 |

## 2. Agent 的执行顺序

### 2.1 固定审查范围

输入至少包括：合并前基线 `BASE`、合并后目标 `TARGET`、上游提交集合、已有适配决定。缺少基线且无法从明确记录确定时，先询问用户。不得把 `merge-base HEAD upstream/main` 当作合并前 dev 基线。

先运行只读命令。下面的占位值必须替换为真实提交号：

```bash
BASE='<合并前 dev 提交>'
TARGET='<本轮目标提交>'
git status --short --branch
git rev-parse "$BASE" "$TARGET"
git merge-base --is-ancestor "$BASE" "$TARGET"
git diff --name-status "$BASE" "$TARGET"
git diff --stat "$BASE" "$TARGET"
git log --first-parent --oneline "$BASE..$TARGET"
git log --merges --grep='^merge(upstream)' --format='%h %s%n%b' "$BASE..$TARGET"
```

- 记录目标源码 tree。后续修改代码后，验证结果必须绑定新 tree。
- 工作区不干净时，单独记录 staged、unstaged、untracked。不得把用户改动归到上游批次。
- 确认阅读的工作树对应 `TARGET`。不对应时，读取目标版本或使用独立工作树；不得擅自切换或覆盖用户工作区。
- 查看每个上游提交的意图和适配记录。不要只看 PR 标题或自动合并成功状态。
- 对真实的上游 merge `M`，使用 `git diff M^1 M` 查看该步在 fork 上落地的内容。先核实父提交，不能对所有 PR merge 一律假定第二父提交是 upstream。
- 净差异为零不代表可以忽略中间迁移。结合部署记录判断中间版本是否接触过数据库。

生成追踪表：

| 上游提交 | 上游意图 | fork 落地提交 | 保留／适配／丢弃及依据 | 任务模块 | 跨模块影响 | 验证证据 |
| --- | --- | --- | --- | --- | --- | --- |
| `<sha>` | `<行为>` | `<sha>` | `<决定>` | `Mxx` | `<调用方/消费者>` | `<路径或未验证>` |

### 2.2 按模块阅读，而不是按提交分工

1. 阅读架构不变量全文。
2. 用第 4 节分配任务模块。
3. 从生产装配确认真实实现。
4. 沿入口读取到外部副作用。
5. 回读返回值、错误和释放路径。
6. 检查直接调用方及最终消费者。
7. 用第 5 节检查跨模块组合。
8. 按第 6 节验证。
9. 用第 7 节输出报告。

可以并行分工，但每个模块必须有一个负责人。并行 Agent 使用同一个 `BASE`、`TARGET` 和适配记录。最后由一个 Agent 汇总跨模块结论，不能直接拼接多个“PASS”。

每个模块必须输出：

- 真实入口和调用链。
- 输入、输出及状态归属。
- 正常、拒绝、失败、取消的分支。
- 本批提交的组合影响。
- 有实际调用路径的发现。
- 测试证据及未验证范围。

### 2.3 机械扫描的限制

当前扫描器是 `.agents/skills/upstream-merge/audit.sh`。它固定扫描 `git diff --cached HEAD`。

- 合并尚未提交时，可以运行 `bash .agents/skills/upstream-merge/audit.sh` 辅助检查。
- 合并已经提交时，暂存区为空会漏掉整批改动。不得把“无命中”报告为批次审计通过。
- 已提交批次必须针对 `git diff "$BASE" "$TARGET"` 逐项检查扫描器中的规则。
- 不得为了复用扫描器而 reset 分支或改写真实暂存区。
- 命中 `map`、锁或 goroutine 只是线索。必须判断其状态和生命周期，不能机械改成 Redis 或 JobLease。

## 3. 核心流程总图

### 3.1 启动与配置

```text
main.runServer
  → buildManagedRuntime → container.BuildContainer
  → config.Load / PostgreSQL / Redis / 服务装配
  → App.Start
      → 迁移 → 初始状态 → Loader.Load
      → 未完成控制操作恢复 → 共享健康恢复
      → 执行运行时 → 监听器 → 日志工作器 → HTTP 与控制后台循环

管理写入
  → 认证/权限/审计/适用的幂等入口
  → 控制事务：业务数据 + config_revision
  → 提交 → 本地发布 + Redis 通知
  → 其他副本：通知或 PG 版本轮询
  → reloadCommittedConfigLocked → 价格/凭据/配置快照
```

### 3.2 普通 HTTP 转发

```text
路由与认证
  → 创建 recorder，冻结币种与 AccessKey 倍率
  → 请求并发槽（全局 + AccessKey）
  → 费用额度 Check → RPM → 读正文/协议 Inspect
  → Responses 归属查询（如有）→ 自动选模（如有）
  → 冻结候选身份 → 亲和 → scheduler.Iterator
  → 对候选执行：
      参数覆盖 → 脱敏 → 凭据/代理准备 → 护栏
      → 分组槽 → 凭据槽 → 费用额度 Admit（尚未取得 ticket 时）
      → 冻结 attempt 报价 → 单次执行 → 响应交付
      → 释放凭据槽和分组槽 → 重试判定/完成
  → 额度 Complete → 日志 Emit → 释放请求槽
  → 日志工作器异步入库与聚合
```

这里的顺序不是所有端点的统一模板：

- models 在准入后生成本地模型列表，不执行普通候选转发。
- usage 有独立入口分支。
- WS 在普通 HTTP 准入前分流，每个业务轮次重新准入。
- 自动选模或护栏辅助调用可以先取得额度 ticket。
- Responses cancel 的具体豁免见 M03 和第 8 节，不得按注释推断所有门控都豁免。

### 3.3 执行与结果消费

```text
channel 声明 + state 编译 + scheduler 选择
  → execution.AttemptSpec
  → provideradapter.Registry
  → Bifrost 或 CPA（由 ProviderKind 绑定）
  → 上游结果/事件/用量证据
  → gateway 交付、重试判定、归属与计时
  → recorder 归属用量、双报价、冻结币种
  → telemetry → requestlog → control 查询/权限裁剪
  → modern API 解析 → 页面展示
```

## 4. 任务模块

| 模块 | 任务 | 重点协作模块 |
| --- | --- | --- |
| M01 | 启动、装配和退出 | M02、M10、M11 |
| M02 | 控制写入、幂等和配置传播 | M03、M08、M09、M12 |
| M03 | 认证、准入和共享状态 | M04、M06、M07、M08 |
| M04 | 渠道、路由、调度和重试 | M05、M07、M09 |
| M05 | 协议执行、转换和流式交付 | M06、M07、M09、M10 |
| M06 | 自动选模、护栏和脱敏 | M03、M04、M08、M09 |
| M07 | Responses 归属和 WS | M03、M04、M05、M10 |
| M08 | 凭据授权、刷新和健康 | M02、M03、M04、M11 |
| M09 | 用量、收费、上游成本和币种 | M02、M03、M10、M12 |
| M10 | 请求日志、输出计时和报表 | M05、M09、M11、M12 |
| M11 | 迁移、后台任务和目录同步 | M01、M02、M08、M10 |
| M12 | 管理 API、权限和现代前端 | M02、M09、M10、M13 |
| M13 | 构建、验证和发布契约 | 所有受影响模块 |

### M01. 启动、装配和退出

**阅读入口**：`main.go` 的 `runServer`；`runtime.go`；`internal/container/container.go` 的 `BuildContainer`；`internal/app/app.go` 的 `Start`、`Stop`；`internal/platform/config/config.go`；`internal/app/readiness.go`。

**实际流程**：环境配置进入容器；容器构造 PG、Redis、服务与适配器。`App.Start` 在服务接收请求前执行迁移、初始化、配置加载和控制操作恢复。共享健康恢复失败只警告，不与其他启动失败混为一类。退出时取消活动请求和控制循环，启动执行器关闭，排空 HTTP 和日志，再关闭基础连接。

**审查要求**：

- 必填依赖保持 PG、Redis、`AUTH_KEY`、`ENCRYPTION_KEY`。不得恢复本地文件密钥和单节点旁路。
- 新接口必须追到生产注入。只在测试构造器中连通不算落地。
- 新资源必须有启动失败清理和退出归属。退出超时不得无限等待。
- 配置或凭据替换后，旧执行资源必须按现有生命周期退役，不能直接关闭仍被请求使用的资源。
- `/health` 的依赖 Ping 不证明配置收敛或 Redis Cluster 槽位完整。

**测试入口**：`runtime_test.go`、`internal/app/app_test.go`、`internal/container/route_contract_test.go`。

### M02. 控制写入、幂等和配置传播

**阅读入口**：`internal/control/http_routes.go`；`service.go` 的 `writeConfig`、`withControlTransaction`、`runControlTransaction`；`idempotency_operation.go`；`operation_recovery.go`；`cluster_revision.go`；`cluster_config_sync.go`；`cluster_reload.go`；`internal/state/loader/loader.go`、`internal/state/snapshot.go`、`internal/state/manager.go`。本节省略路径的文件均位于 `internal/control/`。

**实际流程**：控制事务修改数据并递增数据库配置版本。提交后执行本地发布并广播通知。其他副本通过通知或每 30 秒轮询追赶，重新加载价格、凭据及配置。重载失败不推进已应用的数据库版本。

适用的幂等操作将业务数据与操作记录一起提交。以创建分组为例，恢复链是 `db_committed → prices_published → registry_applied → snapshot_published → completed`。其他操作有自己的阶段，不能套用同一阶段表。

**审查要求**：

- 配置写入走控制事务。进度记账才使用 `withBookkeepingTransaction`。
- 数据库提交后发布失败，不得向客户端解释成业务事务回滚。
- 同一个幂等键重放不得再次创建业务数据。阶段推进必须接受同伴已推进的结果。
- Pub/Sub 丢失时仍有 PG 版本补偿。重载必须保留同身份凭据的健康状态与本地调度进度。
- 数据库 `config_revision` 与进程内 `ConfigSnapshot.Revision` 不是同一个版本号。
- 价格、凭据注册表和配置快照是分步发布，不得描述为一次整体原子替换。检查失败后的重试收敛。

**测试入口**：`internal/control/cluster_config_sync_test.go`、`internal/control/operation_recovery_test.go`。

### M03. 认证、准入和共享状态

**阅读入口**：`internal/gateway/auth.go`；`handler.go` 的 `Handle`、`executeAttempts`；`concurrency.go`；`access_quota_gate.go`；`internal/cluster/concurrency.go`、`access_key_rpm.go`、`access_quota.go`、`rpm_stats.go`。本节未写全的 gateway 文件均位于 `internal/gateway/`。

**实际流程**：认证先读取授权快照。普通转发先取得请求槽，再做费用检查和 RPM。选定业务候选、完成护栏后取得分组槽和凭据槽。费用 `Admit` 与前面的 `Check` 分离；响应后 `Complete` 结算。

| 状态 | 所有者与释放时点 | 关键行为 |
| --- | --- | --- |
| 全局、AccessKey 并发 | 逻辑请求；结算和日志 emit 后释放 | 集群级；满额返回 429 |
| 分组并发 | 一次上游调用；Forward 返回后释放 | 满额直接拒绝，不换组绕过 |
| 凭据并发 | 一次凭据执行；Forward 返回后释放 | 满额跳过凭据，不消耗外发次数 |
| RPM | Redis 时间窗口 | 有限 RPM 依赖共享准入；统计观测尽力而为 |
| 费用额度 | Redis ticket 与持久恢复依据 | Check/Admit 失败拒绝；Complete 尽力结算 |

**审查要求**：

- 检查每个早退、重试、取消及执行器清理后的 release。不得仅检查成功路径。
- 并发上限不大于零时不占该作用域的槽。新实现不得把它变成本地无限计数。
- Redis 故障、规则过期、满额是不同结果。门控中的规则过期对应 `configuration_changed`；其他共享状态不可用对应 `cluster_state_unavailable`，不能混成 429。
- 软亲和、健康回退、统计观测、护栏证据不与强准入共用一套失败策略。
- 当前 HTTP cancel 跳过请求槽、分组槽和护栏；凭据槽调用仍存在。必须按第 8 节核对预期，不把它写成全豁免。
- 使用 Redis hash tag、Lua、TTL 和有界 context。真实 Redis Cluster 的跨槽语义不能只靠内存模拟测试证明。

**测试入口**：`internal/cluster/concurrency_test.go`、`internal/gateway/concurrency_test.go`。同时阅读费用与 RPM 改动对应的同包测试。

### M04. 渠道、路由、调度和重试

**阅读入口**：`internal/channel/modules/`、`internal/channel/channel.go`、`internal/channel/builtins.go`；`internal/state/snapshot.go`；`internal/scheduler/scheduler.go`、`fair.go`；`internal/provideradapter/registry.go`；`internal/gateway/handler.go` 的 `executeAttempts`。

**实际流程**：渠道声明能力和目标；配置编译生成候选；调度器按协议、操作、模型、权限、存储要求和凭据健康选择候选。默认先 Native 后 Converted；`weighted_mix` 不取消存储优先级。公平调度 ledger 和共享别名模型游标留在副本内。生产适配器由 `ProviderKind` 绑定，不由连接类型直接决定。

**审查要求**：

- 一个渠道一个声明文件。新渠道必须同时注册能力、凭据结构和响应清理规则，不为去重引入共享构造层。
- 请求捕获的凭据身份必须在执行前再次核对。删除、重定向和换身份不能继续使用旧凭据。
- 区分候选准备失败、实际外发、受控认证刷新重放。三者不能共用错误的尝试计数。
- 分组参数覆盖不能重选业务路由。换组必须重建 prepared request，不能复用上一组的脱敏或参数结果。
- 重试决策读取执行合同和下游提交状态。不得把所有 401、429、503 或传输错误统一重试。
- Bifrost 当前关闭 SDK 自动重试并清空转换 fallback。不得让 SDK 与网关同时换候选。
- `ResponseScrub`、模型别名及 `/v1/models` 的 `gateway_name` 语义必须贯穿新增出口。

**测试入口**：`internal/scheduler/route_strategy_test.go`；渠道与 provideradapter 的同包测试；`internal/gateway/concurrency_test.go`。

### M05. 协议执行、转换和流式交付

**阅读入口**：`internal/gateway/execution_forward.go` 的 `newExecutionAttemptSpec`、`ForwardStream`；`internal/execution/contracts.go`；`internal/execution/bifrost/executor.go`、`converted.go`、`compatible.go`、`compatible_stream.go`、`chat_conversion.go`、`runtime_manager.go`；`internal/execution/cpa/adapter.go`。

**实际流程**：网关生成单次执行合同；适配器校验能力并执行指定目标。Bifrost 在原生透传、typed Chat、typed Responses 和专用操作间分流。CPA 使用自己的 bridge，不能套用 Bifrost 内部链路。

当前 OpenAICompatible 的 Converted 请求链是：

```text
客户端 Responses / Anthropic / Gemini
  → Converted Responses 准备
  → prepareCompatibleChatRequest
  → Chat SDK（实际外发 /chat/completions）
  → Responses 中间结果
  → 客户端协议编码
```

普通 Compatible Chat 原生请求不经过这条桥。转换流延后发送终态，合并尾部 usage；晚到错误不能伪装成成功。工具增量逐个转换，工具完成事件保持输出索引顺序。raw 捕获只用于判断 usage 存在性，读取后清除。

**审查要求**：

- 同时核对实际 wire 路径、模型、认证头、代理、body 和能力声明。中间类型叫 Responses 不代表外发 `/responses`。
- 原生响应仍有头部清理、别名、渠道 scrub 和还原。不得把 Native 理解为无需检查的字节直通。
- 覆盖完整推理、摘要、工具历史、多工具交错和工具输出类型。不得添加当前生产装配下不可达的兼容分支。
- `Ready` 只准备上游响应头，不等于下游 committed。提交前可按规则重试；提交后不得拼接第二个候选的响应。
- 区分正常协议终态、普通 EOF、缺失 finish、late error、客户端取消及写入失败。
- usage missing 与明确 zero 必须分开。不得让 SDK 合成零值改变计费或日志语义。
- SDK 升级时重查能力解析器、工具转换、raw 捕获、终态与 metadata 清理。旧假设不能自动延续。

**测试入口**：`internal/execution/bifrost/compatible_conversion_test.go`、`compatible_stream_test.go`、`compatible_lifecycle_test.go`；`internal/gateway/output_timing_test.go`。后两个省略前缀的文件同属 `internal/execution/bifrost/`。

### M06. 自动选模、护栏和脱敏

**阅读入口**：`internal/gateway/auto_model.go`、`request_audit.go`、`request_redaction.go`；`internal/automodel/`、`internal/requestaudit/review.go`、`internal/requestredact/`；`internal/cluster/guardrail_verdicts.go`。

**实际流程**：自动选模先过滤有权限且有路由的 preset，再复用已有结果、调用 JEV 或使用 fallback。业务重试不重复自动选模。候选内先应用分组覆盖，再脱敏，再审查客户端协议正文，之后才进入业务执行器转换。

护栏通过 `Prepare → executeJevDecision → Resolve` 获取判定。共享证据按 AccessKey 与内容命名空间隔离；读取按批次组织。一个逻辑请求最多调用一次护栏模型，重试内容变化不能再次送审。辅助调用有独立候选与并发槽，费用归入当前逻辑请求，不递归调用 `Handle`。

**审查要求**：

- 自动选模的缓存或预热结果不等于护栏通过证明。
- 明确 block 返回 403；一般审查异常记录 incomplete 并放行，不能缓存成 passed。
- 不能把 fail-open 理解为吞掉所有拒绝：额度准入失败和当前 JEV 分组并发拒绝仍会向外返回。
- 截断审查不得写完整通过证明。规则、模型、分组路由变化后不能误用旧证据。
- 可逆脱敏密钥从根密钥与 AccessKey ID 派生，不新增每副本密钥文件。
- 响应还原保留在交付边界。即使本次没有脱敏规则，也必须考虑续接内容中的既有签名包装。
- multipart 只改目标文本，不能破坏附件。诊断日志脱敏不能再次破坏成功业务响应。

**测试入口**：`internal/gateway/request_audit_test.go`、`internal/gateway/request_redaction_test.go`；`internal/requestaudit/` 和 `internal/requestredact/` 的同包测试。

### M07. Responses 归属和 WS

**阅读入口**：`internal/gateway/responses_continuation.go`、`responses_store.go`、`websocket.go`、`websocket_turn.go`；`internal/provideradapter/websocket.go`；`internal/cluster/response_bindings.go`。未写全的 gateway 文件均位于 `internal/gateway/`。

**实际流程**：支持存储的原生 Responses 创建请求观察响应 ID，写入共享归属。`previous_response_id` 查询归属并固定凭据身份。`store:false` 或 stateless 降级不能产生虚假的存储承诺。WS 使用声明的原生能力，不是 SSE 转 WS；每轮重新认证、读取配置并准入。

**审查要求**：

- 跨副本续接必须命中同一 AccessKey 下的正确凭据身份，不能仅凭数值 ID 复用。
- 归属查询失败与归属不存在不同。记录失败不能处罚上游凭据。
- stateless 降级必须同步请求与成功响应的 `store:false`。
- WS 握手成功不代表所有后续轮次有权限或额度。
- 已绑定连接不得静默换凭据、目标、代理或身份；需要重连时保留明确协议结果。
- 连接内父响应和推理继承属于连接自身状态，不扩展为跨副本恢复承诺。
- 取消必须覆盖上下游 session、轮次 worker、执行清理和并发租约；不能只关闭客户端 socket。

**测试入口**：`internal/gateway/responses_continuation_cluster_test.go`；`internal/gateway/` 中的 `websocket*_test.go`。

### M08. 凭据授权、刷新和健康

**阅读入口**：`internal/control/credential_stages.go`、`credential_actions.go`、`refresh_sweep.go`、`credential_observations.go`；`internal/subscription/credential_manager.go`、`passive_quota.go`、`passive_quota_flush.go`；`internal/cluster/refresh_lease.go`、`credential_health.go`；`internal/state/shared_health.go`。

**实际流程**：OAuth 授权通过暂存状态领取交换权，分组写事务消费暂存凭据。常规刷新由请求触发：`Prepare → RefreshLease → 重读数据库并校验身份/版本 → Driver.Refresh → CommitCredentialState`。同伴刷新后复用新版本。认证状态、业务健康、额度观测分别维护，不能混成一个“凭据状态”。

**审查要求**：

- 轮换型 token 不得在多个副本重复消费。Redis 刷新租约不可用时不绕过租约外发。
- 刷新结果按旧 `secret_version` 条件提交。失租或本地状态变化后，必须保留迟到成功结果的现有保护，不能丢掉上游已轮换的新 token。
- 自动刷新不得恢复明确失败的认证状态。人工恢复使用独立入口。
- 清扫 refreshing 状态先检查共享租约；Redis 查询失败时不能凭记录年龄判定持有者死亡。
- 健康更新走共享存储；本地回退与后续恢复必须核对身份及版本。
- 额度观测不是 token 刷新。被动样本不得覆盖更新的观测，不把进程内请求合并描述成跨副本互斥。
- 导入保留 `credentials: [{fields, concurrency_limit}]`、每批 1–500 条及指纹冲突校验，不恢复上游文本块 API。

**测试入口**：`internal/subscription/credential_manager_cluster_test.go`；`internal/control/` 的凭据操作测试；共享健康的同包测试。

### M09. 用量、收费、上游成本和币种

**阅读入口**：`internal/gateway/handler.go` 的 `freezeAttemptPricing`；`internal/gateway/request_log.go` 的 `bindUsage`、`quoteFrozenAttempt`、`estimatedCostNano`；`internal/pricing/quote.go`、`currency.go`；`internal/usage/accumulator.go`；`internal/control/client_model_price.go`、`model_price_currency.go`。

**实际流程**：授权快照冻结本次请求币种和 AccessKey 倍率；每个 attempt 冻结自己的价格表与报价参数。执行结果提供用量证据，recorder 将用量归属到对应 attempt，再从同一冻结价格表计算两笔金额。

| 项目 | 计价依据 | 使用者 |
| --- | --- | --- |
| 客户收费 | 客户端模型统一价 × AccessKey 倍率 | 额度、收费日志、用量 |
| 上游成本 | 渠道模型价 × 分组倍率 | 管理员对账 |
| 本币金额 | USD 金额按请求冻结汇率换算 | 历史日志与额度结算 |

无统一价时为 unpriced，不回退到渠道价。自动选模按选定目标模型报价。答复、自动决策、护栏的费用分项结算后相加；上游成本不得解释为所有重试实际账单的总和。

**审查要求**：

- 区分 usage 的 `complete`、`partial`、`missing`、`not_applicable` 和数值 zero。
- 区分收费完整度与用量完整度。已知分项金额不代表完整定价。
- 金额字段必须明确 USD 或冻结币种，保留对应孪生列、币种键、回执及溢出策略。
- 更改系统汇率不能重算历史。价格编辑的当前币种换算不能替代请求结算。
- 额度结算、日志映射、辅助费用聚合必须使用相同分项舍入口径。
- 当前用量/价格状态校验通过 `requestlog.ValidateUsageCostState`。不得仅因目录名存在，就把未接入主链的 `internal/usagecost/` 当作实际入口。

**测试入口**：`internal/usage/accumulator_test.go`、`internal/pricing/client_quote_test.go`、`internal/pricing/quote_test.go`、`internal/gateway/settlement_currency_test.go`、`internal/requestlog/settlement_currency_test.go`。

### M10. 请求日志、输出计时和报表

**阅读入口**：`internal/gateway/request_log.go`、`output_timing.go`；`internal/dialect/output_timing.go`；`internal/telemetry/requestlog.go`、`auto_model_cost.go`；`internal/requestlog/service.go`、`mapper.go`、`worker.go`、`auto_decision_usage.go`、`usage_query.go`、`usage_query_window.go`；`internal/control/request_logs.go`。

**实际流程**：`recorder.emit → Service.Emit → mapEvent → WriteBatch`。Emit 深复制事件，以非阻塞方式入队；队列满可丢弃日志。批次按 ID 去重，在一个写事务内插入明细、attempt 和聚合，不使用 aggregation journal。

短区间报表读明细；长区间使用完整小时聚合与边界明细。自动决策和护栏费用合并时不重复增加答复请求数与 token 数。零 attempt 和独立搜索不进入普通模型用量聚合。

输出计时只观察成功交付的内容。HTTP 在写出并 flush 后观察；WS 有对应交付入口。文本、推理、工具参数属于有效输出；元数据和结束标记不直接算输出。

**审查要求**：

- 日志不是可靠消息队列。额度结算成功不证明日志入库成功；本副本采集指标不代表集群总量。
- 新字段必须贯穿事件、复制、映射、冻结校验、迁移、查询投影、API 和前端解析。
- 共享统计在数据库内自增，upsert 键排序，聚合维度包含 currency。不得恢复 Go 中读—加—写或 journal 重放。
- 对账区分答复费用、总收费和上游成本。短窗、长窗、边界明细及辅助费用必须口径一致。
- 流式时间满足 `0 ≤ first_output_ms ≤ last_output_ms ≤ duration_ms`。没有证据或观察超限时保持空值，不影响转发。
- 旧 `first_response_ms` 字段、旧筛选参数名与当前首输出展示不是同一语义。前端速度公式必须核对实际字段。
- 流式速度使用 `(output_tokens - 1) / (last - first)`，时间换算为秒；非流式使用 `output_tokens / duration`。无有效分母时不伪造数值。

**测试入口**：`internal/requestlog/worker_test.go`、`usage_query_window_test.go`、`output_timing_test.go`；`internal/gateway/output_timing_test.go`。省略前缀的测试同属 `internal/requestlog/`。

### M11. 迁移、后台任务和目录同步

**阅读入口**：`internal/storage/migration.go`、`migration_lock.go`、`migration_inspection.go`、`migrations/`、`dbtx/transaction.go`；`internal/control/runtime.go`、`catalog_sync.go`；`internal/cluster/job_lease.go`、`catalog_store.go`。

**实际流程**：启动持 PostgreSQL advisory lock 检查迁移账本，逐项执行并校验。当前 dev 注册核心 `0001`–`0027`；后续版本必须重新读取注册表。框架支持独立编号线，但当前 dev 入口不注册 main 的灵影迁移。

`Runtime.Run` 管理验证、保留期清理、操作恢复、目录同步、配置同步、健康同步及 OAuth 回调生命周期。共享周期任务使用 JobLease；它是减少重复执行的有时限认领，不是严格 exactly-once。目录通过 Redis 保存共享数据，再应用到 PG 价格与本地目录。

**审查要求**：

- 已应用核心迁移不得因上游编号相同而覆盖、重排或改号。新增迁移使用 dev 下一空号并记录映射。
- 冻结迁移结构体不得引用随业务变化的运行时模型。新库、增量升级和已应用迁移的当前结构验证都必须检查。
- 写事务使用 `READ COMMITTED`；一致报表读取使用 `REPEATABLE READ`。提交不确定与回滚失败必须保持清理策略。
- 旧程序可能因未知账本项拒绝启动。即使只增加可空列，也不能直接承诺仅回退镜像。
- 周期任务必须可重复执行。无租约续期的 JobLease 不能保证长任务永不重叠。
- 配置监听、日志工作器、请求内流转换 goroutine 不因“后台运行”就统一加 JobLease；它们有自己的副本或请求归属。
- 不恢复本地 checkpoint、SQLite/MySQL、RPM 持久表或 journal 清理路径。
- 不得沿用旧文档的“未上线，可直接改创建迁移”许可。涉及现有数据和破坏性迁移时重新确认。

**测试入口**：`internal/storage/migration_test.go`、`internal/storage/migration_recovery_test.go`；`internal/cluster/` 的租约测试；`internal/control/` 的运行时与目录测试。

### M12. 管理 API、权限和现代前端

**阅读入口**：`internal/control/http_routes.go`、`auth.go`、`request_logs.go`、`usage.go`；`docs/openapi.yaml`；`internal/webui/page_routes.json`；`web/src/shared/http/client.ts`；`web/src/frontends/modern/bootstrap.ts`、`app/api-client.ts`、`features/auth/auth-session.ts`、`router.ts`、`api/`、`features/`。

**实际流程**：服务端控制路由执行认证及写入审计。AccessKey 角色只能使用 GET 白名单，并强制限定自身数据。前端 HTTP client 校验 `/api/` 路径、响应 envelope 和错误；业务 API parser 再校验字段。登录恢复先验证身份，身份切换清理查询缓存，旧身份请求的 401 不清除新会话。

**审查要求**：

- 新 GET 不自动对 AccessKey 开放。新增字段必须检查服务端脱敏与作用域，隐藏前端列不能替代权限控制。
- `sanitizeAccessKeyRequestLog` 必须清除新引入的内部信息及上游成本。AccessKey 的价格视图只展示统一价。
- 路由、请求字段、错误码、幂等头、操作枚举同步到 API parser、页面、三语文案和契约测试。
- `docs/openapi.yaml` 只覆盖部分端点。范围内必须同步；范围外不得声称由 OpenAPI 自动保证。
- 页面清单只有 `internal/webui/page_routes.json`。新增页面同步 Vue 路由与后端深链入口。
- 只维护 modern 前端；不恢复 classic、切换器和已删除依赖。
- 金额与大整数按现有 wire 契约解析。输出计时空值与旧日志保持兼容。
- 类型检查和构建不能证明浏览器身份隔离、交互和流式显示正确。

**测试入口**：`internal/control/ui_api_contract_test.go`、`wire_v3_contract_test.go`、`client_model_price_test.go`；`internal/app/http_routes_test.go`；`internal/webui/page_routes_test.go`、`server_test.go`。省略前缀的 control 测试同属 `internal/control/`。

### M13. 构建、验证和发布契约

**阅读入口**：`Makefile`、`go.mod`、`web/package.json`、`web/vite.config.ts`、`Dockerfile`、`docker-compose*.yml`、`.github/workflows/`；`internal/webui/workflow_test.go`、`container_contract_test.go`。

**实际流程**：`make check` 安装锁定前端依赖并启动测试 PG/Redis，执行 gofmt 检查、tidy diff、vet、前端 lint/format/build、Go build、Go tests 和 diff 检查。前端 build 包含类型检查，产物进入 `internal/webui/dist` 后由 Go 嵌入。

**审查要求**：

- 先核实 `go.mod` 和 `web/package.json` 的工具版本，不使用文档中的历史版本替代当前声明。
- 不为减少测试产物而省略验证。只发布容器，不恢复原生安装包或上游维护者验收脚本。
- workflow action SHA、Dockerfile pin、Compose 和发布 stage 改动同步契约测试。
- 测试依赖缺失必须修复环境，不能把失败改成 skip。
- 构建产物必须来自待审 tree。旧嵌入前端不能证明当前源码可发布。
- 若工作区历史 `tmp/` Go 文件干扰全仓检查，使用同源码 tree 的干净工作树；不得删除历史资料掩盖失败。
- 文档、包测试、真实集群、浏览器和真实上游验证分别报告，不互相替代。

## 5. 跨模块组合检查

逐项写“受影响／不受影响及理由”，不能只检查本批修改过的文件。

| 变化类型 | 必须追踪的完整链 |
| --- | --- |
| 新 operation 或渠道 | 声明 → 编译 → 路由 → 适配器 → 原生/转换 → 日志枚举 → UI/契约 |
| 新准入或共享状态 | 入口 → Redis 原子操作 → 错误映射 → HTTP/WS → 取消释放 → 跨副本测试 |
| 新配置项 | API 校验 → 控制事务 → PG revision → 重载/编译 → 消费方 → 设置页/文档 |
| 新费用或用量字段 | 执行证据 → attempt 归属 → 双报价/币种 → 额度 → telemetry → PG/聚合 → 权限 → UI |
| 流式终态或 SDK 升级 | 实际 wire → 增量/工具 → 提交边界 → 尾 usage/错误 → 计时 → 取消清理 |
| 凭据或代理变化 | 幂等写入 → 身份/secret_version → 配置传播 → 旧执行资源退役 → 续接与刷新 |
| 迁移或统计变化 | 新库 → 增量库 → 并发写 → 重复批次 → 报表投影 → 旧日志 → 回滚程序账本 |
| 页面或权限变化 | 后端角色检查 → 数据裁剪 → parser → 会话缓存 → 页面清单 → 三语文案 |

必须回答四个组合问题：

1. 后一个提交是否绕过、覆盖或重复实现了前一个提交的修复？
2. 相邻模块是否对同一字段采用不同含义、冻结时点或单位？
3. 请求在任一失败出口结束时，状态和资源是否仍由原所有者收尾？
4. 放到多个副本后，请求、配置和后台任务是否仍保持预期语义？

精简审查只报告实际可达的重复与多余实体。要求指出调用方、现有替代实现和保持行为的方法。不得为了抽象一致或假想兼容增加 wrapper、接口和配置项。

## 6. 验证计划与完成标准

### 6.1 验证层级

| 层级 | 内容 | 可以支持的结论 |
| --- | --- | --- |
| L0 | 读取实现、装配和测试断言 | 当前源码调用链与设计事实 |
| L1 | 受影响包测试，按需 race | 指定包与用例的本轮运行结果 |
| L2 | 完整 `make check` | 当前 tree 的本地合并门禁 |
| L3 | 授权测试环境的多副本和故障场景 | 本轮环境中的集群行为 |
| L4 | 浏览器或真实上游 | 指定客户端、协议与上游兼容性 |

仅要求分析时，完成 L0 后列出验证计划即可。用户要求合并验收时，L1 不能替代 L2。L3/L4 按改动和授权选择；未执行的检查写明范围，不自动当作产品缺陷或新待办。

### 6.2 本地命令

测试前检查依赖是否为共享环境。先创建本轮独立测试库，再设置 DSN；不要使用或删除其他任务的数据库。

```bash
# 将占位值替换为本轮独立测试库连接，不输出真实秘密。
export GPT_LOAD_DATABASE_TEST_DSN='<本轮独立 PostgreSQL 测试库 DSN>'
export GPT_LOAD_REDIS_TEST_ADDR='127.0.0.1:56379'
make test-deps

# 示例：按受影响模块选择，不代表这些包覆盖整个系统。
go test -count=1 ./internal/gateway ./internal/execution/bifrost ./internal/cluster
go test -race -count=1 ./internal/gateway ./internal/cluster

# 完整本地门禁。
make check
git diff --check
```

具体端口以当前 `tools/testdeps/compose.yml` 为准。清理仅限本轮创建的库和资源，不执行共享 Redis FLUSH。测试失败时保留命令、日志、tree 与环境；分析原因后重跑对应范围。不能因旧记录说“环境问题”就忽略本轮失败。

### 6.3 按改动选择的运行场景

| 改动范围 | 最小有意义的场景 |
| --- | --- |
| 执行/转换 | 受影响协议的原生与转换、流与非流；工具、推理；missing/zero usage；HTTP 拒绝、晚到错误、取消 |
| 准入/租约 | 两副本争同一限制；满额不外发；取消清理后恢复；Redis 故障与规则更新 |
| 控制写入 | 同键跨副本重放；提交后发布失败恢复；丢通知后轮询收敛 |
| Responses/WS | 跨副本续接；store 降级；绑定身份变更；每轮准入；断开后资源释放 |
| 费用/日志 | 无统一价；系统币种变更；辅助费用；短窗/长窗一致；重复批次；AccessKey 隐藏成本 |
| 凭据刷新 | 两副本只消费一次轮换 token；同伴新版本复用；迟到成功提交；失败认证人工恢复 |
| 前端 | 管理员/AccessKey 两种身份；身份切换；旧日志空字段；相关页面交互 |

集群准入的“拒绝”必须结合上游实际调用次数证明，不能只断言 HTTP 状态码。流式“成功”必须检查最终事件与日志状态，不能只断言 HTTP 200。

### 6.4 完成标准

- 追踪表覆盖全部目标上游提交及纳入的前置提交。
- 13 个模块均有结论；不受影响模块写出判定依据。
- 受影响模块覆盖入口、状态归属、失败出口和消费者。
- 所有发现有实际调用链、触发条件和可定位证据。
- 区分批次引入、历史已有、约定不一致和未确认事项。
- 验证结果绑定源码 tree、命令和环境，不复用旧批次的 PASS。
- 未执行的验证单独列出，不以“未发现问题”替代。
- 若修复了代码，重新审查修复差异并重跑受影响门禁。

## 7. Agent 报告模板与调用方式

### 7.1 报告模板

```markdown
# 批次核心流程审查报告

## 结论
- 工作状态：
- 源码范围：BASE / TARGET / tree / 工作区改动：
- 本轮验证：L0–L4 的实际范围：

## 提交追踪
| 上游提交 | 意图 | fork 落地 | 处置依据 | 模块 | 证据 |

## 模块结论
### Mxx 模块名
- 影响：受影响／不受影响，理由。
- 实际调用链：路径 + 符号。
- 状态与资源：所有者、冻结时点、释放时点。
- 组合变化：多个提交共同改变了什么。
- 结论：源码确认的事实。
- 验证：命令、结果、证据路径；或未验证原因。

## 发现
### Fxx 标题
- 分类：批次引入／历史已有／约定不一致／未确认。
- 影响：具体用户或集群行为。
- 触发：真实入口与前置条件。
- 证据：目标提交中的 file:line、调用链、测试或日志。
- 归因：BASE 是否存在；对应上游或适配提交。
- 建议：最小修改或必须确认的决定。
- 验证：复现与修复后的验证方法。

## 跨模块结论
- 提交边界、共享状态、计费、权限与配置传播。

## 验证记录
| 命令/场景 | tree/环境 | 结果 | 证据 | 未覆盖范围 |

## 待确认
- 只列缺少依据或必须由用户裁决的事项。
```

### 7.2 可直接交给 Agent 的指令

```text
请按 docs/upstream-core-flow-review.md 梳理本轮上游合并结果。
BASE=<合并前 dev 提交>
TARGET=<合并后提交>
上游集合=<SHA 列表>
适配记录=<提交说明或文档位置>
任务模式=只读分析
报告路径=<本轮报告文件>

先核对工作区、提交范围和生产依赖装配。
按 M01–M13 分工，按第 5 节汇总跨模块组合影响。
每个结论给真实调用链；每个发现给触发条件与 file:line。
区分当前事实、架构预期、历史问题与本轮回归。
不要仅按 diff 逐文件复述，也不要只输出检查清单。
没有运行的测试必须写未验证。
不得修改业务代码、提交、推送、部署或调用付费上游。
需要扩大权限或裁决语义时，先报告并询问。
```

## 8. 当前基线的易误读事项

这些项目用于防止后续 Agent 复制过时说明。它们不是本轮产品缺陷验收结论。

1. **cancel 并发豁免存在源码与约定差异。** 架构文档 §8 描述三个层级均豁免。当前 `internal/gateway/handler.go` 的 `executeAttempts` 在 `OperationResponsesCancel` 条件外仍调用凭据 `Acquire`。源码确认的豁免只有请求槽、业务分组槽与护栏；影响及预期必须另行裁决，本文不擅自修改代码。
2. **护栏 fail-open 有明确边界。** `internal/gateway/request_audit.go` 仍返回额度准入失败和 JEV 分组并发拒绝。不能把“一般审查异常放行”扩展成所有失败放行。
3. **原审计命令不能直接验收已提交批次。** `.agents/skills/upstream-merge/audit.sh` 只看暂存区。旧说明中的 `.claude/skills/upstream-merge` 路径不能代替当前实际入口。
4. **旧迁移授权不是当前授权。** 架构说明中的“未上线”历史前提不能证明当前数据库可以重建。检查实际账本与本次授权后再决定。
5. **目录名不证明调用关系。** 当前实际状态校验走 `requestlog.ValidateUsageCostState`；Compatible Converted 实际调用 Chat SDK。必须用生产装配与调用搜索确认，不能从包名猜主链。

后续代码或架构决定变化时，更新本节并记录新的源码基线。不能为了让文档与代码一致，直接把尚未裁决的差异当作新架构要求。

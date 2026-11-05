# 上游暂缓合并备忘录

记录 `upstream/main`（tbphp/gpt-load）上决定暂时不合并的提交。执行 `/upstream-merge` 时先查这张表，命中的提交跳过；要恢复合并时，把对应条目移到「已恢复」并写明日期。

## 暂缓中

| 上游提交 | 标题 | 上游日期 | 记录日期 | 原因 | 恢复条件 |
| --- | --- | --- | --- | --- | --- |
| `0a01cdde504062c2810eb095c5e3bda00aae260c` | feat(gateway): add Codex live voice relay (#756) | 2026-09-25 | 2026-10-26 | 待补充 | 待补充 |
| `c4a895ad61382a1960de72c7f9d9d2756e9dbc7f` | test(gateway): wait for live teardown before checking key rotation log (#761) | 2026-09-25 | 2026-10-26 | 只改 `0a01cdde` 引入的 live 集成测试，dev 没有该文件 | 随 `0a01cdde` 一起恢复 |
| `03e1da4824f9ccf37410fba069560addd6420534` | feat(gateway): add voice modes and live call retries (#765) | 2026-09-26 | 2026-10-26 | 在 `0a01cdde` 的 live 中继上扩展语音模式与通话重试，dev 没有 live 代码 | 随 `0a01cdde` 一起恢复 |
| `41339ae935e89f68d342273711a5e91a5f4617bf` | fix(gateway): Restore live logs and bound teardown (#774) | 2026-09-26 | 2026-10-28 | 修 `0a01cdde`/`03e1da48` 引入的 live 会话日志与拆除流程，dev 没有 live 代码 | 在 `03e1da48` 之后恢复 |
| `935aff9776e46fceb23caf3f6c141d28d7533be6` | fix(cpa): refresh subscription model catalogs online (#835) | 2026-10-09 | 2026-11-05 | 用户决定暂缓在线更新；上游逐副本下载目录，共享目录方案缺少 CPA 公开装载接口，暂不引入补丁依赖或完整 SDK 副本 | 重新确认目录一致性要求及依赖方案，通过架构闸门后恢复 |

### `0a01cdde` 改动范围

- 规模：73 个文件，+4019 / -73。
- 新增 Codex live 语音中继：`internal/gateway/live*.go`、`internal/execution/live.go`、`internal/execution/cpa/live.go`、`internal/subscription/providers/codex/live.go`、`third_party/cpaembedded/embedded/codex_live.go`。
- 牵动的公共部分：`go.mod`/`go.sum`、`.env.example`、`internal/platform/config`、`internal/channel`、`internal/control`（分组、模型、路由检查）、`internal/state/snapshot.go`、classic 与 modern 前端（协议标签、路由检查、首页网关配置）。
- 后续上游提交如果依赖这次的 live 相关代码，合并时要一并评估。
- 2026-10-26：它是 `7587c60f`（#760）的前置提交，已用 `git merge -s ours` 记录祖先关系（dev 提交 `909b67b6`），内容没有带入。`merge-base` 已越过它，**恢复时不能再走 merge**，要用 `git cherry-pick 0a01cdde`，或按当时的 dev 架构手工移植，并走一次架构闸门。
- 2026-10-29：`7ceb5976`（#791）把 CPA 升到 v8，模块路径从 `CLIProxyAPI/v7` 改成 `CLIProxyAPI/v8`，同时修改了 `live_media.go`、`codex/live.go`、`codex_live.go` 三个 live 文件的 import。该提交的其余部分已合入 dev，这三个文件仍保持删除。恢复时 import 一律改用 v8 路径，go.mod 里还要重新加上 `pion/*` 依赖，并复核 `DisableCodexCloaking` 和 `codex_headers.go` 里 registry 的头覆写在 live 的 WebSocket 握手上是否同样生效。

### `c4a895ad` 改动范围

- 规模：1 个文件，+8 / -2，只改 `internal/gateway/live_integration_test.go`。
- 内容：`TestCodexLiveRevokesActiveCallWhenAccessKeyRotates` 原来用同一个 5s 窗口同时等授权轮询和 WebRTC 拆除，race 模式下会超时；改成先在 5s 内等通话关闭，再给拆除单独一个窗口。
- 2026-10-26：已用 `git merge -s ours` 记录祖先关系（dev 提交 `68f1b3b1`），内容没有带入。恢复 `0a01cdde` 时一并 `git cherry-pick c4a895ad`，或在移植后的测试里手工补上这个等待窗口。

### `03e1da48` 改动范围

- 规模：51 个文件，+1333 / -261。
- 新增语音模式 `codex_live_mode`（`off` / `direct` / `relay`，默认 `direct`）：系统级运行时设置（`internal/state/runtime_settings.go`、`snapshot.go`），Codex 分组可以覆盖（`internal/control/group_detail.go`、`settings.go`）。只有 `relay` 模式使用进程级的 `CODEX_LIVE_*` 媒体配置。
- live 通话重试与拆除恢复：`internal/gateway/live.go`、`live_session.go`、`internal/execution/cpa/live.go`，新增 `live_routing_test.go`、`live_teardown_test.go`。
- 部署：新增 `docker-compose.voice.yml`（发布 UDP 端口段），`docker-compose.yml`、`.env.example` 同步改动；未设置 `CODEX_LIVE_UDP_PORT_MIN` 时默认使用 50000–50127（`internal/platform/config/config.go`）。
- 其他：路由检查（`internal/scheduler/inspect.go`）展示语音模式；classic 与 modern 前端都有设置项和 i18n；三份 README 都补了语音章节。
- 恢复时要过架构闸门：`codex_live_mode` 属于控制面运行时设置，会走配置 reload；relay 模式的 UDP 端口和媒体 IP 是按副本配置的，3 副本部署下要重新设计。
- 2026-10-26：已用 `git merge -s ours` 记录祖先关系（dev 提交 `2d0effd2`），内容没有带入。恢复 `0a01cdde` 之后再 `git cherry-pick 03e1da48`，或手工移植。

### `41339ae9` 改动范围

- 规模：16 个文件，+459 / -54。
- live 会话日志：本地关闭且第一次上游 hangup 尝试之后写一条会话日志；第一次 hangup 失败时返回 HTTP 502，并立即记为未完成，不等后台清理。后续重试不再改写或重复写这条日志（`internal/gateway/live.go`、`live_session.go`）。
- hangup 重试有上限：每 5 秒重试一次，连同首次和手动尝试最多 3 次。重试期间保留容量槽并拒绝重新接入控制连接；成功或次数用尽后释放本地槽。用尽并不代表上游已终止，服务端告警会带上失败类别和 HTTP 状态。
- 创建后解析失败时，`LiveCall` 也会返回已知的 CallID 和 Session，交给调用方清理（`internal/execution/live.go`、`internal/execution/cpa/live.go`、`internal/subscription/providers/codex/live.go`、`third_party/cpaembedded/embedded/codex_live.go`）。
- 对外契约：新增兼容路由 `POST /v1/live/:call_id/hangup`（`data.codex.live.legacy.hangup`，在 `internal/gateway/router.go` 及其路由契约测试中）。三份 README 改写了语音章节的升级说明。
- 测试：新增 `live_control_test.go`、`live_lifecycle_test.go`，改了 `live_teardown_test.go`、`live_routing_test.go`、`codex_live_test.go`。
- 恢复时要过架构闸门：hangup 重试和容量槽都是按副本持有的状态，3 副本部署下要确认 hangup 请求能落到持有会话的那个副本。
- 2026-10-28：已用 `git merge -s ours` 记录祖先关系（dev 提交 `a55a9b63`），内容没有带入。恢复 `03e1da48` 之后再 `git cherry-pick 41339ae9`，或手工移植。
- 2026-10-28：`1d8232eb`（#781）把 `TestCodexLiveFailedSetupRetainsKnownUpstreamForCleanup` 从 `httptest.NewServer` 改成 `httptest.NewRecorder` 直接调 `engine.ServeHTTP`，以消除 race 模式下的抖动。该提交的其余部分已合入 dev，这个 hunk 没有带入；恢复 `41339ae9` 后再执行 `git show 1d8232eb -- internal/gateway/live_lifecycle_test.go | git apply`，或手工补上。

### `935aff97` 改动范围

- 规模：6 个文件，+256 / -0。
- 新增 CPA 订阅模型目录在线更新器：启动时下载，此后每 3 小时刷新；主备地址均失败时保留当前内存目录。
- 修改 `internal/container/container.go`、`internal/control/runtime.go` 及生命周期测试；新增 `internal/subscription/providers/model_catalog.go`、`third_party/cpaembedded/embedded/model_catalog_updater.go` 及测试。
- 更新对象是 CPA 的模型定义与能力资料，不是 Models.dev 目录、订阅账号凭据、分组配置或价格。当前继续使用 CPA v8.0.8 内置目录；后续升级 CPA 依赖可以更新内置目录，不等于恢复本提交的在线刷新功能。
- 上游更新器按进程独立下载。下载结果不同时，各副本的模型发现及读取目录的参数处理会有差异；目录不承担账号、集群并发或费用额度状态。
- 共享方案需拆分下载与装载：复用集群任务租约，将有效目录存 Redis，各副本加载派生镜像。CPA v8.0.8 未公开静态目录装载接口；仅在现有 cpaembedded 包装层增加函数不能访问未导出的实现。若采用补丁依赖，必须保留目录校验、变化检测与刷新通知语义。
- 2026-11-05：用户最终决定不引入 #835 功能，仅用 `git merge -s ours` 记录祖先关系，并在本文件暂缓登记。此次 merge 仅增加本备忘录，不引入上述 6 个文件的上游改动；此前讨论的 Redis 方案不作为本次实施授权。
- 恢复时不能再次直接 merge 此提交。必须按当时的 dev 架构手工移植，或以 `git cherry-pick 935aff9776e46fceb23caf3f6c141d28d7533be6` 为移植起点，重新执行架构闸门和验证。后续上游修改本更新器时，必须先核对本条暂缓决定。

## 已恢复

（暂无）

# 上游暂缓合并备忘录

记录 `upstream/main`（tbphp/gpt-load）上决定暂时不合并的提交。执行 `/upstream-merge` 时先查这张表，命中的提交跳过；要恢复合并时，把对应条目移到「已恢复」并写明日期。

## 暂缓中

| 上游提交 | 标题 | 上游日期 | 记录日期 | 原因 | 恢复条件 |
| --- | --- | --- | --- | --- | --- |
| `0a01cdde504062c2810eb095c5e3bda00aae260c` | feat(gateway): add Codex live voice relay (#756) | 2026-09-25 | 2026-10-26 | 待补充 | 待补充 |
| `c4a895ad61382a1960de72c7f9d9d2756e9dbc7f` | test(gateway): wait for live teardown before checking key rotation log (#761) | 2026-09-25 | 2026-10-26 | 只改 `0a01cdde` 引入的 live 集成测试，dev 没有该文件 | 随 `0a01cdde` 一起恢复 |
| `03e1da4824f9ccf37410fba069560addd6420534` | feat(gateway): add voice modes and live call retries (#765) | 2026-09-26 | 2026-10-26 | 在 `0a01cdde` 的 live 中继上扩展语音模式与通话重试，dev 没有 live 代码 | 随 `0a01cdde` 一起恢复 |

### `0a01cdde` 改动范围

- 规模：73 个文件，+4019 / -73。
- 新增 Codex live 语音中继：`internal/gateway/live*.go`、`internal/execution/live.go`、`internal/execution/cpa/live.go`、`internal/subscription/providers/codex/live.go`、`third_party/cpaembedded/embedded/codex_live.go`。
- 牵动的公共部分：`go.mod`/`go.sum`、`.env.example`、`internal/platform/config`、`internal/channel`、`internal/control`（分组、模型、路由检查）、`internal/state/snapshot.go`、classic 与 modern 前端（协议标签、路由检查、首页网关配置）。
- 后续上游提交如果依赖这次的 live 相关代码，合并时要一并评估。
- 2026-10-26：它是 `7587c60f`（#760）的前置提交，已用 `git merge -s ours` 记录祖先关系（dev 提交 `909b67b6`），内容没有带入。`merge-base` 已越过它，**恢复时不能再走 merge**，要用 `git cherry-pick 0a01cdde`，或按当时的 dev 架构手工移植，并走一次架构闸门。

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

## 已恢复

（暂无）

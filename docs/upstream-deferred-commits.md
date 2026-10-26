# 上游暂缓合并备忘录

记录 `upstream/main`（tbphp/gpt-load）上决定暂时不合并的提交。执行 `/upstream-merge` 时先查这张表，命中的提交跳过；要恢复合并时，把对应条目移到「已恢复」并写明日期。

## 暂缓中

| 上游提交 | 标题 | 上游日期 | 记录日期 | 原因 | 恢复条件 |
| --- | --- | --- | --- | --- | --- |
| `0a01cdde504062c2810eb095c5e3bda00aae260c` | feat(gateway): add Codex live voice relay (#756) | 2026-09-25 | 2026-10-26 | 待补充 | 待补充 |

### `0a01cdde` 改动范围

- 规模：73 个文件，+4019 / -73。
- 新增 Codex live 语音中继：`internal/gateway/live*.go`、`internal/execution/live.go`、`internal/execution/cpa/live.go`、`internal/subscription/providers/codex/live.go`、`third_party/cpaembedded/embedded/codex_live.go`。
- 牵动的公共部分：`go.mod`/`go.sum`、`.env.example`、`internal/platform/config`、`internal/channel`、`internal/control`（分组、模型、路由检查）、`internal/state/snapshot.go`、classic 与 modern 前端（协议标签、路由检查、首页网关配置）。
- 后续上游提交如果依赖这次的 live 相关代码，合并时要一并评估。
- 2026-10-26：它是 `7587c60f`（#760）的前置提交，已用 `git merge -s ours` 记录祖先关系（dev 提交 `909b67b6`），内容没有带入。`merge-base` 已越过它，**恢复时不能再走 merge**，要用 `git cherry-pick 0a01cdde`，或按当时的 dev 架构手工移植，并走一次架构闸门。

## 已恢复

（暂无）

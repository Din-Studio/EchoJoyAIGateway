# 操作指南：把 dev 同步到 main

- 编写日期：2026-10-22，基于 main `e25ba516`、dev `33b3baff`
- 适用场景：dev 合并了上游（tbphp/gpt-load）提交或其他核心改动后，需要带到 main

## 1. 分支模型

| 分支 | 内容 | 写入方式 |
|---|---|---|
| `upstream/main` | tbphp/gpt-load 上游 | 只读 |
| `dev` | 上游 + 集群架构适配（核心） | 受保护，只能走 PR（`merge/upstream-MMDD` 等分支） |
| `main` | dev + 灵影专属改动（计费回调、EchoJoyAuth、AWS 部署等） | 走 PR（`merge/dev-into-main-MMDD` 等分支） |

数据单向流动：`upstream/main → dev → main`。灵影专属提交只留在 main，不回合到 dev。

```
upstream/main ──(/upstream-merge)──► dev ──(git merge)──► main
                                                   ▲
                                     灵影专属提交 ──┘
```

## 2. 原则：用 merge，不用 cherry-pick 或 rebase

- **用 `git merge`**：merge 会推进 main 与 dev 的共同祖先，下次只带入 dev 的新增提交。
- **不要 cherry-pick**：同一改动在两条分支上会变成不同的提交，下次 merge 时三方合并会在同一处冲突。
- **不要 rebase main**：main 已推送并部署，改写历史会让所有协作者的 main 失效。
- **同一改动只在 dev 提交一次**：核心改动先合进 dev，再通过 merge 带到 main；不要在 main 上手工重做一遍。

### PR 合并方式

| PR 类型 | 合并方式 |
|---|---|
| 普通功能、修复 PR（单个分支，不含合并提交） | **Rebase and merge**，保持历史整洁 |
| `merge/dev-into-main-*`（dev 合进 main） | **Create a merge commit** |
| `merge/upstream-*`（上游合进 dev） | **Create a merge commit** |

同步类 PR 不能用 rebase 或 squash：

- rebase 会丢掉合并提交，把对方分支的提交逐个重放到目标分支。合并提交里解决过的冲突会重新出现，GitHub 会提示无法 rebase。
- 即使没有冲突，重放后的提交也是新 hash，效果等同 cherry-pick，下次同步时会再次冲突（见第 6 节）。

## 3. 操作步骤（约 5–15 分钟，含冲突处理）

```bash
git fetch origin
git switch -c merge/dev-into-main-MMDD origin/main
git merge --no-ff origin/dev -m "merge: dev ($(git rev-parse --short origin/dev)) into main"
# 有冲突时按第 4 节处理，然后 git add + git commit
make check
git push -u origin HEAD
gh pr create --base main --title "merge: dev into main"
gh pr merge <PR号> --merge --delete-branch   # 必须用 merge commit，见第 2 节
```

合并前可以先看差距，确认要带入哪些提交：

```bash
git log --oneline origin/main..origin/dev    # dev 有、main 没有
git cherry -v origin/main origin/dev         # "-" 表示内容已等价存在于 main
```

## 4. 冲突处理规则

| 冲突位置 | 处理 |
|---|---|
| 核心迁移（`internal/storage/migrations/00NN_*.go`） | 以 dev 为准。核心编号归 dev，main 不新增核心编号 |
| 灵影迁移（`lyNNNN_*.go`，`internal/storage/migration_ly.go`） | 只存在于 main，不会与 dev 冲突；保留 main |
| 同一段代码，dev 改上游逻辑、main 改灵影逻辑 | 两边都保留：以 dev 的新版本为底，把灵影逻辑叠加上去 |
| dev 删除的文件 | 跟随 dev 删除 |
| 部署相关（`deploy/`、AWS runbook 等 main 专属内容） | 保留 main |

### 迁移编号线

迁移按编号线独立运行（`internal/storage/migration.go` 的 `migrationTrack`）：

- **核心线**：`00NN_*`，账本表 `schema_migrations`，只在 dev 新增。
- **ly 线**：`lyNNNN_*`，账本表 `ly_schema_migrations`，只在 main 新增，在核心线之后运行。

两条线编号互不占用，dev 新增核心迁移合进 main 时不需要重新编号。main 需要新表或新列时，在 ly 线追加 `lyNNNN`，不要占用核心编号。

## 5. 验证

- `make check` 通过即为合并门禁（本项目不依赖 CI）。
- main 使用默认测试库 `gpt_load`；dev worktree 使用 `gpt_load_dev`（通过 `GPT_LOAD_DATABASE_TEST_DSN` 覆盖）。两边的迁移集合不同，不要共用同一个测试库。
- 涉及迁移时，确认核心线和 ly 线都能从空库跑通。

## 6. 案例：重复提交导致的冲突（2026-10-22，已在 `merge/dev-into-main-1022` 解决）

「按编号线跑迁移」这个改动在 dev（`4f92ae6c`）和 main（`1b308ccc`）上各提交了一次，main 又在其上叠加了 ly 线（`09eab870`）。把 dev 合进 main 时：

- `internal/storage/migration.go` 会冲突。分叉点 `977fd7cb` 之后 dev 对该文件没有其他改动，直接保留 main 版本：

  ```bash
  git checkout --ours internal/storage/migration.go
  git add internal/storage/migration.go
  ```

- `.claude/skills/upstream-merge/` 下的 3 个文件随 dev（`fe69b806`）删除，无冲突。

该次合并完成后，共同祖先推进到了 dev 的最新提交，此类重复提交冲突不会再出现。如果之后又出现，说明有改动绕过 dev 直接在 main 上重做了，按第 2 节纠正流程。

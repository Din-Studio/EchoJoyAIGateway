---
name: upstream-merge
description: 把 upstream/main（tbphp/gpt-load）上的提交按 dev 的集群架构适配后合并进当前分支。用法：/upstream-merge <upstream/main 上的 commit>
argument-hint: <upstream-commit>
disable-model-invocation: true
---

# upstream-merge

目标提交：`$ARGUMENTS`

dev 是架构权威，上游提交只提供**意图**和**行为**。每个上游提交落成 dev 上的一个真实 merge commit（`git merge --no-ff`），这样 `git merge-base HEAD upstream/main` 会自动前进，下一次合并只看新提交。merge commit 里的内容是**适配后**的结果，不是上游原样。

两份配套材料：

- [`ARCHITECTURE.md`](ARCHITECTURE.md)：dev 的架构不变量，以及每类上游写法的适配方式。第 2 步前读完全文。
- `docs/upstream-merge-ledger.md`（台账）：跨提交的长期状态，包括迁移编号映射、已定的概念映射、已知重叠点。每个提交都要读，结束时更新。

## 第 0 步：前置检查

1. `git fetch upstream`。
2. 校验：
   - 目标提交在 upstream/main 上：`git merge-base --is-ancestor <commit> upstream/main`。
   - 工作区干净：`git status --porcelain` 输出为空。
   - 当前分支不是 `main`。
   - 目标提交尚未合入：`git merge-base --is-ancestor <commit> HEAD` 返回非 0。

   任一不满足就停止，并说明原因。
3. 列出待合并提交：`git rev-list --reverse $(git merge-base HEAD upstream/main)..<commit>`。
4. 待合并的不止 1 个时，用 AskUserQuestion 让用户选：
   - 按顺序逐个处理全部（推荐）
   - 只处理最早的 1 个
   - 取消

   每个提交单独走完第 1–7 步，再处理下一个。多个提交不合成一个 merge。

**完成标准**：待处理队列已确定，并且已告知用户（`1/N` 形式）。

## 第 1 步：读约束

读 `ARCHITECTURE.md` 全文和台账全文。处理同一批次的后续提交时，台账要重新读，因为上一个提交可能已经改了它。

## 第 2 步：分诊（先出计划，再动代码）

1. 弄清上游意图：
   - 读 `git show --stat --format=%B <sha>` 和 `git show <sha>`。
   - 提交标题带 `(#NNN)` 时，可以用 `gh pr view NNN --repo tbphp/gpt-load` 读 PR 描述（只读）。
2. 为 `git show --name-status --format= <sha>` 里的**每个文件**定一个处置：
   - **保留**：可以原样落地，不触碰任何不变量。
   - **适配**：落地，但要按 ARCHITECTURE.md 的某一节改写。写明章节和改法。
   - **重编号**：上游迁移。按 §5 配方追加到 dev 下一个空号；台账里已有映射的，按映射找到 dev 文件。
   - **丢弃**：属于 dev 已删除的形态（classic、SQLite/MySQL、本地文件、Windows/原生二进制、journal 等）。写明理由。
   - **重写测试**：上游测试改成 `pgtest`/miniredis 版本，或改成验证 dev 适配后的行为。
3. 对照台账的“已知重叠点”和“概念映射”。已有决定的，直接照做。
4. **决策门**：遇到下列情况，用 AskUserQuestion 让用户拍板。问题里给出推荐项和各选项的后果。
   - 上游功能和 dev 已有功能语义重叠，选哪种会改变产品行为。例如上游的 AccessKey/分组/全局并发与 dev 的凭据并发；“全局上限”是集群级还是每副本级。
   - 适配要改 dev 已有的对外契约（API 字段、计费口径、错误码）。
   - 上游功能和 dev 部署形态根本冲突，只能**整体丢弃一个功能**。例如二进制自更新。

   其他情况按 ARCHITECTURE.md 的默认适配方式自行决定，并在提交说明里写清楚。

**完成标准**：处置表覆盖 `--name-status` 列出的每一个文件；每条“适配”都指向 ARCHITECTURE.md 的具体章节；决策门的问题都已有答案。把处置表展示给用户后再进入第 3 步。

## 第 3 步：合并并落地处置

1. 按处置选合并方式：
   - 全部文件都是“丢弃”：`git merge -s ours --no-ff --no-commit <sha>`。只记录祖先关系，不带入内容；直接跳到第 6 步，跳过验证。
   - 其他情况：`git merge --no-ff --no-commit <sha>`。
2. 逐文件落地处置，**包括 git 没有报冲突、自动合入的文件**：
   - 无冲突新增的 classic 文件、上游迁移、`*_checkpoint.go` 等，正是最容易漏掉的地方。
   - dev 已删除、上游修改过的文件（modify/delete）：保持删除（`git rm`）。上游改动里有 dev 替代实现也需要的行为时，把它移植到替代实现里。
   - 冲突解决以 dev 的结构为骨架，把上游的行为改动嵌进去。不要整块选 `--theirs`。
   - `go.mod`/`go.sum` 冲突：接受上游的版本升级，然后运行 `go mod tidy`。
   - 前端 lock 文件冲突：以 dev 的 `web/package.json` 为准，运行 `pnpm --dir web install` 重新生成。
3. 迁移重编号：用 `git mv` 改名，再改标识符、注册表、`wantIDs` 和测试里的索引切片。
4. 补测试：每个被适配的行为都要有测试覆盖 dev 的语义。例如 Redis 版共享状态，照 `internal/cluster/*_test.go` 的写法。
5. 同步契约：
   - `docs/openapi.yaml`（端点在其覆盖范围内时）。
   - 三份 README。
   - `.env.example`。
   - 路由契约测试。
   - `logOperations`。
   - workflow/compose 的契约测试常量。

**完成标准**：

- `git diff --name-only --diff-filter=U` 为空。
- 处置表里的每一行都已落地。
- `go build ./...` 通过。

## 第 4 步：架构审计

1. 运行 `bash .claude/skills/upstream-merge/audit.sh`。每条命中都要对照 ARCHITECTURE.md 判定：要么修掉，要么写下它合法的理由（例如它是派生镜像，或是本副本自身的记账）。
2. 脚本只做机械匹配。再按 ARCHITECTURE.md §0 的总纲问一遍：**放到 3 个副本后面，这次落地的行为还对吗？** 逐个检查新增的状态、后台任务、配置写入和金额字段。

**完成标准**：每条命中都已修复或附带理由；ARCHITECTURE.md 的 §1–§11 每节都对照 `git diff --cached HEAD` 检查过。

## 第 5 步：验证与独立审查

1. 运行 `make check`，需要 Docker 起测试依赖。它必须全绿。
   - 失败时修复后重跑。
   - 不要跳过或删除测试来过关。
   - 删除测试只有一个合法理由：它测的是已丢弃的上游功能，且这一点已写进处置表。
2. 派一个全新上下文的审查 subagent（`prp-core:code-reviewer`）。给它：
   - 上游提交 sha。
   - 读 `.claude/skills/upstream-merge/ARCHITECTURE.md` 的指令。
   - 第 2 步的处置表。

   要求它审查 `git diff --cached HEAD`。它要回答两件事：有没有违反不变量的地方；上游意图有没有被完整保留，丢弃的部分是否都有理由。
3. 处理审查发现：
   - 成立的：修复后重跑 `make check`。
   - 不成立的：在报告里说明理由。

**完成标准**：`make check` 全绿（贴出结尾输出）；审查发现都已修复或已反驳。

## 第 6 步：提交

1. 更新台账，和合并内容一起暂存：
   - 新的迁移编号映射。
   - 新形成的长期决定，例如“上游 X 概念在 dev 一律映射为 Y”。
   - 移除已处理掉的“已知重叠点”。
2. 提交 merge commit，只在本地提交，不 push：

```text
merge(upstream): <上游提交标题> (<sha7>)

Upstream: <完整 sha>

适配：
- <文件或模块>：<按 ARCHITECTURE.md §N 做了什么>

丢弃：
- <路径或功能>：<理由>

决策：
- <决策门的问题与用户的选择；没有就写“无”>

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
```

**完成标准**：`git log -1` 是一个以上游提交为第二父提交的 merge commit；`git status --porcelain` 为空。

## 第 7 步：汇报，然后进入下一个

每个提交汇报一段，包含：

- 进度（`k/N`）。
- 上游提交。
- 保留、适配、丢弃各几项，以及关键适配点。
- 决策。
- `make check` 结果。
- 新的 merge commit sha。

队列还有提交时，回到第 1 步。全部完成后，提醒用户合并结果只在本地，push 由用户决定。

中途要放弃当前提交时（例如用户在决策门选择取消），执行 `git merge --abort`，回到合并前的状态，然后报告停在哪个提交。

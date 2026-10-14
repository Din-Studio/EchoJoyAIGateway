# 控制台团队切换器（请求级作用域收窄）

- 日期：2026-10-13（同日经第一性原理 / 消融 / 奥卡姆审查修订一次，见第 7 节）
- 分支：`feat/user-team-casdoor-support`
- 状态：Phase 0–3 已实施，本地验证与 xlqy 端到端验证均通过（2026-10-14，见第 9 节）
- 前置：`2026-10-12-casdoor-oidc-user-team.md` 的 4 个必做阶段已完成
- 交付：Phase 0 + 后端 + 前端 + 文档一次提交，本地验证（`go test ./internal/control/... ./internal/requestlog/...`、web lint/type-check、手动端到端）作为合并门禁

## 1. 背景

现状：

- 控制台没有"当前团队"概念。作用域是**并集**：`internal/control/access_scope.go:21` 的 `scopedAccessKeyIDs` 对平台管理员返回 nil（全部），对其他人返回 `team_id IN adminTeams OR owner_user_id = me`。
- 8 个处理器调用这一个函数：access-keys 列表（`access_key_collection_http.go:32`）、options（`server.go:909`）、home（`home.go:207`）、home statistics（`home_statistics.go:120`）、usage（`usage.go:132`）、logs 列表/详情（`request_logs.go:293`、`:327`）、models（`project_model_collection_http.go:16`）。
- **`scope == nil` 被同时当作"平台管理员视图"的开关**（审查发现，原稿遗漏）。纯过滤语义的位置不受影响，但以下位置用 nil 决定"是否展示管理员专属数据"：
  - `internal/requestlog/home_statistics.go:141`：非 nil 时不查分组排行。
  - `internal/control/project_model_collection.go:211`、`:308`：非 nil 时隐藏路由分组与受影响分组。
  - `internal/control/project_model_collection_query.go:123` 与 `project_model_collection_http.go:21`：非 nil 时强制 `group_status=enabled`；`ModelsView.vue:42` 对平台管理员发 `group_status=all`，因此平台管理员一旦带作用域访问模型页会得到 400。
  - `home.go:150`、`:172` 与 `project_model_collection.go:222` 也判 nil，但语义是"按这些 key 可达范围计数/过滤"，对平台管理员选团队后仍然正确，不需要改。
- Principal 由 `internal/control/auth.go:69` 的 `authenticate()` 每请求从会话加载，字段只有 `Teams / Platform / AdminTeams`。
- 前端会话状态在 `web/src/frontends/modern/features/auth/auth-session.ts`；请求头统一在 `web/src/shared/http/client.ts:222` 注入（已有 `Accept-Language` 先例）。
- 原计划文档第 8 节第 503 行写明"不做团队切换器"。

目标：

- 同时属于多个团队的用户（含平台管理员）可以把控制台收窄到一个团队，只看该团队的 key、用量、日志、模型。
- 控制台总是处于一个团队视角（默认第一个可选团队，见第 10 节）；平台管理员选团队后仍是管理员视图（分组排行、路由分组、`group_status=all` 照常）。

## 2. 已确认的决策

| 主题 | 决策 | 理由 |
|---|---|---|
| 路线 | 全局切换器，而非仅在 access-keys 列表加团队筛选 | 需要首页、用量、日志、模型一并按团队收窄 |
| 传递方式 | 请求头 `X-Team-Scope`，不用 URL 参数、不存服务端会话 | 一处注入覆盖所有作用域接口；不新增会话状态和写接口；与"团队以 Casdoor 为准、不建本地表"约束兼容 |
| 作用域语义 | **选了团队 = 现有作用域 ∩ `team_id = T`** | 一条规则覆盖所有身份；成员只看自己在 T 的 key 是交集的必然结果，不是额外决策 |
| 管理员视图 | 从 `scope == nil` 改为显式的 `principal.Platform` | 拆开"能看哪些 key"和"是不是管理员视图"两个概念，是切换器的前提 |

## 3. 核心设计

### 3.1 作用域规则

`team` 为空表示不收窄（现行为），只在平台管理员且 Casdoor 没有任何团队时出现；控制台总是选中一个团队。非空时：

- 平台管理员：`team_id = T`（从"返回 nil"改为真正查询）。
- 其他人：现有 `Where` 之上追加 `team_id = T`，即 `team_id = T AND (team_id IN adminTeams OR owner_user_id = me)`。

推论：不在 T 中的人选 T，交集为空（或只剩自己已脱离成员身份但仍归属 T 的旧 key），没有泄漏，所以**不需要 403**。

约束不变：零命中返回非 nil 空切片（原文档 E1）；交集为空仍编译为 disabled；`FilterSet` 语义不动。

### 3.2 后端契约

- 输入：请求头 `X-Team-Scope: <team name>`（Casdoor group 名，不含组织前缀，与 `Principal.Teams` 同一命名空间）。只在 `requestScope(c)` 里读取，平台专属路由不读取，因此天然不受影响。
- 不校验团队是否存在于 Casdoor，不存在时自然为空结果。
- `usage` / `logs` 的 `access_key_id` 参数继续走 `scopeAccessKeyFilter`，参数不在收窄后的范围内 → 404（现行为）。
- `authorizeAccessKeyCreate` 不变：创建 key 的归属团队由请求体 `team_id` 决定，头只影响读取范围。
- 管理员视图标志：`requestlog.HomeStatisticsQuery` 与 `ProjectModelListQuery` 新增 `PlatformView bool`，由处理器从 `principal.Platform` 赋值；`ScopeAccessKeyIDs` 退化为纯过滤。

### 3.3 前端契约

- 可选团队：`/api/auth/session` 返回 `selectable_teams: [{name, display_name}]`，平台管理员为 Casdoor 全部团队，其他人为自己所属的团队。会话模块不依赖团队管理 API。
- 状态：`auth-session.ts` 的 `state` 新增 `team: string` 与 `selectableTeams`，`team` 持久化到 `localStorage['gpt-load.modern.team']`。
- 默认团队：`ensureValidated` 成功后，若存储的 `team` 不在 `selectable_teams` 里，取第一个。没有"全部团队"选项。
- 切换：`selectTeam(team)` 写 state 与 localStorage，然后 `queryClient.clear()`（与 `clear()` 同款，避免旧团队数据闪现）。
- 注入：`client.ts` 在 `Accept-Language` 同位置加 `X-Team-Scope`，为空不发。
- 显示：顶栏账号名旁 `AppSelect`，选项直接映射 `selectableTeams`（`display_name || name`）。

## 4. 分阶段实施

### Phase 0 拆开 `scope == nil` 的双重语义（~1 h）

1. `internal/requestlog/home_statistics.go`：`HomeStatisticsQuery` 加 `PlatformView bool`；`:141` 改为 `if input.PlatformView`。`internal/control/home_statistics.go:120` 处赋值 `principal.Platform`。
2. `internal/control/project_model_collection_query.go`：`ProjectModelListQuery` 加 `PlatformView bool`；`:123` 校验改为 `!query.PlatformView && GroupStatus != Enabled`。
3. `project_model_collection_http.go:21`：`scope != nil` 分支改为 `!principal.Platform` 时强制 enabled；始终赋值 `ScopeAccessKeyIDs = scope`、`PlatformView = principal.Platform`。
4. `project_model_collection.go:211`、`:308`：`ScopeAccessKeyIDs == nil` 改为 `query.PlatformView`。`:222` 的价格过滤保持按 `ScopeAccessKeyIDs` 判断。
5. 测试：现有用例全部不变（此时 `PlatformView == (scope == nil)`，行为等价）；新增"平台管理员 + 非 nil 作用域"用例各一：统计仍含分组排行，模型页接受 `group_status=all` 且含路由分组。

验证：`go test ./internal/control/... ./internal/requestlog/...`。此阶段单独可提交，行为无变化。

### Phase 1 后端作用域收窄（~1 h）

1. `access_scope.go`：`scopedAccessKeyIDs(ctx, p, team string)` 按 3.1 实现；`scopeAccessKeyFilter` 透传 `team`。
2. 新增 `func (s *Server) requestScope(c *gin.Context) ([]uint, error)`：取 principal、读 `X-Team-Scope`、算 scope。替换第 1 节列出的 8 个调用点里重复的两行；`usage.go:132` 与 `request_logs.go:293` 用 `scopeAccessKeyFilter` 的对应版本。
3. 测试 `team_scope_test.go`：在 `TestTeamScopeLimitsWhichKeysEachRoleSees` 基础上加带团队的用例：平台管理员选 T 只见 T 的 key；A 管理员兼 B 成员选 A 见 A 全部、选 B 只见自己在 B 的 key；非成员选 T 得空切片；`usage` 带团队外的 `access_key_id` → 404。

验证：`go test ./internal/control/...`。

### Phase 2 前端（~1 h）

1. `auth-session.ts`：`team` 状态、读取/校正/`selectTeam`。
2. `client.ts` 创建处增加 `getTeam()` 依赖并注入头；`modern/app/api-client.ts` 接线。
3. `layouts/AppLayout.vue` 顶栏加 `AppSelect`。
4. i18n 三个 locale 加 `shell.teamScope`、`shell.allTeams`。

验证：web lint / type-check；手动端到端见第 6 节。

### Phase 3 文档（~15 min）

- `2026-10-12-casdoor-oidc-user-team.md` 第 8 节把"团队切换器"从"不做"列表移出，补一行指向本文档。

## 5. 刻意不做的事（YAGNI）

- 不在服务端会话存当前团队，不加切换接口。
- 不在中间件校验头、不返回 403：交集语义已保证无泄漏。
- 不建本地 users/teams 表。
- 不给平台专属页面（groups、teams 管理、settings）加团队过滤。
- 首版不做：`AccessKeyPanel` 新建 key 默认选中当前团队；按 `teams.length > 1` 隐藏切换器。两者不影响主流程，需要时各为几行改动。

## 6. 端到端验证清单

1. 平台管理员：全部 → 团队 A → 团队 B，access-keys 列表、首页统计、用量、日志、模型五处数据随之变化；**模型页仍可切 `group_status=all` 并显示路由分组，首页统计仍有分组排行**。选不存在的团队名（手工改 localStorage）得到空列表而非报错。
2. 团队 A 管理员且 B 成员：选 A 看到 A 全部 key；选 B 只看到自己在 B 的 key；"全部"为两者并集加自己其他 key。
3. 伪造头为非所属团队：`accessSession` 接口返回空结果，`accessPlatform` 接口行为不变。
4. 换账号登录后，上一账号的团队选择被清空；刷新页面后选择保留。

## 7. 审查与消融记录（2026-10-13）

| # | 实验 | 结果 | 采纳 |
|---|---|---|---|
| R1 | 追踪平台管理员作用域从 nil 变为非 nil 的影响 | 7 处判 nil；其中 4 处用 nil 当"管理员视图"开关，模型页会对平台管理员返回 400 | 新增 Phase 0，显式 `PlatformView` 标志 |
| R2 | 去掉 `authenticate()` 里的头校验、403、context key、"平台路由忽略该头"规则 | 头只在 `requestScope` 读取；非成员选团队交集为空，无泄漏 | 全部删除 |
| R3 | 用"现有作用域 ∩ `team_id = T`"替代四行规则表 | 非平台用户只多一个 `Where`；成员范围自动得出 | 删表，原"决策 2"消失 |
| R4 | 去掉全局切换器，只给 access-keys 列表加 `team_id` 筛选 | 绕开 R1 全部改动，但失去首页/用量/日志/模型按团队收窄 | 未采纳，用户选择全局切换器 |
| R5 | 用 query key 含 team 替代 `queryClient.clear()` | 需改 8 个 API 函数与 key | 保留 `clear()` |
| R6 | 去掉新建 key 默认团队与 `teams.length > 1` 隐藏条件 | 不影响主流程 | 移入"首版不做" |

## 8. 实施记录（2026-10-14）

- Phase 0：`PlatformView` 加入 `requestlog.HomeStatisticsQuery` 与 `ProjectModelListQuery`，4 处 `scope == nil` 改为该标志；`home.go` 与价格过滤保持按范围判断。新增"平台管理员 + 非 nil 范围"用例：统计仍含分组排行，模型页接受 `group_status=all` 且保留路由分组。
- Phase 1：`scopedAccessKeyIDs(ctx, p, team)` 按"现有作用域 ∩ `team_id = T`"实现；`Server.requestScope` / `requestScopeFilter` 读取 `X-Team-Scope` 并替换 8 个调用点。新增 `TestSelectedTeamOnlyNarrowsTheScope`：7 种身份 × 团队组合、团队外 `access_key_id` → 404、平台路由不受头影响。
- Phase 2：`auth-session.ts` 增加 `team` 状态与 `selectTeam`（localStorage 持久化、换账号校正、`queryClient.clear()`）；`client.ts` 注入请求头；`AppLayout.vue` 顶栏加团队下拉；三语文案。
- 验证：`go test -count=1 . ./internal/...` 全绿；web `lint`、`format`、`vue-tsc`、`build` 通过。

## 9. 测试环境部署与端到端验证记录（2026-10-14）

- 镜像：`echojoy-ai-gateway:test-teamscope-20261014b`，xlqy 三副本滚动更新；原 `.env` 备份为 `.env.bak-20261014`。
- 端到端发现并修复一处前端缺陷：`selectTeam` 原用 `queryClient.clear()`，已挂载的查询不会重新请求，切换后列表不变、刷新后才正确。改为 `queryClient.resetQueries()`（重置并重新拉取活跃查询），修复版 `…20261014b` 验证通过。第 7 节 R5 的结论随之修正：`clear()` 与"query key 含 team"并不等价，`resetQueries()` 才是。
- 测试数据：为 `gw-test-b` 授权分组 1，并以 demo 创建 key `alice team-b key`（归属 gw-test-b，owner gw-alice）。测试用户 gw-alice / gw-m1 的密码经 Casdoor `set-password` 重设为与 demo 相同（`update-user` 直接写 `password` 字段不会按组织哈希方式存储，登录静默失败）。
- 第 6 节各项结果：
  1. demo（平台管理员）：下拉含 Casdoor 全部团队（显示名）；选 Gateway test A 后列表只剩 `m1 e2e key`，`/api/home`、统计、用量、日志、模型（`group_status=all`）均 200 且按团队收窄，统计仍含分组排行、模型仍含路由分组；选 Gateway test B 只剩 `alice team-b key`；回到全部为 3 条；头为不存在的团队返回空列表；`/api/teams` 不受头影响。
  2. gw-alice（gw-test-a 管理员、gw-test-b 成员）：选 gw-test-a 见 `m1 e2e key`；选 gw-test-b 只见自己的 `alice team-b key`；全部为两者并集；`usage?access_key_id=2` 在 gw-test-b 下 404。
  3. 伪造头：gw-m1 带 `gw-test-b` 得空列表；alice 带非所属的 `demo_group` 得空列表；gw-m1 访问 `/api/teams` 仍 403。
  4. alice 选中 gw-test-b 后退出，gw-m1 登录时存储的选择被清空，下拉回到"全部团队"且只列 gw-test-a；刷新页面后选择保留（demo 场景已验）。

## 10. 修订：移除"全部团队"，默认选第一个团队（2026-10-14）

- 需求：控制台总是处于某个团队视角，不再提供并集视图。
- 前端：会话校验时确定团队（平台管理员一并拉取 `/api/teams`，列表存入 `state.platformTeams`，`AppLayout` 不再自己查询）；存储的团队不在可选范围时取第一个。`shell.allTeams` 文案删除。
- 后端不动：空头仍表示不收窄，仅在平台管理员且 Casdoor 没有团队时出现。
- 镜像：`echojoy-ai-gateway:test-teamscope-20261014c`。
- xlqy 验证：gw-alice 登录默认 gw-test-a，选项只有 gw-test-a / gw-test-b；切到 gw-test-b 只见自己的 key；退出后 demo 登录，存储的 gw-test-b 对平台管理员仍有效故保留，选项为 Casdoor 全部团队（无"全部团队"）。

## 11. 内聚 / 耦合审查与修订（2026-10-14）

- 发现：第 10 节的实现让 `auth-session.ts` 在校验阶段调用 `/api/teams`，会话可用性被耦合到 Casdoor 组织 API（失败会把控制台打到 `invalid-response`），同一列表与 `TeamsView`、`AccessKeyPanel` 各拉一份，`AppLayout` 需要三元分支处理两种形状。
- 修订（消融 A）：`/api/auth/session` 返回 `selectable_teams`（平台管理员取 `identity.Teams()`，其他人由 `Teams` 投影）；前端会话只依赖 auth API，`AppLayout` 的 `teamOptions` 直接映射，`showTeamScope` 删除；`ApiClientDependencies.getTeam` 改为必填。
- 其他消融：`requestScopeFilter` 保留；`selectTeam` 同值早返回保留（无害）。
- 镜像：`echojoy-ai-gateway:test-teamscope-20261014d`。
- xlqy 验证（`…20261014d`）：gw-alice 的 `selectable_teams` 为 gw-test-a / gw-test-b，清掉存储后默认 gw-test-a，切 gw-test-b 只见自己的 key；demo 的 `selectable_teams` 为 Casdoor 三个团队（带显示名），默认 Gateway test A，切 Gateway test B 只见 `alice team-b key`。

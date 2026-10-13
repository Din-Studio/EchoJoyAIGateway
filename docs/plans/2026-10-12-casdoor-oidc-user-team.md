# 网关接入 Casdoor（标准 OIDC RP）+ 用户 / 团队 / 角色 / 权限

- 日期：2026-10-12（2026-10-13 经对抗性审查四次修订、测试实例实测和实施前复查，见第 9 节）
- 分支：`feat/user-team-casdoor-support`
- 状态：4 个必做阶段已实施并通过本地验证与 xlqy 端到端验证（2026-10-13，见第 10 节）；Phase 5 未做
- 交付：4 个必做阶段 + 1 个可选阶段，全部完成后合并为一个 PR；以本地验证（`make check` 和手动端到端）作为合并门禁

## 1. 背景

现状：

- 管理端只认共享的 `AUTH_KEY`。前端把它存在 localStorage/sessionStorage（键名 `gpt-load.auth-key`），每次请求带 `Authorization: Bearer`。
- 后端 `internal/control/auth.go` 用 SHA-256 摘要比对 `AUTH_KEY`，再按对端 IP 做失败锁定。
- access key 也能当控制台只读凭证使用，可访问的路由由 `accessKeyControlRoutes` 白名单限定。
- 系统里没有用户、团队、组织、权限的概念。审计日志（`mutation_audit.go`）只记录 `peer_ip`。

目标：

- 网关成为 Casdoor（`http://192.168.48.64:8000`，组织 `echojoy`）的标准 OIDC Relying Party，采用 BFF 模式：Go 后端完成 Authorization Code + PKCE，会话存 Redis，SPA 只持有 httpOnly cookie。
- 引入用户、团队、角色和权限，并实现团队级数据隔离。

## 2. 已确认的决策

| 主题 | 决策 |
|---|---|
| 团队 | Casdoor 组织 `echojoy` 下的 Group，**一个团队只对应一个 Group**，不建子组。团队成员就是该 Group 的直接成员。一个用户可以属于多个团队。 |
| 团队管理员 | 用 casbin **RBAC with tenants** 表达，租户（domain）就是团队。每个团队一条 Permission `gateway_<team>`：`domains=[<team>]`，`users` 直接选该团队的管理员。可以做到逐团队授权，例如 alice 管理 team-a，但在 team-b 只是成员。 |
| 权限定义 | 标准 Casdoor 权限体系：Model（RBAC with tenants）+ Permission。由 Casdoor 内的 casbin 判定：网关在登录和每 5 分钟刷新时调用一次 `/api/batch-enforce?modelId=echojoy/gateway_rbac_tenant`，不在每次请求时调用。 |
| 分组（上游渠道 + 凭证） | 只有平台管理员能管理。平台管理员给每个团队**授权可用分组**。 |
| 团队管理员能力 | 为团队成员创建 access key，设置费用配额规则和 RPM，可选分组限于本团队的授权；读取团队内的 key、日志和用量。 |
| 成员能力 | 通过 Casdoor 登录后，查看自己的 key（可 reveal）、日志和用量。 |
| `AUTH_KEY` | **彻底移除**。 |
| 用 access key 登录控制台 | **移除**。 |
| 团队总预算 | 本期不做。 |
| Back-channel logout | 放到可选的 Phase 5。 |

## 3. Casdoor 事实（已在源码 `/Users/fanlz/Projects/NanoBoom/casdoor` @ `fd7295a` 中核实）

- **端点**（`object/wellknown_oidc_discovery.go:133,160-178`）：
  - authorize `<origin>/login/oauth/authorize`，token `<origin>/api/login/oauth/access_token`，JWKS `<origin>/.well-known/jwks`，end_session `<origin>/api/logout`。网关直接按这些路径静态配置，运行时不做 discovery。
  - issuer 取 `conf/app.conf` 中的 `origin`；该项为空时取请求的 Host，因此部署时要固定 `origin`。
  - PKCE 只支持 `S256`。
- **登出**（`controllers/account.go:410-434`）：`id_token_hint` 可选。只带 `client_id` 和 `post_logout_redirect_uri` 时，Casdoor 清除当前浏览器会话，并按应用的 `redirectUris` 校验后重定向。
- **Token 声明**（`object/token_jwt.go`）：`sub` 是 `user.Id`（UUID），`aud=[clientId]`。
- **用 client 凭证调用 API**：HTTP Basic `clientId:clientSecret`，身份视为**所在组织的管理员**。
  - `GET /api/get-user?userId=<sub>&owner=echojoy`：走 `GetUserByUserId(owner, userId)`（`controllers/user.go:158`、`object/user.go:600`），按组织和 `user.Id` 精确查找，不受改名影响；用户不在该组织时返回 `data=null`。
  - 返回值中的 `groups` 是用户直接所属的组，形如 `["echojoy/team-a"]`；`isForbidden` 表示用户被禁用。
  - `GET /api/get-groups?owner=echojoy` 返回 Group 列表。
  - `GET /api/get-users?owner=echojoy&groupName=<g>` 返回某组的直接成员（`object/group.go:409`）。
  - 返回信封统一为 `{status:"ok"|"error", msg, data, data2}`。
- **User / Group / Role 原生关系**：
  - 组成员关系只存在 `User.groups[]` 上，没有属性。
  - `parentId` 构成的组树不参与授权解析。
  - Role 是全局的。`Role.domains` 字段在源码中没有被任何地方引用。
- **casbin 判定**（`controllers/casbin_api.go`、`object/permission_enforcer.go`）：
  - `POST /api/batch-enforce?modelId=<org>/<model>`：取出该 Model 下的全部 Permission，按 model 和 adapter 分组，每组装配一个 enforcer，逐条判定请求。返回 `data`，每组对应一个 bool 数组。
  - 用 client 凭证调用时，鉴权对象是 Model 所属的组织（`routers/authz_filter.go:302-308`）。
  - **测试实例实测**（2026-10-13）：
    - 组织应用的 client 凭证（Basic auth）调用 `batch-enforce?modelId=…` 和 `?permissionId=…` 均可用。
    - 组织管理员的浏览器会话调用这两种方式会返回 `Unauthorized operation`，说明部署版本的会话鉴权与本地源码不同。网关只使用应用凭证，不受影响。
    - `?owner=echojoy` 会把组织内所有 Permission 一起判定。只要组织里有字段数不同的 Model（例如默认的 3 字段模型），整次调用就会报 `invalid request size`，因此**只能用 `modelId`**。
  - 每次调用都会新建 enforcer：先按 Permission ID 加载策略，再在运行时生成分组策略。开销比 get-user 大，因此不适合每个请求都调用。
  - 策略生成规则（`getPolicies`）：每个主体（users、`group:`+groups、roles）分别与 resources、actions、domains 做笛卡尔积。
  - 运行时分组策略（`getRuntimeGroupingPolicies`）：Role 的每个用户分别与 Permission 的每个 domain 组合；组的每个直接成员都会成为该组主体。
  - Permission 的 `effect` 在外部调用 enforce 时不起作用；allow/deny 要在 Model 中表达。action 存储时会转成小写。
  - Permission 直接授予用户时，策略的 `sub` 就是用户本人。casbin 的 `HasLink(name1, name2)` 在两者相同时恒为 true（`casbin/v2 rbac/default-role-manager/role_manager.go:348`），所以带租户的 matcher `g(r.sub, p.sub, r.dom)` 也能匹配。
  - 不同结构的 Model 应使用各自的 adapter 表，否则加载策略时会冲突（官方文档《权限配置》）。
  - 用户改名时，Casdoor 会同步改写 `Permission.users`（`userChangeTrigger`），但不会重写 adapter 表里已有的策略行。改名后需要重新保存一次相关 Permission。
  - 另一条路是 Casdoor 的 Enforcer 对象（Model + Adapter），可以在策略表里直接编写 `p`/`g` 行（`web/src/pages/EnforcerEditPage.tsx`）。但用户名要手工输入，改名时也不会同步，所以不采用。
- **组成员的维护入口**：在用户编辑页给用户勾选所属 Group；Group 编辑页只读展示成员。
- **Back-channel logout** 的 `logout_token` 不带 `exp`，通常也不带 `sid`，因此只能按 `sub` 处理。
- **测试实例现状**（2026-10-13）：
  - `echojoy` 组织约有 3090 个用户，多数来自另一产品的迁移。demo 是组织管理员。
  - 已有应用 `app-go-oidc-demo` 和 `app-go-oidc-demo-2`。
  - 已有 Group `demo_group`、Role `team-admin`、Permission `admin-permissions`（与本方案无关，不要改动）。没有 Model、Adapter 和 Enforcer。
- **应用凭证的身份**（`object/user_util.go:1119`）：视为 `{Owner: 应用所属组织, IsAdmin: true}`，因此 get-user 返回完整的 `groups`，不受账户字段可见性规则影响。
- **不带 `id_token_hint` 的登出**：只有动态客户端或跨组织客户端会在会话中写入 `aud`（`controllers/auth.go:2015-2023`）。网关应用两者都不是，所以 Casdoor 按 `client_id` 找到网关应用并校验 `post_logout_redirect_uri`。

## 4. 核心设计

### 4.1 身份与会话（新包 `internal/identity/`）

**OIDC 端点**：由 `OIDC_ISSUER` 静态拼出 `oauth2.Endpoint`，用 `oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(context.Background(), jwksURL), &oidc.Config{ClientID})` 校验 ID token。启动时不访问网络，JWKS 在首次校验时拉取，因此 Casdoor 故障不会阻止数据面启动，container 测试也不需要外网。

**登录**：`GET /auth/login?redirect=<相对路径>`

- `redirect` 必须以 `/` 开头且不以 `//` 开头，否则改为 `/`。
- 生成 `state`、`nonce` 和 PKCE verifier，存入 Redis：`Key("oidc-login", state)`，TTL 10 分钟，取出时用 `GETDEL` 保证一次性。
- 同时下发 cookie `gl_login`（随机值，HttpOnly，Path=/auth，TTL 同上），其 SHA-256 一并存入登录状态；回调时比对，把 state 绑定到发起登录的浏览器，避免攻击者把自己的回调 URL 发给受害者（login CSRF）。
- 302 跳转到 Casdoor authorize，scope 为 `openid profile email`。

**回调**：`GET /auth/callback`

1. 用 `GETDEL` 取出登录状态，比对 `gl_login` cookie 的摘要，再用 code 加 verifier 换取 token。回调无论成败都清除 `gl_login`。
2. 用 verifier 校验 id_token 的签名、`iss`、`aud`，再比对 `nonce`。
3. 用 client 凭证调用 `get-user?userId=<sub>&owner=CASDOOR_ORGANIZATION` 和 `batch-enforce`（见 4.2 节），编译出 `Principal`。组织归属由 get-user 的查询条件保证，不再单独校验 `owner` 声明。
4. 用户不存在、被禁用（`isForbidden`）或没有访问资格时 302 到 `/login?error=no_access`，其他失败 302 到 `/login?error=login_failed`。
5. 成功后创建会话，设置 cookie `gl_session`（HttpOnly，SameSite=Lax，Path=/；`PUBLIC_URL` 为 https 时加 Secure），302 回到 `redirect`。

**会话**：

- Redis 键 `Key("sess", sha256hex(cookie))`，值为 `{Principal, CheckedAtMS}`，绝对 TTL 12 小时。
- 更新会话必须用 `SET key value XX KEEPTTL`：只在键仍存在时写入，并保留原过期时间。
  - 如果漏掉 `XX`：登出删除会话的同时，另一个并发请求正在刷新，刷新写回会把会话重新建出来，而且 `KEEPTTL` 作用在不存在的键上会让它永不过期。
- **不保存 refresh_token 和 ID token**。

**权限刷新**：每次认证时，若 `now - CheckedAtMS > 5 分钟`，按 `Principal.UserID` 重新调用 `get-user` 和 `batch-enforce` 并重新编译：

- 用户不存在、被禁用或失去访问资格：删除会话，返回 401。
- Casdoor 不可达或返回错误：继续用缓存的 Principal，记 warn，**同时把 `CheckedAtMS` 更新为当前时间**，下一个 5 分钟窗口再重试。
  - 否则 Casdoor 故障期间，每个控制面请求都要先等一次刷新超时。
  - Casdoor 客户端的 HTTP 超时设为 5 秒。
- 效果：在 Casdoor 中修改的权限，最多 5 分钟后生效。
- 每个会话每 5 分钟只有 2 次 Casdoor 调用；不做进程内缓存。

**登出**：`POST /auth/logout`

- 校验 Origin，删除会话，清除 cookie。
- 返回 `{redirect_url}`，指向 `<OIDC_ISSUER>/api/logout?client_id=<OIDC_CLIENT_ID>&post_logout_redirect_uri=<PUBLIC_URL>/login`。

### 4.2 角色、权限与 casbin 判定（`internal/identity/principal.go`）

**Model `gateway_rbac_tenant`**（RBAC with tenants，domain 就是团队名）：

```ini
[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, dom, obj, act

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub, r.dom) && r.dom == p.dom && r.obj == p.obj && r.act == p.act
```

**Casdoor 侧对象**（名字前缀和 action 是代码常量）：

| 对象 | 何时配置 | 配置 |
|---|---|---|
| Adapter `gateway_rbac_tenant` | 一次 | `useSameDb=true`，`table=gateway_permission_rule`，与其他 Model 的策略表隔离 |
| Model `gateway_rbac_tenant` | 一次 | 上方模型文本 |
| Permission `gateway_platform` | 一次 | users=[平台管理员]，domains=[`*`]，resources=[`gateway`]，actions=[`platform:manage`] |
| Group `<team>` | 每个团队 | 成员 |
| Permission `gateway_<team>` | 每个团队 | users=[该团队管理员]，domains=[`<team>`]，resources=[`gateway`]，actions=[`team:manage`] |

上述 Permission 都设置 `model=gateway_rbac_tenant`、`adapter=gateway_rbac_tenant`。

`team:manage` 的含义：在该团队内创建、分配、轮换和删除 key，设置配额和 RPM，并查看团队的 key、日志和用量。`*` 只是平台权限所用 domain 的字面值，不表示通配；Group 名称里不会出现 `*`。

**判定调用**（登录和每 5 分钟刷新时各一次）：

```
POST /api/batch-enforce?modelId=echojoy/gateway_rbac_tenant
[["echojoy/<name>", "*",      "gateway", "platform:manage"],
 ["echojoy/<name>", "team-a", "gateway", "team:manage"],    ← user.groups 中的每个团队各一条
 ["echojoy/<name>", "team-b", "gateway", "team:manage"]]
→ data: [[<platform>, <team-a>, <team-b>]]（每个 model/adapter 组一行，网关按列取 OR）
```

**编译规则**（`compilePrincipal(user, decisions) Principal`）：

| 输入 | 结论 |
|---|---|
| `user.groups` 去掉组织前缀 | `Teams` |
| `*` 上的 `platform:manage` 为 true | `Platform = true` |
| 团队 T 上的 `team:manage` 为 true | T 加入 `AdminTeams` |
| 访问资格 `HasAccess()` | `Platform \|\| len(Teams) > 0` |

- 只对用户所在的团队发起判定。所以即使 Permission 里列了某人，只要他不是该组成员，也不会成为管理员，不需要额外代码。
- 网关不复制 casbin 的匹配逻辑；在 Casdoor 中修改 Model（例如加入 deny）会直接生效。
- **不要**让多个团队的 Permission 共用一个 Role：Casdoor 会把 Role 的每个持有人与每条 Permission 的每个 domain 做笛卡尔积，造成跨团队越权（第 9 节 E8）。

```go
type Principal struct {
    UserID      string   // Casdoor sub (user.Id)
    Name        string   // Casdoor user.name
    DisplayName string
    Email       string
    Teams       []string // 所属 group 名（不带 org 前缀）
    Platform    bool
    AdminTeams  []string // team:manage 判定为 true 的团队
}

func (p Principal) HasAccess() bool
func (p Principal) IsAdminOf(team string) bool // Platform 恒为 true
```

**成员的隐含权限**（不需要在 Casdoor 中配置）：可以读取 `owner_user_id == 自己` 的 key（包括 reveal）、对应日志和用量，以及首页和模型列表（按自己 key 的可用分组收窄）。

### 4.3 路由与访问声明

- 新增 `auth` 模块（`internal/identity/http_routes.go`）：`OwnerSystem`/`AuthNone`，命名空间 `/auth`。
  - 这个组合是 `validateOwnerAuth` 允许的（`internal/platform/httproute/registry.go:695`）。
  - `/auth` 前缀目前没有被任何模块占用。
- 控制面中，`controlRoute(name, method, path, access, handlers...)` 增加**内联**的 `access` 参数，在鉴权之后插入 `s.requireAccess(access)`。不另建 route→access 映射表，避免和路由声明脱节。

| access | 放行条件 |
|---|---|
| `accessSession` | 任何已登录的 Principal，由 handler 收窄数据 |
| `accessPlatform` | `Platform` |
| `accessTeamAdmin` | `Platform`，或 `len(AdminTeams) > 0`；具体的 key 或团队由 handler 校验 |

71 条现有路由的归属：

| 路由前缀（数量） | access | handler 内的收窄方式 |
|---|---|---|
| `control.auth.session` (1) | session | — |
| `control.credential-stages.*` (7) | platform | — |
| `control.channels.list` (1) | platform | — |
| `control.model-prices.*` (6) | platform | — |
| `control.home.subscription-accounts` (1) | platform | — |
| `control.health` (1) | platform | — |
| `control.route.inspect` (1) | platform | — |
| `control.settings.*` (2) | platform | — |
| `control.system.*` (2) | platform | — |
| `control.modern.*` (5) | platform | — |
| `control.groups.*` (10，含 options) | platform | — |
| `control.group-credentials.*` (18) | platform | — |
| `control.group-models.discover`、`control.models.discover` (2) | platform | — |
| `control.models.list` (1) | session | 可见 key 的可用分组并集 |
| `control.home`、`control.home.statistics` (2) | session | 可见 key 集合；非平台用户隐藏 TopGroups，并去掉 `CurrentAccessKey` |
| `control.logs.list/get` (2) | session | `AdminTeams` 的 key 加上自己的 key；非平台用户的结果脱敏 |
| `control.usage` (1) | session | `AdminTeams` 的 key 加上自己的 key；`SelfScoped=true`；`access_key_id` 参数必须在作用域内 |
| `control.access-keys.list/options/reveal` (3) | session | `AdminTeams` 的 key 加上自己的 key |
| `control.access-keys.create/update/rotate/delete/cost-limits.reset` (5) | teamAdmin | key 的 `team_id` 必须满足 `IsAdminOf` |

新增路由：

| 路由 | access | 用途 |
|---|---|---|
| `GET /api/teams` | platform | Casdoor 团队列表（`get-groups`），附带每个团队的分组授权 |
| `PUT /api/teams/:team/groups` | platform，审计 | 整体替换团队的授权，经 `writeConfig` 触发集群快照重载 |
| `GET /api/teams/:team` | teamAdmin，须 `IsAdminOf(team)` | 返回 `{groups:[{id,name}], members:[{id,name,display_name}]}`；成员来自一次 `get-users?groupName=<team>` 调用 |

**HTTP 状态约定**（前端收到 401 就会清除会话并跳转登录，见 `web/src/shared/http/client.ts:209`）：

| 情况 | 状态 |
|---|---|
| 没有会话或会话无效 | 401 |
| 已登录，但 access 级别不够（如成员访问平台路由） | 403（`ErrForbidden`） |
| 访问作用域外的具体资源（他人的 key、日志） | 404（`ErrResourceNotFound`） |
| 作用域为空（如还没有任何 key 的成员） | 200，返回空结果 |

现有的单 key 读取路径在 key 不存在或不是 active 时返回 `ErrUnauthorized`：`home.go:96,170,202`、`project_model_collection.go:368,372`、`request_logs.go:266`。Phase 3 必须全部改掉。否则还没有 key 的新成员，或 key 因授权撤销而被禁用的成员，一打开首页就会收到 401，然后在“登录 → Casdoor 单点登录自动返回 → 401”之间无限循环。

**CSRF**：cookie 设 SameSite=Lax；非 GET 请求要求 `Origin` 等于 `PUBLIC_URL` 的 origin。开发环境下 Vite 代理不改写 Origin，因此设置 `PUBLIC_URL=http://localhost:5173`。

### 4.4 数据模型与隔离

**迁移 `0020_teams`**（新增）：

- `access_keys` 增加以下列，均可空；`team_id` 为空表示平台 key。
  - `team_id varchar(255)`
  - `owner_user_id varchar(64)`
  - `owner_name varchar(255)`
  - 在 `team_id` 和 `owner_user_id` 上建索引。
- 新表 `team_group_grants`：
  - 列：`team_id varchar(255)`、`group_id bigint`（FK 到 `groups`，ON DELETE CASCADE）、`created_at_ms`。
  - 主键 `(team_id, group_id)`。

**key 的可用分组**，在快照编译时计算（`internal/state/loader/loader.go` 的 `mapAccessKeys`；三个调用点都经过它）：

- 平台 key：保持现状。
- 团队 key 的可用分组 = `key.groups ∩ grants[team]`；`key.groups` 为空时取 `grants[team]` 全部。
- **交集为空时，把该 key 编译为 `Status=disabled`**。`FilterSet.Groups` 保持现有语义（空即不限），调度器和网关的分组判断都不用改。
  - 原因：`filterDTO.toState` 会把空分组折叠成 nil，即"不限"。交集为空若不特殊处理就会 fail-open（见第 9 节 E2）。
  - 覆盖面：HTTP 路由在 `scheduler/inspect.go:105,339` 拒绝 disabled key；WebSocket 每轮在 `websocket.go:296` 要求 `active`。
  - 补一处：`internal/gateway/models.go` 对非 active 的 key 返回空模型列表。禁用 key 也受益。
- 撤销授权会经 `writeConfig` 触发快照重载，相关 key 的路由随即失效。对外表现与禁用 key 相同。

**作用域**（`internal/control/access_scope.go`）：

```go
type accessScope struct {
    all     bool     // Platform
    teams   []string // AdminTeams
    ownerID string   // UserID
}

func scopeOf(p identity.Principal) accessScope
func (s *Service) scopedAccessKeyIDs(ctx context.Context, scope accessScope) ([]uint, error) // all → nil；否则单条 SQL：team_id IN ? OR owner_user_id = ?
func (s *Service) authorizeAccessKey(ctx context.Context, id uint, p identity.Principal) error // 越权时返回 ErrResourceNotFound
```

- 在 `requestlog` 的 `ListQuery`（`types.go:82`）、`UsageQuery`（`types.go:211`）、`HomeStatisticsQuery`（`home_statistics.go:30`）和 `ProjectModelListQuery`（`project_model_collection_query.go:39`）中，把单个 `AccessKeyID *uint` 改为 `ScopeAccessKeyIDs []uint`，nil 表示不限。
- `scopedAccessKeyIDs` 必须是单条 `Pluck`：零命中时 GORM 返回非 nil 空切片（见第 9 节 E1），不能改成多次查询后 `append`，否则零命中会退化成 nil，也就是"不限"。用测试锁定"无 key 的成员看到 0 条日志"。
- 非平台用户的结果复用 `sanitizeAccessKeyRequestLog` 和 `SelfScoped`。
- 首页和模型列表按"作用域内 key 的可用分组并集"收窄。只取 snapshot 中 `Status == active` 的 key：
  - disabled key 的 `Filters.Groups` 为 nil，若参与并集会被当成"不限"，导致越权展示全部分组；
  - 作用域内没有 active key 时，返回空结果，不报错。
- 日志和用量不区分 key 状态。disabled key 的历史记录照常可见。

**key 写入规则**：

- 团队 key 必须同时有 `team_id` 和 `owner_user_id`；平台 key 两者都为空，只有平台管理员能创建。
- 团队必须满足 `IsAdminOf`。
- 创建团队 key 时，`grants[team]` 不能为空，否则返回校验错误"团队尚未授权分组"。避免建出来就是 disabled 的 key。
- `owner_user_id` 存 Casdoor 的 `user.id`（即 `sub`，与 `Principal.UserID` 一致），不存用户名。它必须是该团队成员，经 `TeamMembers` 校验；`owner_name` 也从这里取。
- `team_id` 和 `owner_user_id` 创建后不可修改：更新请求带了不同的值时返回校验错误。要换团队或负责人，就新建 key 再删掉旧的。这样不需要再处理"跨团队转移"的授权。
- `filters.groups` 必须是 `grants[team]` 的子集，在事务内校验。
- `price_multiplier` 只有平台管理员能设置（`access_keys.go:141,152`），否则把它设为 0 就能绕过配额。
- 日/月费用配额沿用现有的 periodic cost-limit rules（`period_seconds` 为 86400 或 2592000）。`access_keys.daily_cost_limit_nano_usd` 和 `monthly_cost_limit_nano_usd` 是死列，不用。
- idempotency digest 纳入新字段。

**已知限制**：

- access key 是硬删除（`access_keys.go:700-715`）。日志和用量中的 `access_key_id` 没有外键，会保留下来，但删除后只有平台管理员能看到这些记录。
- 在 Casdoor 中把成员移出团队，不会影响他已有的 key：数据面照常可用，需要团队管理员手动禁用或删除。网关不同步 Casdoor 的成员变更。

本期接受这两点，并写进文档。

**审计**：`mutation_audit.go` 增加 `actor_user_id` 和 `actor_name` 字段（只写日志，不改 schema）。

### 4.5 配置

`internal/platform/config/config.go`：

- 删除 `AuthKey`。
- 新增 `OIDCConfig`，下列项全部必填，缺失即启动失败：

| 环境变量 | 示例 |
|---|---|
| `PUBLIC_URL` | `http://localhost:5173`（开发）/ 测试环境的外部地址 |
| `OIDC_ISSUER` | `http://192.168.48.64:8000`（须等于 Casdoor 的 `origin`，同时作为 API 基址） |
| `OIDC_CLIENT_ID` | Casdoor 应用的 clientId |
| `OIDC_CLIENT_SECRET` | Casdoor 应用的 clientSecret |
| `CASDOOR_ORGANIZATION` | `echojoy` |

依赖：新增 `github.com/coreos/go-oidc/v3`，`golang.org/x/oauth2` 由 indirect 改为直接依赖。

## 5. 分阶段实施

### Phase 1：Schema 与快照的分组交集（后端，`AUTH_KEY` 暂时保留）

- 新增 `internal/storage/migrations/0020_teams.go` 及其测试，并在 `internal/storage/migration.go` 中注册 `ID0020`。
- 在 `internal/storage/models/access_key.go` 中加三个字段；新增 `internal/storage/models/team_group_grant.go`。
- `internal/state/loader/loader.go`：`queryCompileRows` 一并加载授权；`mapAccessKeys(rows, rules, grantsByTeam)` 计算交集，交集为空时编译为 `Status=disabled`。
- `internal/gateway/models.go`：非 active 的 key 返回空模型列表。
- 验证：`make test-deps && go test ./internal/storage/... ./internal/state/... ./internal/scheduler/... ./internal/gateway/...`。新增用例覆盖：
  - 无授权的团队 key 编译为 disabled，路由返回 `access_key_disabled`，`/v1/models` 为空；
  - 交集计算；
  - key.groups 为空时取全部授权；
  - 平台 key 行为不变。

### Phase 2：用 Casdoor 登录替换 `AUTH_KEY`

本阶段除 `/api/auth/session` 外，所有路由临时设为 `accessPlatform`。

**后端**

- 配置与依赖：见 4.5 节。
- 新包 `internal/identity/`：

| 文件 | 内容 |
|---|---|
| `principal.go` | Model、资源、action 常量，`Principal`、`compilePrincipal` |
| `casdoor.go` | Basic auth 客户端：`GetUser(userID)`、`BatchEnforce(modelID, requests)`、`ListGroups`、`ListGroupUsers`；处理返回信封 |
| `session.go` | 基于 `*cluster.Client` 的会话存储：`Create`、`Get`、`Save`（`XX KEEPTTL`）、`Delete`、`PutLoginState`、`TakeLoginState` |
| `service.go` | `Service`：静态 `oauth2.Config` 与 ID token verifier、PKCE；方法 `Authenticate(ctx, cookie)`、`Teams(ctx)`、`TeamMembers(ctx, team)` |
| `http_routes.go` | `HTTPModule()`：`/auth/login`、`/auth/callback`、`/auth/logout` |
| `*_test.go` | 编译规则（按团队判定的 AdminTeams；platform 判定；批量请求与 Teams 顺序对齐；多个 model/adapter 组按列取 OR；enforce 返回空数组按 false 处理）、会话（miniredis，包括"Delete 之后 Save 不会重建会话"）、刷新（Casdoor 失败时沿用 Principal，并把 `CheckedAtMS` 更新为当前时间）、HTTP 流程（`httptest` 假 OIDC provider，静态端点指向它） |

- `internal/control/auth.go` 重写：
  - 定义 `type Identity interface { Authenticate(...); Teams(...); TeamMembers(...) }`。
  - `authenticate()` 改为读 cookie，并检查非 GET 请求的 Origin。
  - 提供 `currentPrincipal(c)`。
  - `/api/auth/session` 返回 `{user, teams, platform, admin_teams}`。
  - 删除 `accessKeyControlRoutes`、`matchAccessKey`、锁定逻辑和 `currentAccessKeyID`。
- `internal/control/server.go`：
  - 删除 `authDigest`、`authFailures`、`sharedAuthFailures`、`sharedAuthErrors`、`compareDigest` 和 `authFailureEvents`。
  - 构造函数增加 `identity Identity` 参数。
  - 删除 `admin_conflict` 校验（`:900-906`、`:991-995`、`:1253`）。
- `internal/control/http_routes.go`：`controlRoute` 增加 `access` 参数，新增 `requireAccess`。
- `internal/control/mutation_audit.go`：增加 actor 字段。
- `internal/control/system_info.go`：删除 `AuthKey` 字段。
- 删除：
  - `internal/control/auth_limiter.go`、`internal/cluster/auth_failures.go`；
  - 测试 `auth_events_test.go`、`auth_limiter_test.go`、`cluster_auth_test.go`、`cluster/auth_failures_test.go`；
  - `ErrAuthLocked`、`ErrAccessKeyAdminConflict`（`internal/platform/errors/errors.go`）；
  - i18n 键 `auth.locked`、`auth.invalid_key`、`access_key.admin_conflict`（三语 `internal/platform/i18n/locales/*.go`）。
- `internal/container/container.go`：用 `identity.NewService` 替换 `cluster.NewAuthFailures`，提供 `control.Identity`，在 `newHTTPRegistry` 中注册 auth 模块。
- 测试改造：
  - `internal/control/test_helpers_test.go` 的 `newTestServer` 注入 stub Identity（cookie 映射到 Principal），新增 `setTestPlatformSession(req)`。
  - 机械替换约 235 处 Bearer 用法。
  - 删除用 access key 登录控制台的测试。
  - `internal/container/{main_test,route_contract_test,container_test}.go` 设置 OIDC 环境变量，并通过真实 store 写入会话。
  - `TestBuildContainerExposesUnifiedRouteCatalog` 补上新路由。

**前端**（`web/src/frontends/modern/`）

- `features/auth/auth-session.ts`：去掉 key 存储和 `locked` 状态。状态为 `{phase, user, teams, platform, adminTeams}`，提供 `isPlatformAdmin`、`isTeamAdmin` 和 `ensureValidated()`。
- `api/auth.ts`：使用新的 session 返回结构，新增 `logout()`。
- `app/api-client.ts`、`web/src/shared/http/client.ts`、`web/src/shared/http/types.ts`：去掉 `authKey` 和 `Authorization` 头，更新类型。
- `features/auth/LoginView.vue`：改为单一的"使用 EchoJoy 账号登录"按钮，点击后 `location.assign('/auth/login?redirect=…')`；显示 `error=no_access` 和 `error=login_failed`。
- 删除 `use-countdown.ts` 和 `auth-errors.ts` 中的 locked 分支。
- 以下位置的 `principalType === 'admin'` 改为 `isPlatformAdmin`：`AuthGate.vue`、`router.ts`、`use-navigation.ts`、`AppLayout.vue`、`HomeView.vue`、`UsageView.vue`、`LogsView.vue`、`ModelsView.vue`、`useSystemStatus.ts`、`GatewayConnection.vue`。
- `AppLayout.vue`：增加用户菜单（显示名和团队），去掉只读徽标；登出时先 POST `/auth/logout`，再跳转到返回的 URL。
- `api/home.ts` 去掉 `currentKey`，`api/system.ts` 去掉 `authKey`，并同步修改设置页的展示。
- `web/vite.config.ts`：代理增加 `/auth`。
- 三语 i18n。

**部署与文档**

- 修改 `.env.example`、`docker-compose.yml`、`docker-compose.cluster.yml`。
- 修改 `README.md`、`README_CN.md`、`README_JP.md`、`CONTRIBUTING.md`、`.github/ISSUE_TEMPLATE/bug_report.md`，以及 `web/README.md` 的"登录与权限"一节。
- `tools/clusterbench/*`、`.github/scripts/release-docker-smoke.sh`：
  - 先 `SET <prefix>:sess:<sha256(cookie)>` 写入一个平台会话，`CheckedAtMS` 设为远未来，避免触发 Casdoor 刷新；
  - 再带 `Cookie: gl_session=…` 调用 `/api`；
  - 非 GET 请求还要带 `Origin: <PUBLIC_URL>`。smoke 脚本中的 `api_write`（POST `/api/groups`、PUT `/api/model-prices/…`、POST `/api/access-keys`）都需要。

**验证**

- `go test ./internal/identity/... ./internal/control/... ./internal/container/... ./internal/platform/config/...`
- `make check`
- 手动执行第 7 节的步骤 1–2。

### Phase 3：后端团队隔离

- 新增 `internal/control/access_scope.go`（见 4.4 节）和 `internal/control/teams.go`（3 条新路由）。
- `internal/requestlog/{types,query,usage_query,usage_query_minute,auto_decision_usage,home_statistics}.go`：改用 `ScopeAccessKeyIDs`，非 nil 时追加 `access_key_id IN ?`。
- `internal/control/{request_logs,usage,home,home_statistics,project_model_collection_http,project_model_collection}.go`：
  - `currentAccessKeyID` 分支改为作用域判断。
  - `ReadAccessKeyHomeBase` 和 `scopeProjectModelGroups` 改为接收 key ID 集合，并取这些 key 可用分组的并集。
- `internal/control/{access_keys,access_key_collection,access_key_collection_http,access_key_idempotency,access_key_update_idempotency,access_key_rotation}.go` 以及 `server.go` 中的相关 handler：按 4.4 节的写入规则实现；`AccessKeyMetadata` 增加 `team_id`、`owner_user_id`、`owner_name`；列表查询按作用域过滤。
- 把各路由的 `access` 改为 4.3 节表中的最终值。
- 验证：`go test ./internal/control/... ./internal/requestlog/...`。
  - 用表驱动测试覆盖平台、team-a 管理员、成员三种身份，在 key、日志、用量、首页、模型上的可见性和可写性。
  - 断言每条路由都声明了 access。
  - 断言成员看不到也无法 reveal 他人的 key。
  - 断言没有任何 key 的成员看到 0 条日志和用量，首页和模型列表返回 200 和空结果。
  - 断言 key 被编译为 disabled 的成员访问首页和模型列表时返回 200，不会出现 401。
  - 断言成员访问平台路由返回 403，访问他人的 key 返回 404。
  - 断言更新请求修改 `team_id` 或 `owner_user_id` 时被拒绝；团队没有授权分组时无法创建 key。

### Phase 4：前端团队与 key 管理

- `internal/webui/page_routes.json` 增加 `/teams`；`app/navigation.ts` 和 `router.ts` 增加 Teams 页（adminOnly）；Access Keys 页对所有人可见。
- 新增 `api/teams.ts` 和 `features/teams/TeamsView.vue`：列出 Casdoor 团队，从 `/api/groups` 中勾选授权分组。
- `features/access-keys/{AccessKeysView.vue,AccessKeyPanel.vue,access-key-draft.ts}` 和 `api/access-keys.ts`：
  - 团队选择器，数据来自 `adminTeams`；
  - 成员选择器和分组选择器，数据来自 `GET /api/teams/:team`；
  - 用现有的 `AccessKeyQuotaEditor.vue` 设置配额规则，并设置 RPM；
  - 非平台用户隐藏 multiplier；
  - 成员看到只读视图，可以 reveal 自己的 key。
- 三语 i18n。
- 验证：`make check`，再手动执行第 7 节的步骤 3–6。

### Phase 5（可选，后置）：Back-channel logout

- `POST /auth/backchannel-logout`：用 go-oidc 校验 `logout_token`（设 `SkipExpiryCheck`，因为 Casdoor 不签发 exp），检查 `events` 声明，然后按 `sub` 删除该用户的全部会话。
- 需要在 `internal/identity/session.go` 中维护 `Key("sess-user", sub)` 集合。
- 在 Casdoor 应用中配置 `backchannelLogoutUri`。

## 6. Casdoor 侧配置

这些是对外部系统的写操作，执行前需要单独确认。

1. **新建 Application `echojoy-ai-gateway`**（组织 `echojoy`）：
   - `tokenFormat=JWT`
   - `grantTypes=[authorization_code]`
   - `redirectUris`：`<PUBLIC_URL>/auth/callback` 和 `<PUBLIC_URL>/login`，本地开发和 xlqy 测试环境各一套。
   - 记下 clientId 和 clientSecret，用于 `.env`。
2. **权限骨架**（全局只做一次，见 4.2 节）：
   1. Adapter `gateway_rbac_tenant`：`useSameDb=true`，`table=gateway_permission_rule`；
   2. Model `gateway_rbac_tenant`：填入 4.2 节的模型文本；
   3. Permission `gateway_platform`：users=[`echojoy/demo`]，domains=[`*`]，resources=[`gateway`]，actions=[`platform:manage`]。
3. **新建团队**（每个团队 1 个 Group + 1 条 Permission）：
   1. 新建 Group `team-a`，在用户编辑页把成员加入；
   2. 新建 Permission `gateway_team-a`：model 和 adapter 同上，users=[管理员]，domains=[`team-a`]，resources=[`gateway`]，actions=[`team:manage`]。
4. **调整管理员**：只改 `gateway_team-a` 的 users。网关最多 5 分钟后生效。
5. **风险**：
   - `echojoy` 组织同时被另一产品使用，该组织下的任何 Group 都会被网关视为团队。若出现冲突，再改为按 `Group.type` 过滤。
   - 管理员改名后，需要重新保存一次对应的 Permission，让策略行重新生成。

## 7. 端到端验证（Casdoor 测试实例 + 本地或 xlqy）

1. 平台管理员 demo 登录后，所有页面可用。
2. 无团队且无权限的用户登录后，落到 `/login?error=no_access`。登出会经过 Casdoor 再回到 `/login`。在 Casdoor 中把 demo 从 Permission `gateway_platform` 的 users 中移除：
   - demo 不属于任何团队时，5 分钟内会话被删除，API 返回 401，重新登录后落到 `no_access`；
   - demo 属于某个团队时，平台路由返回 403，团队相关页面仍可用。
3. 平台管理员给 team-a 授权分组 G1。把 team-a 成员 alice 加进 Permission `gateway_team-a` 的 users，5 分钟内 alice 能看到 Access Keys 的写操作入口；alice 若同时是 team-b 成员，在 team-b 中仍只是成员。
4. team-a 管理员 alice 为成员 m1 创建 key：分组只能在 G1 内选择，并设置日配额规则和 RPM。
5. m1 登录后 reveal 自己的 key，调用 `/v1/chat/completions` 成功，且只能看到自己的日志和用量；team-a 管理员能看到全团队的日志。
6. 撤销 team-a 的 G1 授权后，该 key 的请求随即返回 `access_key_disabled`，`/v1/models` 为空。

## 8. 刻意不做的事（YAGNI）

- **不保存 refresh token 和 ID token**：权限刷新改用 client 凭证调用 get-user；登出用 `client_id` 代替 `id_token_hint`。
- **不在每次请求时调用 `/api/enforce`**。
- **不拆细粒度 action**：只有 `platform:manage` 和 `team:manage` 两个。因为 access 是内联在每条路由上声明的，以后要拆分成本很低。
- **网关不内嵌 casbin，也不解析 get-user 的 permissions**：判定只交给 Casdoor 的 enforce，网关只对用户所在的团队发起判定。
- **不用 Role**：每个团队的 Permission 直接选人。多个团队共用一个 Role 会越权（E8），按团队各建一个 Role 又会多出一倍对象。
- **不用子组**：一个团队就是一个 Group。
- **不做 OIDC discovery 和组织结构缓存**。
- **不改 `FilterSet` 的 nil/empty 语义**：交集为空走 disabled。
- **前端不建权限表**：菜单只区分"平台"和"所有人"，沿用 `adminOnly` 布尔值，只改它的数据来源。
- **不做团队总预算、团队切换器、本地 users/teams 表**：团队和成员以 Casdoor 为准。

## 9. 对抗性审查与消融记录（2026-10-13）

| # | 实验 | 结果 | 采纳 |
|---|---|---|---|
| E1 | 真实 PG 测试库中，`Pluck` 零命中 | 返回非 nil 空切片 | 作用域查询保持单条 `Pluck`，并用测试锁定零命中 |
| E2 | `scheduler.Inspect` 对比三种编译结果：空分组 map、`disabled`、只允许不存在的分组 | 空 map 放行全部分组（group 1 的原因是 `no_credentials`，而不是 `group_filtered`）；`disabled` 直接返回 `access_key_disabled` | 交集为空编译为 disabled，取代 `FilterSet` 语义改动及 7 个调用点 |
| E3 | Casdoor `fillSourceGroups` | 直接授予时为 nil；经由 group 授予时为该 group ID；沿 Role 继承链传播 | 证明 Role 可以替代 Permission；团队管理员的表达方式随后在第二次修订中改为 Group 属性 |

**第二次修订（已被第三次修订取代）**：用 `Group.properties.gateway_admins` 表达团队管理员。它能做到逐团队设置，但绕开了 Casdoor 的 Role/Permission/casbin 体系。

**第三次修订（已被第四次修订取代）：标准 RBAC + casbin 判定**

- 目标：每个团队中有一人或几人可以创建和分配 key 及额度。权限要按 Casdoor 的标准方式定义：Role → Permission → Model，由 casbin 判定。
- 消融实验（在 Casdoor 源码中调用 `GetBuiltInModel` 和 `getPolicies`，按 `newRuntimeGroupingPolicy` 的格式构造分组策略）：

| # | 建模方式 | 结果 | 结论 |
|---|---|---|---|
| E4 | Role `gateway_key_manager` + Permission + RBAC Model | alice（持有该 Role）为 true；bob 为 false；两个 Permission 的结果互不串 | 第三次修订采纳 |
| E5 | 一个 Permission，domains=[team-a, team-b]，RBAC with domains | alice 在 team-a、team-b 都为 true | domain 与主体做笛卡尔积，不能逐团队授权 |
| E6 | 把团队 Group 写进 Permission 的 groups | team-a 的普通成员 bob 也为 true | 按组授予等于全员都是管理员 |

- 结论：
  - casbin 只回答"能做什么"，团队范围由组成员关系决定。
  - 代价：同一个人不能在 A 团队当管理员、在 B 团队只当成员。第四次修订解决了这个问题。
- 判定时机：Casdoor 每次 enforce 都会新建 enforcer 并读库，所以只在登录和每 5 分钟刷新时调用一次 `batch-enforce`，结果缓存在会话里。

**第四次修订：RBAC with tenants，每个团队一条 Permission**

- 动机：第三次修订不能逐团队授权（E5）。用户提出采用 RBAC with tenants 模型。
- 消融实验（方法同 E4）：

| # | 建模方式 | 结果 | 结论 |
|---|---|---|---|
| E7 | 每个团队一条 Permission，users 直接选人，domains=[该团队]；平台权限使用 domain `*` | alice@team-a 为 true，alice@team-b 为 false，carol@team-b 为 true，bob 为 false；平台权限与团队权限互不越权，共 7 项断言全部符合 | 采纳 |
| E8 | 每个团队一条 Permission，但共用一个 Role | alice@team-b 为 true | Role 持有人与 domain 做笛卡尔积，导致越权，禁止这样配置 |
| E9 | 独立 Enforcer，手写 `p`/`g` 策略行 | 判定正确 | 可行，但用户名要手工输入、改名时不同步，不采用 |

- 代价：每个团队需要 1 个 Group（成员）加 1 条 Permission（管理员）。两者都在标准界面里通过选择器配置。

**测试实例实测（E10，2026-10-13）**

- 配置：在 `http://192.168.48.64:8000` 上临时创建 Adapter（`useSameDb=true`）、本方案的 Model、3 条 Permission（主体均为虚构用户，资源为测试专用字符串），以及一个 echojoy 组织下的测试应用。
- 结果：

| 检查 | 结果 |
|---|---|
| 用应用凭证调用 `batch-enforce?modelId=…`，7 项判定（逐团队授权、平台与团队互不越权、非成员为 false） | 7/7 PASS |
| 改用 `?permissionId=…` | 7/7 PASS，只判定该条 Permission |
| 在 Casdoor 中把 team-a 的 users 从 alice 改为 bob，再次 enforce | alice 立即为 false，bob 立即为 true |
| Permission 编辑页 | 有"包含用户"选择器和"包含域"文本框，`*` 和团队名都能填 |
| 组织管理员的浏览器会话调用 enforce | `Unauthorized operation`（不影响网关） |
| `?owner=echojoy` | 与其他 3 字段模型的 Permission 冲突而报错，弃用 |

- 测试对象已全部删除；Adapter 曾经创建的表 `zz_gw_test_rule` 可能还留在 Casdoor 数据库中。

**实施前复查（2026-10-13）**

逐项核对主流程后，修订如下：

| # | 问题 | 后果 | 修订 |
|---|---|---|---|
| R1 | 控制面单 key 读取路径在 key 不存在或 disabled 时返回 401 | 没有 key 的新成员，或 key 被禁用的成员，会在"登录 ↔ 401"之间无限循环 | 4.3 节"HTTP 状态约定"，Phase 3 新增断言 |
| R2 | 首页和模型列表的分组并集包含 disabled key | disabled key 的分组为 nil，等于"不限"，会越权展示全部分组 | 4.4 节：只取 active key |
| R3 | 会话 `Save` 只用 `KEEPTTL` | 与登出并发时会重建出一个永不过期的会话 | `SET XX KEEPTTL` |
| R4 | Casdoor 故障时每个请求都去刷新 | 整个控制面每个请求都多等一次超时 | 失败时也更新 `CheckedAtMS`；客户端超时 5 秒 |
| R5 | key 更新时可以修改 `team_id` 或 owner | 需要额外处理跨团队授权，容易留下漏洞 | 两者创建后不可修改 |
| R6 | 团队没有授权分组时也能建 key | 建出来就是 disabled，管理员不知道原因 | 创建时校验 |
| R7 | smoke 脚本的写请求没有 Origin | 改造后 smoke 会失败 | Phase 2 部署与文档一节已注明 |
| R8 | 第 7 节第 2 步还引用已删除的 Role，且把 401 写成 403 | 端到端验证会对不上 | 已改写 |

核实后确认不是问题：

- 不带 `id_token_hint` 的登出可以正确跳回 `/login`（`aud` 只对动态客户端或跨组织客户端写入）。
- 应用凭证调用 get-user 能拿到 `groups`（应用身份视为组织管理员）。
- ID token 的 `iss` 就是 Casdoor 的 origin，全局 JWKS 包含全部证书。
- 迁移编号 `0020` 是下一个可用编号。

源码核实后的其他删减：

- 会话不存 ID token：`/api/logout` 不需要 `id_token_hint`。
- 静态端点取代惰性 discovery。
- 按 `userId+owner` 调用 get-user，取代按 name 查询加 `owner` 声明校验。
- 删除组织结构缓存。
- 删除 `CASDOOR_APPLICATION`。

原方案中"忽略经由 group 授予的 `platform:manage`"这条规则可以绕过：把 Role 挂到 group 上，权限会带上 `sourceRoles`。这条规则随 Permission 一起删除。

## 10. 实施与端到端验证记录（2026-10-13）

- 本地：`make check` 通过。
- xlqy 测试环境：镜像 `echojoy-ai-gateway:test-casdoor-20261013c`（审查修订后），`PUBLIC_URL=http://192.168.48.64:3001`；原 `.env`、`docker-compose.yml` 备份为 `*.bak-20261013`。
- Casdoor（echojoy 组织）：应用 `echojoy-ai-gateway`（回调含 xlqy:3001 与 localhost:5173）、Adapter/Model `gateway_rbac_tenant`、Permission `gateway_platform`（demo）；测试对象 Group `gw-test-a`/`gw-test-b`、Permission `gateway_gw-test-a`（gw-alice）、用户 `gw-alice`、`gw-m1`、`gw-nobody`。
  - 经 API 创建 Permission 时 `model` 必须写完整 ID `echojoy/gateway_rbac_tenant`（`GetModel` 按 ID 查找，batch-enforce 也按该值匹配）；`adapter` 写名称即可。界面选择器会自动处理。
- 第 7 节各步结果：
  1. demo 登录为平台管理员，分组、团队页可用。
  2. gw-nobody 落到 `/login?error=no_access`；登出返回 Casdoor 登出地址，带 Casdoor 会话时 302 回 `/login`。浏览器若持有 `built-in/admin` 的 Casdoor 会话，会因不属于 echojoy 组织被拒（日志 `identity.login_denied` 写明原因）。
  3. 授权 gw-test-a 使用分组 1；在 Casdoor 中把 gw-alice 加回 `gateway_gw-test-a` 后，已有会话约 5 分钟后变为团队管理员；gw-alice 在 gw-test-b 只是成员（读 gw-test-b 返回 404）。
  4. gw-alice 为 gw-m1 创建 key（分组 1、日配额、RPM 10）；未授权分组 400，设置倍率 403。
  5. gw-m1 只看到自己的 key，reveal 他人 key 404、修改自己的 key 403、平台路由 403；`/v1/chat/completions` 成功，日志对 gw-m1、gw-alice、demo 各自可见。
  6. 撤销授权后该 key 立即被拒（`401 invalid_access_key`，与手动禁用的 key 相同，因为非 active key 不进入鉴权索引），路由检查器显示 `access_key_disabled`；首页与模型列表返回 200 空结果；重新授权后恢复。
- 已知：部署版 Casdoor 在没有会话时调用 `/api/logout` 返回 JSON 而不重定向（本地源码会重定向），浏览器正常登出（有会话）不受影响。

### 10.1 对抗性审查与消融实验后的修订（2026-10-13）

- 消融实验：逐项关闭 17 个核心机制跑现有测试，14 项被测试抓住；3 个盲区（nonce 校验、state 单次使用、团队管理员改自己在非管理团队的 key）已补测试。
- 安全：登录 state 原先只存于 Redis、不绑定浏览器，补 `gl_login` cookie（见 4.1 节）。
- 可见性：团队 key 因授权被收回而编译为 disabled 时，列表增加派生字段 `blocked_by_team_grants`（前端显示"团队未授权"），快照编译打 Warn `config.team_key_blocked`；首页只跳过快照为非 active 的行，快照缺失仍是 500。
- 前端退出改为先结束网关会话再跳转，失败时留在当前页显示错误（原先先跳转会卸载布局，错误无处显示且会话仍存活）。
- 删减：首页统计的后置排行过滤（requestlog 层已按作用域过滤）、更新请求的 `team_id`/`owner_user_id` 字段（strict JSON 已拒绝未知字段，现返回 INVALID_JSON）、创建请求里走私的 `ownerName`（改为 `authorizeAccessKeyCreate` 返回 owner 传给 `CreateAccessKeyIdempotent`）、`accessScope` 投影、`TeamMemberItem`、`controlRoute` 的运行时枚举检查、前端 `isTeamAdmin`/`retryAfterSeconds` 等 AUTH_KEY 时代遗留；TeamsView 改为只保存用户改过的团队，刷新不冲掉编辑。

# 运维手册：更新与重新部署

本文说明上线以后怎样发布新版本、重启、回滚和修改配置。首次部署见 [README](README.md)，设计依据见 [生产部署计划](../../docs/aws-ningxia-production-deployment-plan.md)。

以下命令都在 `deploy/aws/` 目录下执行。本机需要已配置宁夏账号的 `aws`，以及已登录 `docker.echojoy.cn` 的 `docker`：

```bash
cd deploy/aws
docker login docker.echojoy.cn -u admin    # 只需登录一次
```

## 1. 发布新版本

```bash
git commit ...                                           # 镜像标签就是提交的短 SHA
TAG=$(scripts/build-push.sh | tail -1)                   # 构建 amd64 镜像并推送
scripts/deploy.sh gateway --image-tag "$TAG" --mode maintenance --dry-run
scripts/deploy.sh gateway --image-tag "$TAG" --mode maintenance  # 默认：停机发布
# 只有确认数据库及持久配置格式兼容时，才改用 --mode rolling
```

在仓库根目录执行 `make aws-deploy`，会构建、推送并正式发布，相当于上面的第 2 步和第 4 步，不做预览。

`build-push.sh` 有两条限制：

- 除 `deploy/` 和 `docs/` 外，工作区不能有未提交的改动。这两个目录的内容不会打进镜像。
- 标签在仓库里已存在时拒绝推送。同一个提交不能发布两次，代码改动要先提交再构建。

`deploy.sh gateway` 支持两种模式，不根据 Git 文件名自动猜测兼容性：

| 模式 | 适用条件 | 失败策略 |
|---|---|---|
| `maintenance`（默认） | 数据库结构、迁移账本、持久配置格式变化，或要求停机发布 | 不自动回退旧镜像或数据库；尽力归零任务数并暂停扩缩 |
| `rolling` | 纯代码更新，且新旧版本能共用当前数据库、Redis与持久配置 | 保留ECS的100%/200%滚动策略及自动回退 |

**停机模式**：

1. 检查镜像、现有服务稳定状态和扩缩目标。保存原任务数、扩缩范围、三个暂停标志到`.backup/gateway-update.XXXXXX/`。
2. 暂停动态扩容、动态缩容和定时扩缩，把任务扩缩下限降到0，再把服务任务数降到0。等待旧任务实际进入`STOPPED`，不能只看`runningCount=0`。
3. 使用`MaintenanceWindow=true`更新CloudFormation：任务数和扩缩下限均为0，三个扩缩暂停标志均为true。关闭ECS和CloudFormation的自动回退，防止旧配置重新拉起任务。
4. 核对新任务定义的镜像，按原任务数启动新版本。应用执行迁移；等待指定任务定义达到目标任务数，并核对这些任务所在EC2实例在两个目标组均为`healthy`。扩容沿用的`rolloutState=COMPLETED`不能代替ALB检查。
5. 健康后再次更新栈，以同一镜像关闭停机窗口，恢复原扩缩范围及暂停标志。关闭窗口时省略`DesiredCount`，保留已恢复的任务数。

首次创建没有旧任务，先以0任务创建栈，再按`MinTasks`启动。已有服务的原任务数为0或部署尚未完成时拒绝继续，避免把失败现场当成正常基线。停机期间入口可返回503；不自动把流量切回旧灵影。

ALB排空等待为630秒，超过默认ECS waiter的600秒。脚本使用有界等待，`GATEWAY_DEPLOY_TIMEOUT_SECONDS`默认1800秒（每个等待阶段）。超过排空窗口的长连接不能保证不中断。

**滚动模式**：只更新一次栈，不归零任务、不暂停扩缩。新旧任务短暂并行，仍需要额外EC2容量。没有数据库兼容性保证时不得选择此模式。旧停机窗口未关闭时拒绝切到rolling。

`--dry-run`仅查询现状并预览变更集，不停止任务；仍会创建CloudFormation变更集。停机模式预览第一阶段的0任务窗口，正式执行还会在健康后关闭窗口。

线上镜像是 `25eca3c9` 或更早的版本时，第一次升级前要先修迁移账本，见[第 10 节](#10-一次性升级灵影迁移移到-ly-账本)。不修的话，新任务启动时迁移失败。

## 2. 连接管理后台和数据库

实例不开 22 端口，也没有 SSH，用 SSM 端口转发。本机需要安装 `session-manager-plugin`，IAM 用户需要 `ssm:StartSession` 权限。在仓库根目录执行：

```bash
make aws-tunnel             # 任选一台运行网关任务的实例的 3001 → localhost:13001，浏览器打开后用 AUTH_KEY 登录
make aws-tunnel TUNNEL=pg   # PostgreSQL 5432 → localhost:15432，库和用户都是 gateway
```

PostgreSQL 的密码这样取：`aws ssm get-parameter --name /echojoy-gateway/prod/PG_PASSWORD --with-decryption --query Parameter.Value --output text`。

Redis 没有隧道，因为集群会用 `MOVED` 把客户端重定向到内网 IP。要操作 Redis，用 `aws ssm start-session --target <实例 ID>` 登录节点后执行 `redis-cli`。

## 3. 不换镜像，重启全部任务

```bash
aws ecs update-service --cluster echojoy-gateway-prod --service ai-gateway --force-new-deployment
```

这条命令保留ECS当前部署策略，不执行脚本的停机流程，只适用于同一任务定义的兼容重启。要求停机重启时，使用第1节的`--mode maintenance`并指定当前镜像标签。

修改 SSM 里的密钥后必须执行这一步，因为任务只在启动时读取一次密钥。`put-secrets.sh` 只创建不存在的参数，不覆盖已有的，所以改密钥要直接用 CLI：

```bash
aws ssm put-parameter --name /echojoy-gateway/prod/AUTH_KEY --type SecureString \
  --value "$(openssl rand -hex 32)" --overwrite
```

不要修改 `ENCRYPTION_KEY`。它一旦变化，库里的渠道凭据就无法解密。

## 4. 回滚

| 情况 | 操作 |
|---|---|
| 兼容代码更新失败 | rolling模式可以自动回退；手工选择旧标签前仍必须确认数据库和持久配置兼容 |
| 不兼容升级失败 | 保持停机，使用发布前现场和完整备份制定恢复方案；不得只换旧镜像或删除新增迁移账本 |
| 文本接口切回旧灵影 | 先验证旧灵影仍提供文本接口，再考虑`scripts/alb-cutover.sh rollback`；该命令不恢复数据库 |
| WAF 改动有问题 | `scripts/waf-update.sh restore .backup/waf-acl-<时间>.json` |

### 停机发布失败后的人工恢复

脚本保留`.backup/gateway-update.XXXXXX/`中的`service-before.json`、`scaling-before.json`和目标任务定义（若已取得）。它们不是数据库备份。首次创建没有旧服务快照，恢复数量取栈的`MinTasks`。

1. 检查栈事件、ECS任务和数据库迁移状态。先确认没有旧任务进程，服务任务数为0，扩缩下限为0且全部暂停。进程强杀、网络故障或AWS权限不足时，失败保护不能代替人工检查。
2. 数据库修改前使用经恢复验证的完整生产备份；日常`pg_dump_upload`排除了请求日志数据，不能冒充完整发布快照。保留`ENCRYPTION_KEY`及`ly_usage_reports`。不得自动恢复旧快照覆盖部署后的写入。
3. CloudFormation处于`CREATE_FAILED`/`UPDATE_FAILED`时，修正原因后用CloudFormation的重试/更新流程完成目标版本，保持`DeploymentMode=maintenance`、`MaintenanceWindow=true`。不得直接使用回退栈操作，它会恢复旧扩缩配置。
4. 确认数据库和目标镜像兼容后，按原快照的任务数启动新任务，检查任务定义及两个目标组健康。然后用同一模板和镜像关闭`MaintenanceWindow`，将`MinTasks`、`MaxTasks`、`SuspendScaleIn`、`SuspendScaleOut`、`SuspendScheduledScaling`恢复为原快照值。

未恢复到稳定状态前，不得直接重跑发布脚本覆盖原任务数和扩缩状态。脚本不自动更换数据库快照、不自动修正旧迁移账本，也不运行任意数据库维护命令。

查看版本：

```bash
# 当前运行的镜像
aws ecs describe-task-definition --task-definition ai-gateway \
  --query 'taskDefinition.containerDefinitions[0].image' --output text
# 仓库里有哪些版本
curl -u admin https://docker.echojoy.cn/v2/echojoy-gateway/ai-gateway/tags/list
```

## 5. 修改配置和基础设施

每次修改都先用 `--dry-run` 预览变更集，确认后再正式执行。

| 改什么 | 怎么做 | 注意 |
|---|---|---|
| 网关环境变量、机型、任务数上下限 | 改 `gateway.yaml` 或 `config.env`，执行 `scripts/deploy.sh gateway --image-tag <当前标签>` | 默认执行第1节的停机发布；确认兼容后可以显式选择rolling |
| 安全组、告警主题 | 改 `network.yaml`，执行 `scripts/deploy.sh network` | 宁夏的 SNS 不支持邮件订阅，不要设置 `ALERT_EMAIL` |
| 数据层实例、数据卷、快照策略、告警 | 改 `data.yaml`，执行 `scripts/deploy.sh data` | 栈开了删除保护。预览里 Replacement 列为 `True` 的实例会被替换：数据卷会保留，但替换后要重新执行对应的 bootstrap 脚本 |
| Redis 或 PostgreSQL 参数、主机上的定时任务（指标上报、备份） | 改 `remote/` 下的脚本，例如 `redis.sh` 的 `redis_write_conf`、`postgres.sh` 的 `PG_SETTINGS` 或 `pg_report_metrics`，再执行 `scripts/bootstrap-redis.sh` 或 `scripts/bootstrap-pg.sh` | 配置有变化时容器会重建，配置没变时容器不动；定时任务的脚本每次都会重新安装 |

### 客户端 IP

`config.env`新增`CLIENT_IP_HEADER`（默认`X-Forwarded-For`）和`TRUSTED_PROXIES`（默认空），两种发布模式都传入任务定义。环境变量可以覆盖配置默认值，包括将头名称显式设为空以恢复连接IP模式。

空可信代理名单表示直接信任选定转发头；对于XFF取最左IP。这不是开放安全组或取消访问Key的CIDR限制。客户端能够自行提供XFF，ALB默认追加地址不等于清洗客户端的头。只有可信入口清理/重写转发头时才使用此默认值，否则必须填写真实代理IP/CIDR列表（逗号分隔），使应用从右往左识别来源。修改后重新发布任务。

## 6. 升级 postgres 或 redis 镜像

1. 在 `config.env` 里把 `POSTGRES_SOURCE_IMAGE`、`POSTGRES_IMAGE`（或 Redis 的对应变量）改成新版本的标签，例如 `redis:7.4.9-alpine`。`mirror-images.sh` 不会覆盖仓库里已有的标签，所以必须换一个新标签。
2. 执行 `scripts/mirror-images.sh`。连不上 Docker Hub 时，可以用环境变量把来源换成加速源 `docker.wwhy.games`。
3. 执行 `scripts/bootstrap-redis.sh` 或 `scripts/bootstrap-pg.sh`。

容器会重建。数据层是单节点、没有副本，重建期间网关不可用：Redis 每个节点约几秒，PostgreSQL 约十几秒。请选在低峰时段操作。

PostgreSQL 只能在同一个大版本内升级（例如 18.3 → 18.4）。跨大版本升级需要 `pg_upgrade` 或用 `pg_dump` 迁移，本手册不包含这部分。

## 7. 更换 AMI

**数据层实例**：AMI 固定为 `config.env` 的 `DATA_AMI_ID`。Redis 和 PostgreSQL 实例引用启动模板的最新版本，AMI 一变，`deploy.sh data` 就会替换全部 4 台实例，所以不跟随最新 AMI。预览里实例的 Replacement 列出现 `Conditional` 或 `True` 时，先确认是不是 `DATA_AMI_ID` 变了。

**网关实例**：启动模板的 AMI 来自 SSM 参数 `/aws/service/ecs/optimized-ami/amazon-linux-2023/recommended/image_id`，每次执行 `scripts/deploy.sh gateway` 时取最新值。之后新启动的实例使用新 AMI，现有实例不会被自动替换。

替换现有实例的流程还没有验证，计划放在 P4 演练。容量提供程序开启了 managed termination protection 和 managed draining，验证时要确认替换过程会先排空任务。

## 8. 灵影计费回调

名称为 `ly:<key_id>` 的访问密钥由灵影网关同步而来。这类请求在转发前调用灵影的 `POST /internal/aigw/balance` 查余额，结束后把用量写进 PostgreSQL 表 `ly_usage_reports`，由各副本批量投递到 `POST /internal/aigw/usage/batch`：每个副本每次最多投递 `LINGYING_USAGE_DELIVERY_BATCH_SIZE` 条（`gateway.yaml` 设为 100，即灵影允许的上限），同一时刻只有一个投递请求。余额查询的"允许"结果在每个副本内缓存 5 秒。设计见 [对接计划](../../docs/lingying-billing-callback-plan.md)。

**首次启用**

1. 和灵影约定 token，灵影侧配置为 `AI_GATEWAY_CALLBACK_TOKEN`。
2. 执行 `scripts/put-secrets.sh`，按提示输入同一个 token，写入 `LINGYING_BILLING_TOKEN`。
3. 执行 `scripts/deploy.sh gateway --image-tag <标签>`。`LINGYING_BILLING_BASE_URL` 已在 `gateway.yaml` 中设为 `https://<GatewayHost>`，`LINGYING_USAGE_DELIVERY_BATCH_SIZE` 设为 100。
4. 在管理后台确认系统货币为 CNY。不是 CNY 时，灵影的密钥一律返回 503 `billing_unavailable`。
5. 在网关实例上确认网络可达，期望返回 404，消息为 `api key not found or expired`。返回 401 或 403 说明 token 不一致；消息为「接口不存在」说明灵影还没有部署这个接口：

   ```bash
   curl -sS -X POST https://lingying-gateway.echojoy.cn/internal/aigw/balance \
     -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' -d '{"api_key":"sk-none"}'
   ```

6. 确认灵影已经部署批量上报接口，期望返回 400，消息为 `reports must hold 1 to 100 entries`。空批次不会记账。消息为「接口不存在」说明灵影还没有部署这个接口，在此之前用量会在 `ly_usage_reports` 积压，灵影部署后自动补报：

   ```bash
   curl -sS -X POST https://lingying-gateway.echojoy.cn/internal/aigw/usage/batch \
     -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' -d '{"reports":[]}'
   ```

**灵影调用控制面**：灵影用 `GET /api/models` 读取文本模型目录，用 `POST /api/access-keys`、`DELETE /api/access-keys/{id}` 同步个人 API Key。入口是 `config.env` 的 `CONTROL_HOST`（`aigw-internal.echojoy.cn`）：`scripts/private-dns.sh` 让它只在 VPC 内解析到 alb-internal，`gateway.yaml` 的 `InternalControlListenerRule` 只放行这两组路径，其余路径落到 ALB 默认动作。灵影侧配置：

| 变量 | 值 |
|---|---|
| `AI_GATEWAY_BASE_URL` | `https://aigw-internal.echojoy.cn` |
| `AI_GATEWAY_AUTH_KEY` | SSM `$SSM_PREFIX/AUTH_KEY` |
| `AI_GATEWAY_CALLBACK_TOKEN` | SSM `$SSM_PREFIX/LINGYING_BILLING_TOKEN` |

轮换 `AUTH_KEY` 时要同步改灵影的 `AI_GATEWAY_AUTH_KEY`，否则 key 同步会返回 401。

**EchoJoyAuth access token**：数据面请求可以用灵影用户的 access token 代替访问密钥，按该用户的默认密钥 `ly:default:<key_id>` 处理。配置在 `config.env` 的 `ECHOJOY_AUTH_JWKS_URL`、`ECHOJOY_AUTH_ISSUER`、`ECHOJOY_AUTH_AUDIENCE`，由 `deploy.sh gateway` 传进任务定义，修改后要重新执行 `scripts/deploy.sh gateway`。`ISSUER` 要与灵影的 `AUTH_TOKEN_ISSUER` 一致，`AUDIENCE` 要与灵影的 `AUTH_AUDIENCE` 一致，灵影没有设置时留空。JWKS URL 和 issuer 都留空则关闭这个功能。

**轮换 token**：先改灵影侧，再用第 3 节的 `aws ssm put-parameter --overwrite` 改 `LINGYING_BILLING_TOKEN` 并重启任务。两边不一致期间，查余额返回 503，用量上报失败后会持续重试，token 一致后自动补报。

**积压告警**：PG 主机每分钟上报 `LyUsageReportsPending`（已到期的条数）和 `LyUsageReportOldestDueSeconds`（最早到期一条已等待的秒数）。后者连续 5 分钟超过 300 秒时，`gateway-ly-usage-backlog` 发出通知。处置：

1. 用下面的 SQL 看 `last_error`。
2. `HTTP 404` 说明灵影还没有部署批量接口；`HTTP 401`、`HTTP 403` 说明 token 不一致；`HTTP 5xx` 或单条 `503` 看灵影日志里的 `aigw usage batch` 错误。
3. 看 DocumentDB 的 CPU 和 `TransactionsAborted`。`LINGYING_USAGE_DELIVERY_BATCH_SIZE` 已是灵影允许的上限 100，调大后网关启动失败，调小只会增加请求数和事务数。中止多说明同一用户的写冲突严重，需要灵影侧处理。

**排查积压**：用第 2 节的 PostgreSQL 隧道执行：

```sql
-- 待投递的数量和其中最早开始的请求
SELECT count(*), min((payload->>'started_at')::timestamptz) FROM ly_usage_reports WHERE next_attempt_at_ms IS NOT NULL;
-- 最近的失败原因
SELECT request_id, attempts, last_error FROM ly_usage_reports WHERE next_attempt_at_ms IS NOT NULL ORDER BY attempts DESC LIMIT 20;
-- 被灵影永久拒收（400、422）的上报
SELECT request_id, last_error, payload FROM ly_usage_reports WHERE next_attempt_at_ms IS NULL;
```

被拒收的上报修正原因后，可以重新排队：`UPDATE ly_usage_reports SET next_attempt_at_ms = 0 WHERE request_id = '<id>';`。灵影按 `request_id` 幂等，重复投递不会重复扣费。

## 9. 2026-10-15 的线上状态

| 项目 | 值 |
|---|---|
| 网关镜像 | `docker.echojoy.cn/echojoy-gateway/ai-gateway:a5ca5e99`，任务定义 `ai-gateway:2` |
| ECS | 集群 `echojoy-gateway-prod`，服务 `ai-gateway`，2 个任务，2 台 `c6in.large`（1a、1b） |
| 数据层镜像 | `echojoy-gateway/postgres:18-alpine`（18.3）、`echojoy-gateway/redis:7.4-alpine`（7.4.8） |
| 流量 | 已切换：`lingying-gateway.echojoy.cn` 的 `/gateway/v1/chat/completions`、`messages*`、`responses*`、`v1beta/models/*` 由新网关处理 |

## 10. 一次性升级：灵影迁移移到 ly 账本

`25eca3c9` 及更早的版本把灵影的用量表迁移记为核心第 23 号 `0023_ly_usage_reports`。之后的版本把核心第 23 号改为 `0023_auto_decision_attribution`，灵影迁移改名 `ly0001_usage_reports`，记在单独的账本表 `ly_schema_migrations`。新版本启动时发现 `schema_migrations` 里有 `0023_ly_usage_reports`，会报 `unknown or non-contiguous migration` 并退出。

这是一次人工维护，不由发布脚本自动删除账本。必须先取得完整生产备份并验证恢复，再进入停机窗口，确认旧任务全部`STOPPED`且扩缩暂停。新任务因这条旧账本启动失败时，停机发布保持现场，按第4节人工恢复流程继续。

只在确认账本恰为该历史布局时删除这一条旧记录，不删除表或其他迁移记录：

```sql
DELETE FROM schema_migrations WHERE id = '0023_ly_usage_reports';
```

然后启动目标版本，让应用完成核心迁移并建立独立ly账本。当前代码的目标为核心`0031_group_priority`和`ly0001_usage_reports`，不是旧手册中的0025；`ly_usage_reports`已有数据必须保留。

从核心0025升级还包含代理目录转换：旧URL配置会改为加密proxy_id引用。旧版本既不认识新增迁移账本，也不认识该配置格式，因此不得混跑，失败后不得仅回退镜像。后续版本以其实际迁移集合及配置格式为准。

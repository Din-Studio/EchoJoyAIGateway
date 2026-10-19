# AWS 宁夏生产部署

设计依据见 [生产部署计划](../../docs/aws-ningxia-production-deployment-plan.md)。本目录是它的实现：3 个 CloudFormation 模板、本地执行的部署脚本、经 SSM 下发到数据层实例的脚本，以及测试。

所有取值集中在 [`config.env`](config.env)。脚本在本机执行，需要 `aws`（已配置宁夏账号）、`docker`、`python3`、`openssl`，兼容 macOS 自带的 bash 3.2。

## 目录

| 路径 | 内容 |
|---|---|
| `network.yaml` | 3 个安全组、SNS 告警主题、可选的月度预算 |
| `data.yaml` | 3 台 Redis、1 台 PostgreSQL（固定私有 IP）、数据卷、DLM 快照、备份桶、自动恢复和服务告警 |
| `gateway.yaml` | ECS 集群、ASG 和容量提供程序、任务定义和服务、两个目标组、两个 ALB 上的新规则（含 alb-internal 上给灵影的控制面入口）、扩缩、告警 |
| `scripts/deploy.sh` | 部署上面 3 个栈，`--dry-run` 只预览变更集 |
| `scripts/put-secrets.sh` | 在 SSM 创建密钥，只创建不覆盖 |
| `scripts/mirror-images.sh` | 同步 amd64 的 postgres 和 redis 到 `docker.echojoy.cn/echojoy-gateway/`，已存在的标签不覆盖 |
| `scripts/build-push.sh` | 构建 amd64 网关镜像并推送，标签为提交短 SHA |
| `scripts/bootstrap-redis.sh`、`scripts/bootstrap-pg.sh` | 经 SSM 配置数据层实例，可重复执行 |
| `scripts/waf-update.sh` | 调整共用 Web ACL，自动备份，可 `restore` |
| `scripts/alb-cutover.sh` | 切换或回滚：旧网关的文本接口（`/gateway/v1/chat/completions`、`messages`、`responses`）转给新网关 |
| `scripts/private-dns.sh` | 给域名建指向 `alb-internal` 的 VPC 私有解析 |
| `scripts/tunnel.sh` | 用 SSM 端口转发把管理后台或 PostgreSQL 映射到本机 |
| `remote/` | 在数据层实例上执行的函数和入口脚本 |
| `tests/` | 静态检查、对真实账号的只读检查、本地 docker 集成测试 |

## 部署步骤

前三步不影响现有服务。第 7 步才会让新集群接到流量。

```bash
cd deploy/aws
docker login docker.echojoy.cn -u admin

# 1. 密钥（会提示输入 docker.echojoy.cn 的密码）和镜像
scripts/put-secrets.sh
scripts/mirror-images.sh          # 连不上 Docker Hub 时：POSTGRES_SOURCE_IMAGE=... REDIS_SOURCE_IMAGE=... scripts/mirror-images.sh
TAG=$(scripts/build-push.sh | tail -1)

# 2. 基础资源（宁夏的 SNS 不支持邮件订阅，不要设置 ALERT_EMAIL）
scripts/deploy.sh network

# 3. 数据层
scripts/deploy.sh data
scripts/bootstrap-redis.sh
scripts/bootstrap-pg.sh

# 4. 网关。新规则以优先级 30 创建，排在旧网关规则（优先级 2）之后，此时不接流量
scripts/deploy.sh gateway --image-tag "$TAG"

# 5. 确认两个新目标组都有健康目标
scripts/alb-cutover.sh status

# 6. 调整 WAF（block-llm-api-endpoints 和 KnownBadInputs 对网关域名的文本接口开例外，删除 PHP 规则组）
scripts/waf-update.sh --dry-run
scripts/waf-update.sh apply

# 7. 切换：旧网关规则从优先级 2 挪到 31，新规则生效
scripts/alb-cutover.sh cutover
```

每个会改动资源的脚本都先用 `--dry-run` 看一遍。

## 上线以后

发布新版本、重启、回滚、修改配置和升级镜像，见 [运维手册](OPERATIONS.md)。

## 测试

```bash
tests/static.sh            # shellcheck、bash -n、cfn-lint、validate-template
tests/aws-readonly.sh      # 对真实账号只读：WAF 目标容量、ALB 规则位置、3 个栈的变更集预览（预览后删除）
scripts/mirror-images.sh --no-push && scripts/build-push.sh --no-push
tests/local-integration.sh # 本机 docker：Redis Cluster、PostgreSQL、amd64 网关联调
tests/aws-instance.sh      # 临时 t3.micro：SSM 下发、数据卷挂载、重启后自动挂载、systemd timer，结束时终止实例
```

这些测试不会改动账号里的任何现有资源。`aws-instance.sh` 会新建一台临时实例，借用现有的实例配置文件 `ssm-rule-for-local-dev`，结束时无论成功失败都会终止它。

网关实例的 user data（镜像仓库凭证、CloudWatch Agent）和 ECS 部署本身没有单独的测试，在 P3 部署时验证。

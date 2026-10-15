# AWS 宁夏账号可复用资源调研报告

调研日期：2026-10-14。区域：`cn-northwest-1`。账号：`015060879373`。

本报告对照 [生产部署计划](aws-ningxia-production-deployment-plan.md)，逐项检查账号里现有的资源能否被新网关复用，并给出计划文档的修改方案和预期收益。

## 1. 结论

1. 计划已经复用了账号里最值钱的资源：默认 VPC 和 3 个默认子网、IGW、`dev-alb`、`alb-internal`、Web ACL `acl`、`*.echojoy.cn` 证书和公有托管区。少建 2 个 ALB 和 1 个 Web ACL，每月固定费用少约 ¥261。
2. 新发现能复用、且值得改计划的只有两项：
   - PostgreSQL 改用固定私有 IP 访问，不建私有托管区 `gateway.internal`。
   - 精简 Web ACL `acl`，让它的容量回到 1500 WCU 以内，所有经过 `dev-alb` 的请求单价从 ¥5.37 降到 ¥3.92/百万请求。
3. 计划文档有 2 处错误需要修正：
   - WAF 单价按 ¥3.92 计算，实际账单是第 2 档 ¥5.37。
   - 启动模板写死的 AMI ID 已经过期。
4. 现有的 PG、Redis、私有镜像仓库、实例角色和安全组都不适合生产复用，原因见第 4 节。
5. 计划之外有一项更大的节省：旧网关的 `gateway-copy`（`c5.xlarge`）长期闲置，下线后每月省约 ¥760。是否下线需要旧网关负责人决定。

## 2. 调研方法

- 用 AWS CLI 盘点了以下资源：EC2、EBS、AMI、快照、ECS、ECR、ASG、启动模板、RDS/DocumentDB、ElastiCache、ALB、目标组、安全组、IAM 角色和实例配置文件、Route 53、Cloud Map、CloudWatch Logs 和告警、SNS、SSM 参数、Secrets Manager、S3、DLM、KMS、ACM、Budgets、CloudFormation、VPC Endpoint、服务配额。
- 在两台已托管到 SSM 的实例上执行了只读命令（`docker ps`、`docker stats`、`ss -ltn`、`free`、`df`），这两台是 `gateway-db` `i-060a52bdb5bed95ed` 和 `base` `i-0d0e8696fcc435fbf`。
- 统计了 7 台运行中实例最近 14 天的 CPU 和 `NetworkOut`。
- 用 `wafv2 check-capacity` 计算了 Web ACL 改动后的 WCU，用 Cost Explorer 核对了 WAF 实际计费的档位。
- 价格来自宁夏 Pricing API，均为按需价，不含税。

## 3. 资源盘点

### 3.1 账号里还没有的资源

以下服务在宁夏还没有任何资源，必须按计划新建：ECS 集群和任务定义、ECR 仓库、ASG 和启动模板、ElastiCache、Cloud Map、SNS 主题、CloudWatch 告警、AWS Budgets、DLM 策略、VPC Endpoint、SSM Parameter Store 参数。

IAM 里已经有 ECS 服务关联角色 `AWSServiceRoleForECS`（2026-09-18 创建），没有 `ecsInstanceRole`、`ecsTaskExecutionRole` 和 DLM 默认角色。

### 3.2 运行中的实例

| 名称 | 实例 | 规格 | 14 天 CPU 平均/峰值 | 出流量 GB/天 | 用途 |
|---|---|---|---|---|---|
| `gateway-db` | `i-060a52bdb5bed95ed` | `c5.xlarge` | 8.3% / 33.9% | 1.56 | 33 个容器：旧网关 4 副本和 admin、PG 18 主从加 pgbouncer、4 个 Redis、Mongo、ClickHouse，以及 assethub、auth、支付、imagor 等业务。内存只剩 2.1 GiB 可用，磁盘已用 72% |
| `gateway` | `i-0ecdb756a8284e998` | `c5.xlarge` | 0.5% / 4.5% | 0.40 | 旧网关 4 副本（`gateway-dev-tg` 端口 8100 至 8103） |
| `gateway-copy` | `i-0bc6d638e83c809a1` | `c5.xlarge` | 0.4% / 4.7% | 0.41 | 9 月 17 日从 `gateway` 克隆，同样跑旧网关 4 副本 |
| `base` | `i-0d0e8696fcc435fbf` | `t2.medium` | 2.6% / 22.6% | 0.55 | 私有镜像仓库 `docker.echojoy.cn`（`registry:3`）、scenemint 开发环境 |
| `scenemint` | `i-0a91f38a03ec8a920` | `c5.xlarge` | 7.4% / 97.5% | 3.01 | scenemint |
| `jianda` | `i-0f5de63751bc16fee` | `t3.large` | 11.4% / 56.6% | 1.82 | jianda |
| `jianda-pre` | `i-04ad45b5398a735e6` | `t3.small` | 9.6% / 22.4% | 0.21 | jianda 预发 |

7 台实例都在 AZ 1b。

### 3.3 Web ACL `acl`

- 共 19 条规则，当前容量 1550 WCU。
- 超过 1500 WCU 后按第 2 档计费。Cost Explorer 显示 10 月 1 日至 13 日计费项为 `CNW1-RequestV2-Tier2-2000WCU`，共 759 万次请求，费用 ¥40.78，折合 ¥5.37/百万请求。第 1 档是 ¥3.92/百万请求。
- 每条规则 ¥6.53/月，Web ACL 本身 ¥32.65/月。

| 改动 | WCU | 计费档位 |
|---|---|---|
| 当前 | 1550 | 第 2 档 |
| 删除 `block-llm-api-endpoints` | 1536 | 第 2 档 |
| 删除该规则，再按计划给 PHP 和 KnownBadInputs 两组托管规则加 scope-down | 1544 | 第 2 档 |
| 删除该规则，KnownBadInputs 加 scope-down，**删除 `AWSManagedRulesPHPRuleSet`** | 1440 | **第 1 档** |

## 4. 逐项复用判断

### 4.1 计划中已经复用的资源

| 资源 | 复用方式 | 每月少付的固定费用 |
|---|---|---|
| 默认 VPC、3 个默认子网、主路由表、IGW | 网关和数据层都放进默认子网 | 免费资源，少维护一套网络 |
| `dev-alb`、`alb-internal` | 新增目标组和监听规则 | 2 × ¥0.156 × 730 ≈ ¥228 |
| Web ACL `acl` | 新集群的公网请求沿用这个 Web ACL | ¥32.65 起 |
| ACM `*.echojoy.cn`、公有托管区 `echojoy.cn` | 沿用域名 `lingying-gateway.echojoy.cn` | 免费；不需要新域名和备案 |
| `AWSServiceRoleForECS` | 已存在，创建集群时不再自动建 | 无 |
| 两个 ALB 的安全组 | 出站已经放行全部流量，不需要改 ALB 安全组 | 无 |
| EC2 配额 | 按需标准实例上限 1152 vCPU。现有实例共 22 vCPU，新网关满扩时再加约 104 vCPU，不用提额 | 无 |

### 4.2 建议新增的复用

**PostgreSQL 用固定私有 IP 访问，不建私有托管区。**

Redis 已经用固定 IP `172.31.15.11` 等地址访问，PostgreSQL 也可以直接用 `172.31.15.21`。账号里现有的 4 个私有托管区都是 `*.echojoy.cn` 的业务子域，命名上不适合放 `pg` 记录，所以计划才另建了 `gateway.internal`。

不建私有托管区后的恢复方式：

- 实例级故障：EC2 自动恢复会保留私有 IP，DSN 不用改。
- 同 AZ 重建：终止旧实例，从快照新建实例，并指定同一个 IP。
- 跨 AZ 重建：IP 会变。更新 SSM 参数 `DATABASE_DSN`，再强制重新部署 ECS 服务。比改 DNS 记录多花约 5 分钟。

### 4.3 不建议复用的资源

| 资源 | 不复用的原因 |
|---|---|
| `gateway-db` 上的 `pg-primary`、`redis-production` | 和 30 多个容器混在一台机器上，内存只剩 2.1 GiB，磁盘已用 72%。旧网关和其他业务出问题时会连带影响生产数据 |
| 私有镜像仓库 `docker.echojoy.cn` | 只跑在一台 `t2.medium` 上，网关扩容拉镜像时要依赖它。ECS 从私有仓库拉镜像还要把仓库凭证放进 Secrets Manager，多一项按月计费的密钥。ECR 存储费只要约 ¥1 到 2/月 |
| 实例配置文件 `ssm-rule-for-local-dev` | 附带 `AmazonSSMFullAccess`，可以读写账号里所有的 SSM 参数，权限过大。按计划新建最小权限角色 |
| 旧安全组 `gateway-sg`、`gateway-2-sg` | 放行了 22 端口和多个开发 IP，规则混杂。新建专用安全组 |
| DocumentDB `gateway-logs` | 旧网关的日志库。新网关的请求日志写在 PostgreSQL 里 |
| `gateway-db` 上的 EasyTier 和 beszel | 用 EasyTier 访问管理界面、用 beszel 看主机指标都可行，但要在生产安全组里放行开发机。计划里的 SSM 端口转发和 CloudWatch 不依赖任何现有主机，维持原方案 |

## 5. 计划文档的修改方案

| # | 修改 | 涉及章节 | 预期收益 |
|---|---|---|---|
| 1 | 删除私有托管区 `gateway.internal`，DSN 改为 `postgres://gateway:<pw>@172.31.15.21:5432/gateway?sslmode=require`；恢复步骤按 4.2 节改写；`network.yaml` 里不再创建托管区 | 1、8.1、8.3、11、13.1 | 少一个资源和 Route 53 依赖，每月省几元 |
| 2 | WAF 单价改为当前实际的 ¥5.37/百万请求，并注明 WCU 回到 1500 以内后是 ¥3.92 | 13.2 | 修正成本估算 |
| 3 | `waf-update.sh` 增加一步：删除 `AWSManagedRulesPHPRuleSet`，KnownBadInputs 保留 scope-down，WCU 降到 1440 | 5.3、11、12 | 见下文第 6 节。**前提是 `dev-alb` 后面没有 PHP 服务**（盘点到的业务都不是 PHP） |
| 4 | 启动模板的 AMI 不再写死 ID，模板参数类型用 `AWS::SSM::Parameter::Value<AWS::EC2::Image::Id>`，默认值是 SSM 参数路径。文档里写的 `ami-0dd34b2c52294ba90` 已经过期，当前是 `ami-012ffd2dd15181abc` | 6.1 | 避免部署时用到旧 AMI |
| 5 | 在第 11 节的模板清单里写明 `ecsInstanceRole`、`ecsTaskExecutionRole` 和 DLM 执行角色由模板创建，账号里目前没有这三个角色 | 6.1、6.2、11 | 避免 P2 部署时缺少角色 |

## 6. 预期收益

| 项目 | 每月节省 | 依据 |
|---|---|---|
| 删除 `block-llm-api-endpoints`（计划里已有，这里补上金额） | ¥6.53 | 每条规则 ¥6.53/月 |
| 删除 PHP 规则组的规则费 | ¥6.53 | 同上 |
| WAF 回到第 1 档，现有请求 | 约 ¥25 | 现有约 1750 万次/月，每百万次少 ¥1.45 |
| WAF 回到第 1 档，新网关请求 | 每 1000 万次约 ¥14.5 | 新网关的请求量还没有数据 |
| 删除私有托管区 | 几元 | 现有 5 个托管区 14 天共 ¥18.6，其中包含查询费 |
| **计划内合计** | **约 ¥40 起，随请求量增加** | |
| 计划外：下线 `gateway-copy` | 约 ¥760 | `c5.xlarge` ¥0.986 × 730 = ¥720，加 30 GB 卷 ¥16 和公网 IP ¥24。14 天 CPU 峰值 4.7%，另外两台主机还在跑旧网关 |

计划内的改动省得不多，主要价值在于修正成本估算、少一个组件。真正明显的节省在计划之外的 `gateway-copy`。

## 7. 决策记录（2026-10-14）

1. `dev-alb` 后面没有 PHP 服务。执行修改 3，删除 `AWSManagedRulesPHPRuleSet`。
2. 不下线现有的任何服务和资源，`gateway-copy` 保留。第 6 节中计划外的那一项不执行。
3. 第 5 节的 5 项修改已同步到计划文档。
4. 2026-10-14 用户决定改用 `docker.echojoy.cn` 作为镜像仓库，不建 ECR。4.3 节列出的单点风险保留，已记入计划文档第 14 节第 10 项。

其他观察，不影响本计划：

- `gateway-pg-sg` 和 `rds-ec2-1` 没有挂载任何网卡，是已删除 RDS 留下的安全组。`gateway-pg-sg` 还放行了公网 IP `117.172.29.49` 访问 5432。
- 多个目标组有不健康的目标：`int-console-tg`、`int-imagor-tg`、`scenemint-tg`、`int-scenemint-tg`、`jianda-pre-auth`、`int-jianda-pre-*`。

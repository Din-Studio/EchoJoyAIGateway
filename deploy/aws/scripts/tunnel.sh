#!/usr/bin/env bash
# 用 SSM 端口转发把线上服务映射到本机（计划文档 5.3 节）。实例不开 22 端口，没有 SSH。
# 需要本机装有 session-manager-plugin，IAM 用户有 ssm:StartSession 权限。Ctrl-C 断开。
#
# 用法：scripts/tunnel.sh [admin|pg] [本机端口]
#   admin  任选一台运行网关任务的实例的 3001（Web UI 和 /api），默认本机 13001，用 AUTH_KEY 登录
#   pg     PostgreSQL 的 5432，默认本机 15432，密码在 SSM 参数 ${SSM_PREFIX}/PG_PASSWORD
# Redis 不提供：集群会用 MOVED 把客户端重定向到内网 IP，转发到本机后无法使用。
# 要操作 Redis，用 aws ssm start-session --target <实例> 登录节点后执行 redis-cli。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws session-manager-plugin

case "${1:-admin}" in
  admin)
    # 选正在运行网关任务的实例：滚动部署后 ASG 里会留着任务已停止、尚未回收的空闲实例
    cluster=$(stack_output "$STACK_GATEWAY" ClusterName)
    service=$(stack_output "$STACK_GATEWAY" ServiceName)
    task=$(aws ecs list-tasks --cluster "$cluster" --service-name "$service" --desired-status RUNNING \
      --query 'taskArns[0]' --output text)
    [ "$task" != "None" ] || die "服务 $service 没有运行中的任务"
    container_instance=$(aws ecs describe-tasks --cluster "$cluster" --tasks "$task" \
      --query 'tasks[0].containerInstanceArn' --output text)
    instance=$(aws ecs describe-container-instances --cluster "$cluster" --container-instances "$container_instance" \
      --query 'containerInstances[0].ec2InstanceId' --output text)
    remote_port=3001 local_port=${2:-13001}
    ;;
  pg)
    instance=$(stack_output "$STACK_DATA" PostgresInstanceId)
    remote_port=5432 local_port=${2:-15432}
    ;;
  *) die "未知目标 $1，可选 admin、pg" ;;
esac

log "${instance}:${remote_port} -> localhost:${local_port}"
exec aws ssm start-session --target "$instance" \
  --document-name AWS-StartPortForwardingSession \
  --parameters "{\"portNumber\":[\"$remote_port\"],\"localPortNumber\":[\"$local_port\"]}"

#!/usr/bin/env bash
# 配置 3 个 Redis 节点并建立集群（计划文档 7 节）。可重复执行：配置没变时不重启 redis。
# 用法：scripts/bootstrap-redis.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws python3

# 节点名与 data.yaml 输出的对应关系
NODES="redis-a:RedisAInstanceId redis-b:RedisBInstanceId redis-c:RedisCInstanceId"
# shellcheck disable=SC2034 # 由 remote_exports 间接引用
REDIS_CLUSTER_ADDRS="$REDIS_IP_A:6379 $REDIS_IP_B:6379 $REDIS_IP_C:6379"

for node in $NODES; do
  NODE_NAME=${node%%:*}
  instance=$(stack_output "$STACK_DATA" "${node#*:}")
  [ "$NODE_NAME" = redis-a ] && REDIS_A_INSTANCE=$instance
  wait_ssm_online "$instance"
  log "配置 ${NODE_NAME}（${instance}）"
  {
    remote_exports AWS_REGION SSM_PREFIX REGISTRY REGISTRY_USER REDIS_IMAGE NODE_NAME
    cat "$REMOTE_DIR/host.sh" "$REMOTE_DIR/redis.sh" "$REMOTE_DIR/redis-node.sh"
  } | ssm_run "$instance" "gateway bootstrap $NODE_NAME"
done

log "在 redis-a 上建立集群"
{
  remote_exports REDIS_CLUSTER_ADDRS
  cat "$REMOTE_DIR/host.sh" "$REMOTE_DIR/redis.sh" "$REMOTE_DIR/redis-cluster.sh"
} | ssm_run "$REDIS_A_INSTANCE" "gateway redis cluster create"
log "完成"

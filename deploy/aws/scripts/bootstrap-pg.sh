#!/usr/bin/env bash
# 配置 PostgreSQL 实例：数据卷、自签证书、容器、每小时 pg_dump 和指标上报（计划文档 8 节）。
# 可重复执行：镜像、参数和密码都没变时不重启 PostgreSQL。
# 用法：scripts/bootstrap-pg.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws python3

instance=$(stack_output "$STACK_DATA" PostgresInstanceId)
# shellcheck disable=SC2034 # 由 remote_exports 间接引用
BACKUP_BUCKET=$(stack_output "$STACK_DATA" BackupBucketName)
wait_ssm_online "$instance"
log "配置 pg（${instance}）"
{
  remote_exports AWS_REGION SSM_PREFIX REGISTRY REGISTRY_USER POSTGRES_IMAGE PG_DB PG_USER PG_IP BACKUP_BUCKET
  cat "$REMOTE_DIR/host.sh" "$REMOTE_DIR/postgres.sh" "$REMOTE_DIR/pg-node.sh"
} | ssm_run "$instance" "gateway bootstrap pg"
log "完成"

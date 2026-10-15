# shellcheck shell=bash
# 在 PostgreSQL 实例上执行（bootstrap-pg.sh 把 host.sh、postgres.sh 和本文件拼接后经 SSM 下发）。
# 需要的环境变量：AWS_REGION SSM_PREFIX REGISTRY REGISTRY_USER POSTGRES_IMAGE PG_DB PG_USER PG_IP BACKUP_BUCKET
set -euo pipefail

wait_for_boot
mount_data_volume /dev/sdf /data
registry_login
docker pull -q "$POSTGRES_IMAGE"

pg_write_cert /data/pgcert "$POSTGRES_IMAGE" "$PG_IP"
pg_write_env /data/pg.env "$PG_DB" "$PG_USER" "$(ssm_secret PG_PASSWORD)"
pg_run postgres "$POSTGRES_IMAGE" /data/pgdata /data/pgcert /data/pg.env --network host
pg_wait_ready postgres "$PG_USER" "$PG_DB"

export_function_script /usr/local/bin/gateway-pg-dump pg_dump_upload
install_timer gateway-pg-dump '*-*-* *:05:00' \
  /usr/local/bin/gateway-pg-dump postgres "$PG_USER" "$PG_DB" "s3://$BACKUP_BUCKET/pg" "$AWS_REGION" /data/backup-tmp
export_function_script /usr/local/bin/gateway-report-metrics pg_report_metrics
install_timer gateway-report-metrics '*:*:00' \
  /usr/local/bin/gateway-report-metrics postgres "$PG_USER" pg "$AWS_REGION" /data
echo "postgres ready"

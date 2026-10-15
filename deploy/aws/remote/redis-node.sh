# shellcheck shell=bash
# 在一台 Redis 实例上执行（bootstrap-redis.sh 把 host.sh、redis.sh 和本文件拼接后经 SSM 下发）。
# 需要的环境变量：AWS_REGION SSM_PREFIX REGISTRY REGISTRY_USER REDIS_IMAGE NODE_NAME
set -euo pipefail

wait_for_boot
mount_data_volume /dev/sdf /data
redis_host_tuning
registry_login
docker pull -q "$REDIS_IMAGE"

redis_write_conf /data/redis "$(ssm_secret REDIS_PASSWORD)"
redis_run redis "$REDIS_IMAGE" /data/redis --network host
redis_wait_ready redis "$(awk '$1 == "requirepass" {print $2}' /data/redis/redis.conf)"

export_function_script /usr/local/bin/gateway-report-metrics redis_report_metrics
install_timer gateway-report-metrics '*:*:00' \
  /usr/local/bin/gateway-report-metrics redis /data/redis/redis.conf "$NODE_NAME" "$AWS_REGION"
echo "redis node $NODE_NAME ready"

# shellcheck shell=bash
# 在 redis-a 上执行，建立 3 主无副本集群并校验。可重复执行。
# 需要的环境变量：REDIS_CLUSTER_ADDRS（空格分隔的 ip:port）
set -euo pipefail

# shellcheck disable=SC2086 # 地址列表需要按空格拆开
redis_cluster_create redis "$(awk '$1 == "requirepass" {print $2}' /data/redis/redis.conf)" $REDIS_CLUSTER_ADDRS

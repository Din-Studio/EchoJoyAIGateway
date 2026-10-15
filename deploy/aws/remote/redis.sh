# shellcheck shell=bash
# Redis Cluster 节点函数（计划文档第 7 节）。只定义函数，供 redis-node.sh、redis-cluster.sh 和测试使用。

REDIS_MAXMEMORY=2560mb

# redis_write_conf <dir> <password>：写 redis.conf，返回前文件权限为 600。
redis_write_conf() {
  local dir=$1 password=$2
  mkdir -p "$dir"
  (
    umask 077
    cat > "$dir/redis.conf" <<EOF
port 6379
dir /data
cluster-enabled yes
cluster-config-file /data/nodes.conf
cluster-node-timeout 5000
appendonly yes
appendfsync everysec
maxmemory $REDIS_MAXMEMORY
maxmemory-policy noeviction
requirepass $password
masterauth $password
protected-mode no
EOF
  )
}

# redis_run <container> <image> <dir> <docker-network-args...>：
# 以 unless-stopped 运行 redis-server。镜像和配置都没变时不重建容器。
redis_run() {
  local name=$1 image=$2 dir=$3 digest
  shift 3
  digest=$(file_digest "$dir/redis.conf")
  if container_matches "$name" "$image" "$digest"; then
    echo "redis container $name is up to date"
    return 0
  fi
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --restart unless-stopped --label "conf-digest=$digest" \
    "$@" -v "$dir:/data" "$image" redis-server /data/redis.conf >/dev/null
  echo "redis container $name started"
}

# redis_cli <container> <password> <args...>
redis_cli() {
  local name=$1 password=$2
  shift 2
  docker exec "$name" redis-cli -a "$password" --no-auth-warning "$@"
}

redis_wait_ready() {
  local _
  for _ in $(seq 30); do
    [ "$(redis_cli "$1" "$2" ping 2>/dev/null)" = "PONG" ] && return 0
    sleep 1
  done
  echo "redis $1 did not answer PING" >&2
  return 1
}

# redis_cluster_create <container> <password> <ip:port...>：建 3 主无副本集群。
# 集群已经正常时直接校验，可重复执行。
redis_cluster_create() {
  local name=$1 password=$2 info _
  shift 2
  info=$(redis_cli "$name" "$password" cluster info)
  if echo "$info" | grep -q 'cluster_state:ok' && echo "$info" | grep -q "cluster_known_nodes:$#"; then
    echo "redis cluster already formed"
  else
    redis_cli "$name" "$password" --cluster create "$@" --cluster-replicas 0 --cluster-yes
  fi
  for _ in $(seq 30); do
    redis_cli "$name" "$password" cluster info | grep -q 'cluster_state:ok' && break
    sleep 1
  done
  redis_cli "$name" "$password" --cluster check "$1"
}

# redis_host_tuning：Redis 官方建议的内核设置（7.2 节）。
redis_host_tuning() {
  echo 'vm.overcommit_memory = 1' > /etc/sysctl.d/90-redis.conf
  sysctl --system >/dev/null
  cat > /etc/systemd/system/disable-thp.service <<'EOF'
[Unit]
Description=Disable transparent huge pages for Redis
Before=docker.service

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo never > /sys/kernel/mm/transparent_hugepage/enabled'

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now disable-thp.service >/dev/null
}

# redis_report_metrics <container> <conf-path> <node> <region>：
# 上报 RedisClusterOk 和 RedisMemoryUsedPercent，由 systemd timer 每分钟调用。
redis_report_metrics() {
  local name=$1 conf=$2 node=$3 region=$4 password ok=0 used max pct
  password=$(awk '$1 == "requirepass" {print $2}' "$conf")
  if docker exec "$name" redis-cli -a "$password" --no-auth-warning cluster info | grep -q 'cluster_state:ok'; then
    ok=1
  fi
  used=$(docker exec "$name" redis-cli -a "$password" --no-auth-warning info memory | awk -F: '$1 == "used_memory" {print $2+0}')
  max=$(docker exec "$name" redis-cli -a "$password" --no-auth-warning config get maxmemory | awk 'NR == 2 {print $1+0}')
  pct=$(awk -v u="$used" -v m="$max" 'BEGIN {printf "%.1f", (m > 0 ? u * 100 / m : 0)}')
  aws cloudwatch put-metric-data --region "$region" --namespace EchoJoyGateway --metric-data \
    "MetricName=RedisClusterOk,Value=$ok,Unit=Count,Dimensions=[{Name=Node,Value=$node}]" \
    "MetricName=RedisMemoryUsedPercent,Value=$pct,Unit=Percent,Dimensions=[{Name=Node,Value=$node}]"
}

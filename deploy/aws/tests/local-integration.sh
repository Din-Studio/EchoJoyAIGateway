#!/usr/bin/env bash
# 本地 docker 集成测试：用 remote/ 里的同一套函数拉起 3 主 Redis Cluster 和带 SSL 的 PostgreSQL，
# 再用 gateway.yaml 任务定义里的环境变量启动 amd64 网关镜像，确认网关能连上数据层。
# 同时测试 pg_dump 备份、指标上报、配置不变时不重建容器、Redis 节点重启后集群恢复。
#
# 不访问 AWS：aws 命令被替换成记录参数的桩。只在本机 docker 里创建以 gwt- 开头的容器和网络。
#
# 前置：scripts/mirror-images.sh --no-push 和 scripts/build-push.sh --no-push 已在本机生成镜像。
# 用法：tests/local-integration.sh [网关镜像标签]
set -euo pipefail

DEPLOY_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=../config.env
source "$DEPLOY_DIR/config.env"
# shellcheck source=../remote/host.sh
source "$DEPLOY_DIR/remote/host.sh"
# shellcheck source=../remote/redis.sh
source "$DEPLOY_DIR/remote/redis.sh"
# shellcheck source=../remote/postgres.sh
source "$DEPLOY_DIR/remote/postgres.sh"

TAG=${1:-$(git -C "$DEPLOY_DIR" rev-parse --short HEAD)}
GATEWAY_IMAGE=$GATEWAY_IMAGE_REPO:$TAG
NET=gwt-net
SUBNET_PREFIX=172.30.99
PLATFORM=(--platform linux/amd64)
WORK=$(mktemp -d "$HOME/.cache/gwt.XXXXXX")
FAILURES=0

pass() { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
check() { # check <描述> <命令...>
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
section() { printf '\n== %s\n' "$*"; }
contains() { printf '%s\n' "$1" | grep -q -- "$2"; }

cleanup() {
  docker rm -f gwt-redis-1 gwt-redis-2 gwt-redis-3 gwt-pg gwt-gateway >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  # 数据目录里有容器用户创建的文件，借容器删除
  docker run --rm -v "$WORK:/w" alpine:3.24 sh -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# aws 桩：s3 cp 把文件复制到 $WORK/s3，cloudwatch put-metric-data 把参数追加到 $WORK/metrics.log
mkdir -p "$WORK/bin" "$WORK/s3"
cat > "$WORK/bin/aws" <<EOF
#!/bin/bash
case "\$1 \$2" in
  "s3 cp") for a in "\$@"; do last=\$prev; prev=\$a; done; cp "\$last" "$WORK/s3/\$(basename "\$prev")" ;;
  "cloudwatch put-metric-data") echo "\$*" >> "$WORK/metrics.log" ;;
  *) echo "unexpected aws call: \$*" >&2; exit 1 ;;
esac
EOF
chmod +x "$WORK/bin/aws"

for image in "$REDIS_IMAGE" "$POSTGRES_IMAGE" "$GATEWAY_IMAGE"; do
  docker image inspect "$image" >/dev/null 2>&1 || { echo "缺少本地镜像 $image" >&2; exit 1; }
done

docker rm -f gwt-redis-1 gwt-redis-2 gwt-redis-3 gwt-pg gwt-gateway >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create --subnet "$SUBNET_PREFIX.0/24" "$NET" >/dev/null

REDIS_PASSWORD=$(openssl rand -hex 24)
PG_PASSWORD=$(openssl rand -hex 24)

section "Redis Cluster（3 主无副本）"
ADDRS=""
for n in 1 2 3; do
  redis_write_conf "$WORK/redis-$n" "$REDIS_PASSWORD"
  redis_run "gwt-redis-$n" "$REDIS_IMAGE" "$WORK/redis-$n" --network "$NET" --ip "$SUBNET_PREFIX.1$n" "${PLATFORM[@]}" >/dev/null
  ADDRS="$ADDRS $SUBNET_PREFIX.1$n:6379"
done
check "redis.conf 权限为 600" test "$(stat -f %Lp "$WORK/redis-1/redis.conf" 2>/dev/null || stat -c %a "$WORK/redis-1/redis.conf")" = 600
for n in 1 2 3; do redis_wait_ready "gwt-redis-$n" "$REDIS_PASSWORD"; done
pass "3 个节点都应答 PING"

before=$(docker inspect -f '{{.Id}}' gwt-redis-1)
redis_run gwt-redis-1 "$REDIS_IMAGE" "$WORK/redis-1" --network "$NET" --ip "$SUBNET_PREFIX.11" "${PLATFORM[@]}" >/dev/null
check "配置不变时 redis_run 不重建容器" test "$before" = "$(docker inspect -f '{{.Id}}' gwt-redis-1)"

# shellcheck disable=SC2086
redis_cluster_create gwt-redis-1 "$REDIS_PASSWORD" $ADDRS > "$WORK/cluster-create.log" 2>&1
check "集群状态 ok" sh -c "docker exec gwt-redis-1 redis-cli -a $REDIS_PASSWORD --no-auth-warning cluster info | grep -q cluster_state:ok"
check "16384 个 slot 全部分配" sh -c "docker exec gwt-redis-1 redis-cli -a $REDIS_PASSWORD --no-auth-warning cluster info | grep -q cluster_slots_assigned:16384"
# shellcheck disable=SC2086
check "再次执行 redis_cluster_create 不重建集群" contains "$(redis_cluster_create gwt-redis-1 "$REDIS_PASSWORD" $ADDRS)" 'already formed'

check "maxmemory-policy 为 noeviction" sh -c "docker exec gwt-redis-1 redis-cli -a $REDIS_PASSWORD --no-auth-warning config get maxmemory-policy | grep -q noeviction"
check "未带密码的连接被拒绝" sh -c "docker exec gwt-redis-1 redis-cli ping | grep -q NOAUTH"

section "PostgreSQL（自签证书，8.1 节参数）"
pg_write_cert "$WORK/pgcert" "$POSTGRES_IMAGE" "$SUBNET_PREFIX.21"
cert_before=$(cksum < "$WORK/pgcert/server.crt")
pg_write_cert "$WORK/pgcert" "$POSTGRES_IMAGE" "$SUBNET_PREFIX.21"
check "证书已存在时不重新生成" test "$cert_before" = "$(cksum < "$WORK/pgcert/server.crt")"
pg_write_env "$WORK/pg.env" "$PG_DB" "$PG_USER" "$PG_PASSWORD"
pg_run gwt-pg "$POSTGRES_IMAGE" "$WORK/pgdata" "$WORK/pgcert" "$WORK/pg.env" --network "$NET" --ip "$SUBNET_PREFIX.21" "${PLATFORM[@]}" >/dev/null
pg_wait_ready gwt-pg "$PG_USER" "$PG_DB"
pass "PostgreSQL 就绪"
psql() { docker exec gwt-pg psql -U "$PG_USER" -d "$PG_DB" -tAc "$1"; }
check "ssl = on" test "$(psql 'show ssl')" = on
check "max_connections = 400" test "$(psql 'show max_connections')" = 400
check "shared_buffers = 2GB" test "$(psql 'show shared_buffers')" = 2GB
before=$(docker inspect -f '{{.Id}}' gwt-pg)
pg_run gwt-pg "$POSTGRES_IMAGE" "$WORK/pgdata" "$WORK/pgcert" "$WORK/pg.env" --network "$NET" --ip "$SUBNET_PREFIX.21" "${PLATFORM[@]}" >/dev/null
check "配置不变时 pg_run 不重建容器" test "$before" = "$(docker inspect -f '{{.Id}}' gwt-pg)"

section "网关（amd64 镜像，环境变量取自 gateway.yaml）"
ENV_ARGS=()
while IFS='=' read -r k v; do
  ENV_ARGS+=(-e "$k=$v")
done < <(python3 - "$DEPLOY_DIR/gateway.yaml" <<'EOF'
import re, sys
for line in open(sys.argv[1]):
    m = re.match(r'\s*- \{Name: (\w+), Value: ("?)([^"!}]+)\2\}\s*$', line)
    if m:
        print(f"{m.group(1)}={m.group(3)}")
EOF
)
check "从模板读到 11 个固定环境变量" test "${#ENV_ARGS[@]}" -eq 22
docker run -d --name gwt-gateway "${PLATFORM[@]}" --network "$NET" --ip "$SUBNET_PREFIX.31" \
  -p 127.0.0.1:13901:3001 --ulimit nofile=1048576:1048576 "${ENV_ARGS[@]}" \
  -e "REDIS_ADDRS=$SUBNET_PREFIX.11:6379,$SUBNET_PREFIX.12:6379,$SUBNET_PREFIX.13:6379" \
  -e "REDIS_PASSWORD=$REDIS_PASSWORD" \
  -e "AUTH_KEY=$(openssl rand -hex 32)" -e "ENCRYPTION_KEY=$(openssl rand -hex 32)" \
  -e "DATABASE_DSN=postgres://$PG_USER:$PG_PASSWORD@$SUBNET_PREFIX.21:5432/$PG_DB?sslmode=require" \
  "$GATEWAY_IMAGE" >/dev/null
health=""
for _ in $(seq 90); do
  health=$(curl -s --noproxy '*' -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:13901/health || true)
  [ "$health" = 200 ] && break
  sleep 2
done
check "网关 /health 返回 200（数据库经 SSL、Redis 走集群客户端）" test "$health" = 200
[ "$health" = 200 ] || docker logs --tail 40 gwt-gateway
check "迁移已建表 request_logs" test "$(psql "select to_regclass('public.request_logs') is not null")" = t
check "数据库连接使用了 SSL" test "$(psql "select bool_or(ssl) from pg_stat_ssl join pg_stat_activity using (pid) where client_addr = '$SUBNET_PREFIX.31'")" = t
check "/v1/models 由网关处理（未授权返回 401）" test "$(curl -s --noproxy '*' -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:13901/v1/models)" = 401

section "pg_dump 备份（aws 为桩）"
psql "insert into request_logs default values" >/dev/null 2>&1 || true
PATH="$WORK/bin:$PATH" pg_dump_upload gwt-pg "$PG_USER" "$PG_DB" s3://bucket/pg cn-northwest-1 "$WORK/backup-tmp" > "$WORK/dump.log"
dump=$(find "$WORK/s3" -name "*.dump" | head -1)
check "备份已上传" test -s "$dump"
check "临时文件已删除" test ! -e "$WORK/backup-tmp/gateway.dump"
docker cp "$dump" gwt-pg:/tmp/check.dump
listing=$(docker exec gwt-pg pg_restore --list /tmp/check.dump)
check "备份包含 request_logs 表结构" contains "$listing" 'TABLE public request_logs '
if contains "$listing" 'TABLE DATA public request_logs '; then fail "备份不含 request_logs 数据"; else pass "备份不含 request_logs 数据"; fi
if contains "$listing" 'TABLE DATA public request_log_attempts '; then fail "备份不含 request_log_attempts 数据"; else pass "备份不含 request_log_attempts 数据"; fi

section "指标上报（aws 为桩，用 export_function_script 生成的独立脚本）"
export_function_script "$WORK/bin/redis-metrics" redis_report_metrics
export_function_script "$WORK/bin/pg-metrics" pg_report_metrics
PATH="$WORK/bin:$PATH" "$WORK/bin/redis-metrics" gwt-redis-1 "$WORK/redis-1/redis.conf" redis-a cn-northwest-1
PATH="$WORK/bin:$PATH" "$WORK/bin/pg-metrics" gwt-pg "$PG_USER" "$PG_DB" pg cn-northwest-1 "$WORK"
check "RedisClusterOk=1" grep -q 'MetricName=RedisClusterOk,Value=1,' "$WORK/metrics.log"
check "RedisMemoryUsedPercent 已上报" grep -qE 'MetricName=RedisMemoryUsedPercent,Value=[0-9.]+,' "$WORK/metrics.log"
check "PostgresUp=1" grep -q 'MetricName=PostgresUp,Value=1,' "$WORK/metrics.log"
check "DataDiskUsedPercent 已上报" grep -qE 'MetricName=DataDiskUsedPercent,Value=[0-9]+,' "$WORK/metrics.log"
check "LyUsageReportsPending=0" grep -q 'MetricName=LyUsageReportsPending,Value=0,' "$WORK/metrics.log"
check "LyUsageReportOldestDueSeconds=0" grep -q 'MetricName=LyUsageReportOldestDueSeconds,Value=0,' "$WORK/metrics.log"
: > "$WORK/metrics.log"
# postgres 库里没有 ly_usage_reports，相当于网关还没跑迁移
PATH="$WORK/bin:$PATH" "$WORK/bin/pg-metrics" gwt-pg "$PG_USER" postgres pg cn-northwest-1 "$WORK"
check "积压表不存在时 PostgresUp=1 照常上报" grep -q 'MetricName=PostgresUp,Value=1,' "$WORK/metrics.log"
if grep -q LyUsage "$WORK/metrics.log"; then fail "积压表不存在时不上报积压指标"; else pass "积压表不存在时不上报积压指标"; fi

section "Redis 节点故障与恢复"
docker stop gwt-redis-2 >/dev/null
for _ in $(seq 20); do
  docker exec gwt-redis-1 redis-cli -a "$REDIS_PASSWORD" --no-auth-warning cluster info | grep -q cluster_state:fail && break
  sleep 1
done
: > "$WORK/metrics.log"
PATH="$WORK/bin:$PATH" "$WORK/bin/redis-metrics" gwt-redis-1 "$WORK/redis-1/redis.conf" redis-a cn-northwest-1
check "节点停止后上报 RedisClusterOk=0" grep -q 'MetricName=RedisClusterOk,Value=0,' "$WORK/metrics.log"
docker start gwt-redis-2 >/dev/null
for _ in $(seq 30); do
  docker exec gwt-redis-1 redis-cli -a "$REDIS_PASSWORD" --no-auth-warning cluster info | grep -q cluster_state:ok && break
  sleep 1
done
check "节点重启后集群恢复 ok（AOF 和 nodes.conf 在数据目录里）" sh -c "docker exec gwt-redis-1 redis-cli -a $REDIS_PASSWORD --no-auth-warning cluster info | grep -q cluster_state:ok"
health=""
for _ in $(seq 30); do
  health=$(curl -s --noproxy '*' -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:13901/health || true)
  [ "$health" = 200 ] && break
  sleep 2
done
check "集群恢复后网关 /health 回到 200" test "$health" = 200

printf '\n%s\n' "$([ $FAILURES -eq 0 ] && echo "全部通过" || echo "$FAILURES 项失败")"
[ $FAILURES -eq 0 ]

# shellcheck shell=bash
# PostgreSQL 节点函数（计划文档第 8 节）。只定义函数，供 pg-node.sh 和测试使用。

# 8.1 节的参数，以 `postgres -c` 传入。SSL 证书在初始化时自签生成。
PG_SETTINGS="max_connections=400 shared_buffers=2GB effective_cache_size=6GB work_mem=8MB
maintenance_work_mem=512MB max_wal_size=8GB random_page_cost=1.1 checkpoint_timeout=15min
ssl=on ssl_cert_file=/certs/server.crt ssl_key_file=/certs/server.key"

# pg_write_cert <cert-dir> <image> <common-name>：证书不存在时用主机的 openssl 自签一张
# （postgres 镜像里没有 openssl），再借镜像以 root 把属主改为其中的 postgres 用户（uid 70）。
pg_write_cert() {
  local dir=$1 image=$2 cn=$3
  [ -s "$dir/server.crt" ] && [ -s "$dir/server.key" ] && return 0
  mkdir -p "$dir"
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=$cn" \
    -keyout "$dir/server.key" -out "$dir/server.crt" 2>/dev/null
  docker run --rm --user 0 --entrypoint sh -v "$dir:/certs" "$image" -c \
    'chown 70:70 /certs/server.key /certs/server.crt && chmod 600 /certs/server.key'
}

# pg_write_env <env-file> <db> <user> <password>：容器首次初始化用的环境变量，权限 600。
pg_write_env() {
  (
    umask 077
    printf 'POSTGRES_DB=%s\nPOSTGRES_USER=%s\nPOSTGRES_PASSWORD=%s\n' "$2" "$3" "$4" > "$1"
  )
}

# pg_run <container> <image> <data-dir> <cert-dir> <env-file> <docker-network-args...>：
# 以 unless-stopped 运行 PostgreSQL。镜像、参数和环境文件都没变时不重建容器。
pg_run() {
  local name=$1 image=$2 data=$3 certs=$4 envfile=$5 digest setting
  shift 5
  local args=()
  for setting in $PG_SETTINGS; do
    args+=(-c "$setting")
  done
  digest=$( { cat "$envfile"; echo "$PG_SETTINGS"; } | cksum | cut -d' ' -f1)
  if container_matches "$name" "$image" "$digest"; then
    echo "postgres container $name is up to date"
    return 0
  fi
  mkdir -p "$data"
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --restart unless-stopped --label "conf-digest=$digest" \
    --shm-size 1g --env-file "$envfile" "$@" \
    -v "$data:/var/lib/postgresql" -v "$certs:/certs:ro" \
    "$image" postgres "${args[@]}" >/dev/null
  echo "postgres container $name started"
}

# pg_wait_ready <container> <user> <db>
pg_wait_ready() {
  local _
  for _ in $(seq 60); do
    docker exec "$1" pg_isready -q -U "$2" -d "$3" && return 0
    sleep 2
  done
  echo "postgres $1 is not ready" >&2
  return 1
}

# pg_dump_upload <container> <user> <db> <s3-prefix> <region> <tmp-dir>：
# 逻辑备份，排除请求日志两张表的数据（8.2 节）。先写本地临时文件，成功后再上传，
# 不会在 S3 留下半截的备份。
pg_dump_upload() {
  local name=$1 user=$2 db=$3 prefix=$4 region=$5 tmp=$6 file key
  mkdir -p "$tmp"
  file=$tmp/gateway.dump
  docker exec "$name" pg_dump -U "$user" -d "$db" -Fc \
    --exclude-table-data=request_logs --exclude-table-data=request_log_attempts > "$file"
  key=$prefix/$(date -u +%Y/%m/%d/%H%M%S).dump
  aws s3 cp --region "$region" --only-show-errors "$file" "$key"
  rm -f "$file"
  echo "uploaded $key"
}

# pg_report_metrics <container> <user> <db> <node> <region> <data-dir>：
# 上报 PostgresUp 和 DataDiskUsedPercent，以及灵影用量上报的积压
# LyUsageReportsPending（已到期的条数）和 LyUsageReportOldestDueSeconds
# （最早到期一条已等待的秒数），由 systemd timer 每分钟调用。
# 积压查询失败时（例如网关还没建表）只上报前两个指标。
pg_report_metrics() {
  local name=$1 user=$2 db=$3 node=$4 region=$5 data=$6 up=0 disk backlog='' pending oldest
  local metrics=()
  if docker exec "$name" pg_isready -q -U "$user"; then
    up=1
  fi
  disk=$(df -P "$data" | awk 'NR == 2 {gsub("%", "", $5); print $5}')
  metrics+=("MetricName=PostgresUp,Value=$up,Unit=Count,Dimensions=[{Name=Node,Value=$node}]")
  metrics+=("MetricName=DataDiskUsedPercent,Value=$disk,Unit=Percent,Dimensions=[{Name=Node,Value=$node}]")
  if [ "$up" = 1 ]; then
    backlog=$(docker exec "$name" psql -U "$user" -d "$db" -At -F' ' -c \
      "SELECT count(*), COALESCE(floor(extract(epoch FROM now()) - min(next_attempt_at_ms) / 1000.0), 0)::bigint
       FROM ly_usage_reports
       WHERE next_attempt_at_ms IS NOT NULL AND next_attempt_at_ms <= (extract(epoch FROM now()) * 1000)::bigint" \
      2>/dev/null) || backlog=
  fi
  if [ -n "$backlog" ]; then
    read -r pending oldest <<< "$backlog"
    metrics+=("MetricName=LyUsageReportsPending,Value=$pending,Unit=Count,Dimensions=[{Name=Node,Value=$node}]")
    metrics+=("MetricName=LyUsageReportOldestDueSeconds,Value=$oldest,Unit=Seconds,Dimensions=[{Name=Node,Value=$node}]")
  fi
  aws cloudwatch put-metric-data --region "$region" --namespace EchoJoyGateway --metric-data "${metrics[@]}"
}

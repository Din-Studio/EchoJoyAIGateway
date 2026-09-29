#!/usr/bin/env bash
# 在 docker-compose.cluster.yml 样例拓扑上运行集群验收。
#   run.sh e2e    Redis Cluster 合同测试 + 确定性验收（CI 使用）
#   run.sh bench  确定性验收 + 吞吐与 p99 基准（需要 ≥ 8 核的专用主机）
# 设置 GPT_LOAD_IMAGE 时直接使用该镜像（例如已发布的版本或在宿主机预构建的镜像），
# 否则用根目录 Dockerfile 构建当前源码。
set -euo pipefail

mode="${1:-}"
case "${mode}" in
  e2e | bench) ;;
  *) echo "usage: $0 <e2e|bench>" >&2; exit 2 ;;
esac

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${root}"

random_hex() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }

export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-gl-clusterbench}"
build_gateway=false
if [ -z "${GPT_LOAD_IMAGE:-}" ]; then
  build_gateway=true
  export GPT_LOAD_IMAGE="gpt-load:${COMPOSE_PROJECT_NAME}"
fi
export CLUSTERBENCH_IMAGE="gpt-load-clusterbench:${COMPOSE_PROJECT_NAME}"
export AUTH_KEY="${AUTH_KEY:-bench-$(random_hex)}"
export ENCRYPTION_KEY="${ENCRYPTION_KEY:-$(random_hex)}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-$(random_hex)}"

compose=(docker compose -f docker-compose.cluster.yml -f tools/clusterbench/compose.yml)

cleanup() {
  local status=$?
  if [ "${status}" -ne 0 ]; then
    echo "::group::cluster logs"
    "${compose[@]}" logs --no-color --tail=200 || true
    echo "::endgroup::"
  fi
  "${compose[@]}" --profile tools down -v --rmi local --remove-orphans >/dev/null 2>&1 || true
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "${build_gateway}" = true ]; then
  docker build -t "${GPT_LOAD_IMAGE}" .
fi
docker build -f tools/clusterbench/Dockerfile -t "${CLUSTERBENCH_IMAGE}" .
"${compose[@]}" up -d --wait --wait-timeout 300

case "${mode}" in
  e2e)
    "${compose[@]}" exec -T postgres createdb -U gpt_load gpt_load_contract
    "${compose[@]}" --profile tools run --rm --build contract
    "${compose[@]}" --profile tools run --rm clusterbench verify
    ;;
  bench)
    "${compose[@]}" --profile tools run --rm clusterbench verify
    "${compose[@]}" --profile tools run --rm clusterbench bench ${BENCH_ARGS:-}
    ;;
esac

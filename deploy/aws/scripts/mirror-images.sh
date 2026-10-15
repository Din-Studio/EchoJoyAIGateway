#!/usr/bin/env bash
# 把数据层用的 amd64 postgres 和 redis 镜像同步到 docker.echojoy.cn（计划文档 9.1 节）。
# 目标标签在仓库里已存在时跳过，不覆盖。
# 用法：scripts/mirror-images.sh [--no-push]
# --no-push 只拉取并打标签，用于本地测试。推送前需要先 docker login docker.echojoy.cn。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require docker

PUSH=1
[ "${1:-}" = "--no-push" ] && PUSH=0

# 源镜像 → 目标镜像
for pair in "$POSTGRES_SOURCE_IMAGE=$POSTGRES_IMAGE" "$REDIS_SOURCE_IMAGE=$REDIS_IMAGE"; do
  src=${pair%%=*}
  dst=${pair#*=}
  log "拉取 ${src}（linux/amd64）"
  docker pull -q --platform linux/amd64 "$src" >/dev/null
  docker tag "$src" "$dst"
  arch=$(docker image inspect -f '{{.Architecture}}' "$dst")
  [ "$arch" = amd64 ] || die "$dst 的架构是 ${arch}，不是 amd64"
  if [ $PUSH -eq 1 ] && docker manifest inspect "$dst" >/dev/null 2>&1; then
    log "${dst} 已在仓库中，跳过推送"
  elif [ $PUSH -eq 1 ]; then
    docker push -q "$dst" >/dev/null
    log "已推送 $dst"
  else
    log "已打标签 ${dst}（未推送）"
  fi
done

#!/usr/bin/env bash
# 构建 amd64 网关镜像并推送到 docker.echojoy.cn（计划文档 9.1 节）。
# 标签是当前提交的短 SHA，最后一行输出这个标签，供 deploy.sh gateway --image-tag 使用。
#
# 用法：scripts/build-push.sh [--no-push]
# --no-push 只构建并载入本机 docker，用于本地测试。
# 推送前需要先 docker login docker.echojoy.cn。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require docker git

PUSH=1
[ "${1:-}" = "--no-push" ] && PUSH=0

REPO_ROOT=$(cd "$DEPLOY_DIR/../.." && pwd)
cd "$REPO_ROOT" || exit 1
# deploy/ 和 docs/ 不进镜像（见 Dockerfile 的 COPY），它们的改动不影响镜像与提交的对应关系。
if [ -n "$(git status --porcelain -- . ':(exclude)deploy' ':(exclude)docs')" ]; then
  [ $PUSH -eq 0 ] || die "工作区有未提交的改动，镜像标签无法对应到提交"
  log "工作区有未提交的改动，本地测试镜像的标签不对应任何提交"
fi
TAG=$(git rev-parse --short HEAD)
IMAGE=$GATEWAY_IMAGE_REPO:$TAG

if [ $PUSH -eq 1 ] && docker manifest inspect "$IMAGE" >/dev/null 2>&1; then
  die "$IMAGE 已存在。约定不覆盖已有标签，提交新的改动后再构建"
fi

log "构建 ${IMAGE}（linux/amd64）"
OUTPUT=--load
[ $PUSH -eq 1 ] && OUTPUT=--push
docker buildx build --platform linux/amd64 --target source-build \
  --build-arg VERSION="$(git describe --tags --always)" \
  -t "$IMAGE" "$OUTPUT" .
log "完成：$IMAGE"
echo "$TAG"

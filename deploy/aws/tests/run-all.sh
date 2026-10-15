#!/usr/bin/env bash
# 依次运行全部测试。本地集成测试需要先在本机生成镜像：
#   scripts/mirror-images.sh --no-push
#   scripts/build-push.sh --no-push
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
./static.sh
./aws-readonly.sh
./local-integration.sh "$@"

#!/usr/bin/env bash
# 在 SSM Parameter Store 创建网关的密钥（计划文档 9.2 节）。只创建不存在的参数，
# 已有的参数一律不覆盖：ENCRYPTION_KEY 一旦被覆盖，库里的渠道凭据就无法解密。
#
# 用法：scripts/put-secrets.sh [--dry-run]
# REGISTRY_PASSWORD、LINGYING_BILLING_TOKEN 不存在时会提示输入，也可以通过同名环境变量传入。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws openssl

DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

param_exists() {
  aws ssm get-parameter --name "$SSM_PREFIX/$1" --query Parameter.Name --output text >/dev/null 2>&1
}

param_value() {
  aws ssm get-parameter --name "$SSM_PREFIX/$1" --with-decryption --query Parameter.Value --output text
}

# put_new <name> <value>
put_new() {
  if [ $DRY_RUN -eq 1 ]; then
    log "将创建 $SSM_PREFIX/$1"
    return 0
  fi
  aws ssm put-parameter --name "$SSM_PREFIX/$1" --type SecureString --value "$2" >/dev/null
  log "已创建 $SSM_PREFIX/$1"
}

# ensure <name> <生成命令...>：参数不存在时用生成命令的输出创建。
ensure() {
  local name=$1
  shift
  if param_exists "$name"; then
    log "已存在 $SSM_PREFIX/${name}，跳过"
  else
    put_new "$name" "$("$@")"
  fi
}

ensure AUTH_KEY openssl rand -hex 32
ensure ENCRYPTION_KEY openssl rand -hex 32
ensure REDIS_PASSWORD openssl rand -hex 24
ensure PG_PASSWORD openssl rand -hex 24

if param_exists DATABASE_DSN; then
  log "已存在 $SSM_PREFIX/DATABASE_DSN，跳过"
elif [ $DRY_RUN -eq 1 ]; then
  log "将创建 $SSM_PREFIX/DATABASE_DSN（由 PG_PASSWORD 拼接）"
else
  put_new DATABASE_DSN "postgres://$PG_USER:$(param_value PG_PASSWORD)@$PG_IP:5432/$PG_DB?sslmode=require"
fi

if param_exists REGISTRY_PASSWORD; then
  log "已存在 $SSM_PREFIX/REGISTRY_PASSWORD，跳过"
elif [ $DRY_RUN -eq 1 ]; then
  log "将创建 $SSM_PREFIX/REGISTRY_PASSWORD（$REGISTRY 的 $REGISTRY_USER 密码，需要输入）"
else
  if [ -z "${REGISTRY_PASSWORD:-}" ]; then
    read -rsp "$REGISTRY 的 $REGISTRY_USER 密码：" REGISTRY_PASSWORD
    echo >&2
  fi
  [ -n "$REGISTRY_PASSWORD" ] || die "密码为空"
  put_new REGISTRY_PASSWORD "$REGISTRY_PASSWORD"
fi

# 灵影计费回调 token 由双方约定，必须与灵影网关的 AI_GATEWAY_CALLBACK_TOKEN 相同，所以不自动生成。
if param_exists LINGYING_BILLING_TOKEN; then
  log "已存在 $SSM_PREFIX/LINGYING_BILLING_TOKEN，跳过"
elif [ $DRY_RUN -eq 1 ]; then
  log "将创建 $SSM_PREFIX/LINGYING_BILLING_TOKEN（灵影网关的 AI_GATEWAY_CALLBACK_TOKEN，需要输入）"
else
  if [ -z "${LINGYING_BILLING_TOKEN:-}" ]; then
    read -rsp "灵影网关的 AI_GATEWAY_CALLBACK_TOKEN：" LINGYING_BILLING_TOKEN
    echo >&2
  fi
  [ -n "$LINGYING_BILLING_TOKEN" ] || die "token 为空"
  put_new LINGYING_BILLING_TOKEN "$LINGYING_BILLING_TOKEN"
fi

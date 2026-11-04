#!/usr/bin/env bash
# 部署 CloudFormation 栈（计划文档 11 节）。参数全部来自 config.env 和上游栈的输出。
#
# 用法：
#   scripts/deploy.sh network [--dry-run]
#   scripts/deploy.sh data    [--dry-run]
#   scripts/deploy.sh gateway --image-tag <git-sha> [--mode maintenance|rolling] [--dry-run]
# gateway 默认 maintenance：先停旧任务，再更新；rolling 仅用于数据格式兼容的普通代码更新。
#
# --dry-run 只生成变更集并打印变更内容，不执行；如果栈原本不存在，预览后删除留下的空栈。
# network 栈可选环境变量：ALERT_EMAIL（告警邮箱）、MONTHLY_BUDGET_CNY（月预算，元）。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
source "$(dirname "${BASH_SOURCE[0]}")/gateway-update.sh"

template_for() {
  case $1 in
    network) echo "$DEPLOY_DIR/network.yaml" ;;
    data) echo "$DEPLOY_DIR/data.yaml" ;;
    gateway) echo "$DEPLOY_DIR/gateway.yaml" ;;
    *) die "未知的栈 $1" ;;
  esac
}

stack_for() {
  case $1 in
    network) echo "$STACK_NETWORK" ;;
    data) echo "$STACK_DATA" ;;
    gateway) echo "$STACK_GATEWAY" ;;
    *) die "未知的栈 $1" ;;
  esac
}

# stack_params <network|data|gateway> [image-tag]：每行一个 Key=Value。
stack_params() {
  case $1 in
    network)
      cat <<EOF
VpcId=$VPC_ID
DevAlbSecurityGroupId=$DEV_ALB_SG
InternalAlbSecurityGroupId=$INT_ALB_SG
AlertEmail=${ALERT_EMAIL:-}
MonthlyBudgetCny=${MONTHLY_BUDGET_CNY:-0}
EOF
      ;;
    data)
      cat <<EOF
SubnetA=$SUBNET_A
SubnetB=$SUBNET_B
SubnetC=$SUBNET_C
RedisSecurityGroupId=$(stack_output "$STACK_NETWORK" RedisSecurityGroupId)
PostgresSecurityGroupId=$(stack_output "$STACK_NETWORK" PostgresSecurityGroupId)
AlertTopicArn=$(stack_output "$STACK_NETWORK" AlertTopicArn)
RedisIpA=$REDIS_IP_A
RedisIpB=$REDIS_IP_B
RedisIpC=$REDIS_IP_C
PostgresIp=$PG_IP
SsmPrefix=$SSM_PREFIX
AmiId=$DATA_AMI_ID
EOF
      ;;
    gateway)
      [ -n "${2:-}" ] || die "gateway 栈需要 --image-tag"
      cat <<EOF
VpcId=$VPC_ID
Subnets=$SUBNET_A,$SUBNET_B,$SUBNET_C
GatewaySecurityGroupId=$(stack_output "$STACK_NETWORK" GatewaySecurityGroupId)
AlertTopicArn=$(stack_output "$STACK_NETWORK" AlertTopicArn)
ImageRepo=$GATEWAY_IMAGE_REPO
ImageTag=$2
Registry=$REGISTRY
RegistryUser=$REGISTRY_USER
SsmPrefix=$SSM_PREFIX
GatewayHost=$GATEWAY_HOST
DevAlbListenerArn=$DEV_ALB_LISTENER_ARN
InternalAlbListenerArn=$INT_ALB_LISTENER_ARN
DevAlbFullName=$DEV_ALB_FULL_NAME
InternalAlbFullName=$INT_ALB_FULL_NAME
NewRulePriority=$NEW_RULE_PRIORITY
ControlHost=$CONTROL_HOST
ControlRulePriority=$CONTROL_RULE_PRIORITY
RedisAddrs=$REDIS_IP_A:6379,$REDIS_IP_B:6379,$REDIS_IP_C:6379
EchoJoyAuthJwksUrl=$ECHOJOY_AUTH_JWKS_URL
EchoJoyAuthIssuer=$ECHOJOY_AUTH_ISSUER
EchoJoyAuthAudience=$ECHOJOY_AUTH_AUDIENCE
ClientIPHeader=$CLIENT_IP_HEADER
TrustedProxies=$TRUSTED_PROXIES
DeploymentMode=${3:-maintenance}
EOF
      ;;
  esac
}

stack_status() {
  local result
  if result=$(aws cloudformation describe-stacks --stack-name "$1" --query 'Stacks[0].StackStatus' --output text 2>&1); then
    printf '%s\n' "$result"
  elif [[ "$result" == *"does not exist"* ]]; then
    echo NONE
  else
    printf '%s\n' "$result" >&2
    return 1
  fi
}

print_latest_changeset() {
  local name
  name=$(aws cloudformation list-change-sets --stack-name "$1" \
    --query 'reverse(sort_by(Summaries,&CreationTime))[0].ChangeSetName' --output text)
  aws cloudformation describe-change-set --stack-name "$1" --change-set-name "$name" \
    --query '[Status,StatusReason]' --output text >&2
  aws cloudformation describe-change-set --stack-name "$1" --change-set-name "$name" \
    --query 'Changes[].ResourceChange.[Action,LogicalResourceId,ResourceType,Replacement]' --output table
}

main() {
  local which=${1:-} tag="" dry_run=0 mode=maintenance mode_set=0 stack template before out params=() line window
  [ $# -gt 0 ] && shift
  while [ $# -gt 0 ]; do
    case $1 in
      --image-tag) [ $# -ge 2 ] || die "--image-tag 缺少值"; tag=$2; shift 2 ;;
      --mode) [ $# -ge 2 ] || die "--mode 缺少值"; mode=$2; mode_set=1; shift 2 ;;
      --dry-run) dry_run=1; shift ;;
      *) die "未知参数 $1" ;;
    esac
  done
  case $mode in maintenance|rolling) ;; *) die "--mode 只允许 maintenance 或 rolling" ;; esac
  [ "$which" = gateway ] || [ "$mode_set" -eq 0 ] || die "--mode 仅适用于 gateway"
  if [ "$which" = gateway ]; then
    printf '%s\n' "$tag" | grep -Eq '^[0-9a-f]{7,40}$' || die "gateway 需要合法的提交 SHA --image-tag"
  fi
  stack=$(stack_for "$which")
  template=$(template_for "$which")
  out=$(stack_params "$which" "$tag" "$mode")
  while IFS= read -r line; do params+=("$line"); done <<< "$out"

  if [ "$which" = gateway ] && [ $dry_run -eq 0 ]; then
    docker manifest inspect "$GATEWAY_IMAGE_REPO:$tag" >/dev/null 2>&1 \
      || die "镜像 $GATEWAY_IMAGE_REPO:$tag 不存在，先运行 scripts/build-push.sh（需要已 docker login ${REGISTRY}）"
  fi

  before=$(stack_status "$stack")
  log "部署 ${stack}（当前状态 ${before}）"
  if [ "$which" = gateway ]; then
    case $before in NONE|CREATE_COMPLETE|UPDATE_COMPLETE|UPDATE_ROLLBACK_COMPLETE) ;; *) die "gateway 栈不处于可发布状态：$before" ;; esac
    log "gateway 发布模式：$mode"
    if [ "$mode" = rolling ] && [ "$before" != NONE ]; then
      window=$(aws cloudformation describe-stacks --stack-name "$stack" \
        --query "Stacks[0].Parameters[?ParameterKey=='MaintenanceWindow'].ParameterValue | [0]" --output text)
      [ "$window" != true ] || die "上次停机窗口尚未关闭，禁止用 rolling 绕过恢复检查"
    fi
  fi
  # 模板新增 SuspendedState 后也保留手动暂停状态；预览使用与正式发布相同的扩缩参数。
  if [ "$which" = gateway ] && [ "$before" != NONE ] && { [ $dry_run -eq 1 ] || [ "$mode" = rolling ]; }; then
    gateway_capture "$before"
    params+=("MinTasks=$GW_MIN" "MaxTasks=$GW_MAX" "SuspendScaleIn=$GW_IN" \
      "SuspendScaleOut=$GW_OUT" "SuspendScheduledScaling=$GW_SCHEDULED")
  fi
  if [ $dry_run -eq 1 ]; then
    if [ "$which" = gateway ]; then
      if [ "$mode" = maintenance ]; then params+=(MaintenanceWindow=true); else params+=(MaintenanceWindow=false); fi
    fi
    aws cloudformation deploy --stack-name "$stack" --template-file "$template" \
      --parameter-overrides "${params[@]}" --capabilities CAPABILITY_IAM \
      --no-fail-on-empty-changeset --no-execute-changeset >/dev/null
    print_latest_changeset "$stack"
    if [ "$before" = NONE ]; then
      aws cloudformation delete-stack --stack-name "$stack"
      aws cloudformation wait stack-delete-complete --stack-name "$stack"
      log "已删除预览留下的空栈 $stack"
    fi
  else
    if [ "$which" = gateway ] && [ "$mode" = maintenance ]; then
      GW_EXPECTED_IMAGE=$GATEWAY_IMAGE_REPO:$tag
      gateway_deploy_maintenance "$before" "${params[@]}"
    else
      if [ "$which" = gateway ]; then params+=(MaintenanceWindow=false); fi
      aws cloudformation deploy --stack-name "$stack" --template-file "$template" \
        --parameter-overrides "${params[@]}" --capabilities CAPABILITY_IAM \
        --no-fail-on-empty-changeset
    fi
    if [ "$which" = data ]; then
      aws cloudformation update-termination-protection --stack-name "$stack" --enable-termination-protection >/dev/null
      log "已开启 $stack 的删除保护"
    fi
    aws cloudformation describe-stacks --stack-name "$stack" --query 'Stacks[0].Outputs' --output table
  fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi

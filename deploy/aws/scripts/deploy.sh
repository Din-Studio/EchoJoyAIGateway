#!/usr/bin/env bash
# 部署 CloudFormation 栈（计划文档 11 节）。参数全部来自 config.env 和上游栈的输出。
#
# 用法：
#   scripts/deploy.sh network [--dry-run]
#   scripts/deploy.sh data    [--dry-run]
#   scripts/deploy.sh gateway --image-tag <git-sha> [--dry-run]
#
# --dry-run 只生成变更集并打印变更内容，不执行；如果栈原本不存在，预览后删除留下的空栈。
# network 栈可选环境变量：ALERT_EMAIL（告警邮箱）、MONTHLY_BUDGET_CNY（月预算，元）。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

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
RedisAddrs=$REDIS_IP_A:6379,$REDIS_IP_B:6379,$REDIS_IP_C:6379
EOF
      ;;
  esac
}

stack_status() {
  aws cloudformation describe-stacks --stack-name "$1" --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE
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
  local which=${1:-} tag="" dry_run=0 stack template before out params=() line
  [ $# -gt 0 ] && shift
  while [ $# -gt 0 ]; do
    case $1 in
      --image-tag) tag=$2; shift 2 ;;
      --dry-run) dry_run=1; shift ;;
      *) die "未知参数 $1" ;;
    esac
  done
  stack=$(stack_for "$which")
  template=$(template_for "$which")
  out=$(stack_params "$which" "$tag")
  while IFS= read -r line; do params+=("$line"); done <<< "$out"

  if [ "$which" = gateway ] && [ $dry_run -eq 0 ]; then
    docker manifest inspect "$GATEWAY_IMAGE_REPO:$tag" >/dev/null 2>&1 \
      || die "镜像 $GATEWAY_IMAGE_REPO:$tag 不存在，先运行 scripts/build-push.sh（需要已 docker login ${REGISTRY}）"
  fi

  before=$(stack_status "$stack")
  log "部署 ${stack}（当前状态 ${before}）"
  if [ $dry_run -eq 1 ]; then
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
    aws cloudformation deploy --stack-name "$stack" --template-file "$template" \
      --parameter-overrides "${params[@]}" --capabilities CAPABILITY_IAM \
      --no-fail-on-empty-changeset
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

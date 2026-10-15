#!/usr/bin/env bash
# 本地部署脚本共用的函数。各脚本 source 本文件。
# 兼容 macOS 自带的 bash 3.2：不用关联数组、mapfile、${var,,}。
set -euo pipefail

DEPLOY_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck disable=SC2034 # 供 source 本文件的脚本使用
REMOTE_DIR=$DEPLOY_DIR/remote
# shellcheck source=../config.env
source "$DEPLOY_DIR/config.env"

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "错误：$*"; exit 1; }

require() {
  local c
  for c in "$@"; do command -v "$c" >/dev/null || die "缺少命令 $c"; done
}

# stack_output <stack> <output-key>
stack_output() {
  local v
  v=$(aws cloudformation describe-stacks --stack-name "$1" \
    --query "Stacks[0].Outputs[?OutputKey=='$2'].OutputValue" --output text)
  [ -n "$v" ] && [ "$v" != "None" ] || die "栈 $1 没有输出 $2"
  printf '%s\n' "$v"
}

# remote_exports VAR... ：生成在实例上重建这些变量的 export 语句。
remote_exports() {
  local name
  for name in "$@"; do
    printf 'export %s=%q\n' "$name" "${!name}"
  done
}

# wait_ssm_online <instance-id>：等实例的 SSM agent 上线。
wait_ssm_online() {
  local i status
  for i in $(seq 60); do
    status=$(aws ssm describe-instance-information \
      --filters "Key=InstanceIds,Values=$1" \
      --query 'InstanceInformationList[0].PingStatus' --output text)
    [ "$status" = "Online" ] && return 0
    log "等待 $1 的 SSM agent 上线（$i/60）"
    sleep 10
  done
  die "$1 的 SSM agent 10 分钟内没有上线"
}

# ssm_run <instance-id> <comment>：从 stdin 读入脚本，用 SSM Run Command 以 root 执行，
# 回显输出。命令失败时退出。
ssm_run() {
  local instance=$1 comment=$2 params cid status i
  params=$(python3 -c 'import json, sys; print(json.dumps({"commands": [sys.stdin.read()], "executionTimeout": ["1800"]}))')
  cid=$(aws ssm send-command --instance-ids "$instance" --document-name AWS-RunShellScript \
    --comment "$comment" --parameters "$params" --query Command.CommandId --output text)
  for i in $(seq 360); do
    sleep 5
    status=$(aws ssm get-command-invocation --command-id "$cid" --instance-id "$instance" \
      --query Status --output text 2>/dev/null || echo Pending)
    case $status in Pending|InProgress|Delayed) continue ;; esac
    break
  done
  aws ssm get-command-invocation --command-id "$cid" --instance-id "$instance" \
    --query '[StandardOutputContent,StandardErrorContent]' --output text >&2
  [ "$status" = "Success" ] || die "$instance 上的「${comment}」结束状态为 ${status}（命令 ${cid}）"
}

# listener_rule_by_host <listener-arn> <host>：输出只按这个域名匹配（没有路径条件）的规则的 "ARN 优先级"。
listener_rule_by_host() {
  aws elbv2 describe-rules --listener-arn "$1" --output json | python3 -c '
import json, sys
host = sys.argv[1]
for r in json.load(sys.stdin)["Rules"]:
    conds = r["Conditions"]
    if len(conds) != 1 or conds[0]["Field"] != "host-header":
        continue
    values = conds[0].get("HostHeaderConfig", {}).get("Values") or conds[0].get("Values", [])
    if values == [host]:
        print(r["RuleArn"], r["Priority"])
' "$2"
}

# listener_rule_by_priority <listener-arn> <priority>：输出该优先级规则的 ARN，没有则为空。
listener_rule_by_priority() {
  aws elbv2 describe-rules --listener-arn "$1" \
    --query "Rules[?Priority=='$2'].RuleArn" --output text | grep -v '^None$' || true
}

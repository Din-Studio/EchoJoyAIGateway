#!/usr/bin/env bash
# 把 /api/v1* 流量切到新集群，或切回旧网关（计划文档 5.3、12 节）。
#
# gateway.yaml 在两个 ALB 上以优先级 NEW_RULE_PRIORITY（30）创建新规则，此时旧网关的
# 整域名规则在 OLD_RULE_PRIORITY（2），先命中，新规则不接流量。
# cutover 把旧规则挪到 OLD_RULE_PARKED_PRIORITY（31），新规则随即生效；rollback 挪回 2。
# 每个 ALB 的改动是一次 set-rule-priorities 调用，原子生效。
#
# 用法：
#   scripts/alb-cutover.sh status
#   scripts/alb-cutover.sh cutover [--dry-run]
#   scripts/alb-cutover.sh rollback [--dry-run]
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws python3

# target_group_healthy_count <rule-arn>
target_group_healthy_count() {
  local tg
  tg=$(aws elbv2 describe-rules --rule-arns "$1" \
    --query 'Rules[0].Actions[?Type==`forward`].TargetGroupArn | [0]' --output text)
  aws elbv2 describe-target-health --target-group-arn "$tg" \
    --query 'length(TargetHealthDescriptions[?TargetHealth.State==`healthy`])' --output text
}

# check_listener <alb-name> <listener-arn> <mode>：打印现状；mode 为 cutover/rollback 时
# 检查前置条件，并把要执行的 "规则ARN 目标优先级" 追加到 ${PLAN}。
check_listener() {
  local alb=$1 listener=$2 mode=$3 old old_arn old_priority new_arn healthy
  old=$(listener_rule_by_host "$listener" "$GATEWAY_HOST")
  [ "$(printf '%s\n' "$old" | grep -c .)" = 1 ] || die "$alb 上找不到唯一一条只按 $GATEWAY_HOST 匹配的规则"
  old_arn=${old% *}
  old_priority=${old#* }
  new_arn=$(listener_rule_by_priority "$listener" "$NEW_RULE_PRIORITY")
  log "${alb}：旧网关整域名规则在优先级 ${old_priority}；新规则（${NEW_RULE_PRIORITY}）$([ -n "$new_arn" ] && echo 已部署 || echo 未部署)"

  case $mode in
    status) ;;
    cutover)
      [ -n "$new_arn" ] || die "$alb 上没有优先级 $NEW_RULE_PRIORITY 的新规则，先部署 gateway 栈"
      healthy=$(target_group_healthy_count "$new_arn")
      [ "$healthy" -ge 1 ] || die "$alb 新规则的目标组没有健康目标，不能切换"
      log "${alb}：新目标组 $healthy 个健康目标"
      if [ "$old_priority" = "$OLD_RULE_PARKED_PRIORITY" ]; then
        log "${alb}：已经切换过"
      else
        [ "$old_priority" = "$OLD_RULE_PRIORITY" ] || die "$alb 旧规则在意料之外的优先级 $old_priority"
        [ -z "$(listener_rule_by_priority "$listener" "$OLD_RULE_PARKED_PRIORITY")" ] \
          || die "$alb 的优先级 $OLD_RULE_PARKED_PRIORITY 已被占用"
        PLAN="$PLAN$listener $old_arn $OLD_RULE_PARKED_PRIORITY
"
      fi
      ;;
    rollback)
      if [ "$old_priority" = "$OLD_RULE_PRIORITY" ]; then
        log "${alb}：旧规则已在优先级 $OLD_RULE_PRIORITY"
      else
        [ -z "$(listener_rule_by_priority "$listener" "$OLD_RULE_PRIORITY")" ] \
          || die "$alb 的优先级 $OLD_RULE_PRIORITY 已被占用"
        PLAN="$PLAN$listener $old_arn $OLD_RULE_PRIORITY
"
      fi
      ;;
  esac
}

mode=${1:-status}
dry_run=0
[ "${2:-}" = "--dry-run" ] && dry_run=1
case $mode in status|cutover|rollback) ;; *) die "未知命令 $mode" ;; esac

PLAN=""
check_listener dev-alb "$DEV_ALB_LISTENER_ARN" "$mode"
check_listener alb-internal "$INT_ALB_LISTENER_ARN" "$mode"
[ "$mode" = status ] && exit 0
if [ -z "$PLAN" ]; then
  log "无需改动"
  exit 0
fi

# 两个 ALB 的前置检查都通过后才改，避免只切了一个。
printf '%s' "$PLAN" | while read -r listener rule priority; do
  if [ $dry_run -eq 1 ]; then
    log "dry-run：将把 $rule 改为优先级 $priority"
  else
    aws elbv2 set-rule-priorities --rule-priorities "RuleArn=$rule,Priority=$priority" >/dev/null
    log "已把 ${listener##*/app/} 上的旧规则改为优先级 $priority"
  fi
done

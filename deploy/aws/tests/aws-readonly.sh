#!/usr/bin/env bash
# shellcheck disable=SC2015 # pass 只打印不会失败，A && pass || fail 等价于 if-else
# 对真实账号的只读和预览检查，不改动任何现有资源：
# - WAF：计算目标规则和 WCU，并确认规则变换可重复执行（结果不变）
# - ALB：确认旧网关规则的位置；新规则未部署时 cutover 会拒绝执行
# - 密钥、私有 DNS：--dry-run
# - 3 个 CloudFormation 栈：只生成变更集预览，随后删除预览留下的空栈
#   （data 和 gateway 栈依赖的 network 栈输出用默认安全组和占位 ARN 代替）
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../config.env
source config.env

FAILURES=0
pass() { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

echo "== WAF"
out=$(scripts/waf-update.sh --dry-run 2>&1) && pass "waf-update --dry-run" || fail "waf-update --dry-run"
printf '%s\n' "$out" | sed 's/^/    /'
wcu=$(printf '%s\n' "$out" | sed -n 's/.*目标 .* 条规则，\([0-9]*\) WCU.*/\1/p')
[ -n "$wcu" ] && [ "$wcu" -le 1500 ] && pass "目标容量 $wcu WCU，不超过 1500" || fail "目标容量 ${wcu:-?} 超过 1500"
tmp=$(mktemp -d)
aws wafv2 get-web-acl --scope REGIONAL --name "$WAF_ACL_NAME" --id "$WAF_ACL_ID" --output json > "$tmp/acl.json"
python3 scripts/waf_rules.py "$GATEWAY_HOST" < "$tmp/acl.json" > "$tmp/once.json"
python3 -c 'import json,sys; json.dump({"WebACL": {"Rules": json.load(open(sys.argv[1]))}}, open(sys.argv[2], "w"))' "$tmp/once.json" "$tmp/acl2.json"
python3 scripts/waf_rules.py "$GATEWAY_HOST" < "$tmp/acl2.json" > "$tmp/twice.json"
cmp -s "$tmp/once.json" "$tmp/twice.json" && pass "规则变换可重复执行" || fail "规则变换不是幂等的"
rm -rf "$tmp"

echo "== ALB"
out=$(scripts/alb-cutover.sh status 2>&1) && pass "alb-cutover status" || fail "alb-cutover status"
printf '%s\n' "$out" | sed 's/^/    /'
if [ "$(printf '%s\n' "$out" | grep -c "旧网关整域名规则在优先级 ${OLD_RULE_PARKED_PRIORITY}；新规则（${NEW_RULE_PRIORITY}）已部署")" = 2 ]; then
  pass "两个 ALB 都已切换：新规则 $NEW_RULE_PRIORITY 生效，旧网关规则在 $OLD_RULE_PARKED_PRIORITY"
  out=$(scripts/alb-cutover.sh cutover --dry-run 2>&1 || true)
  printf '%s\n' "$out" | grep -q '无需改动' && pass "再次 cutover 无需改动" || fail "cutover --dry-run：$out"
elif [ "$(printf '%s\n' "$out" | grep -c "旧网关整域名规则在优先级 $OLD_RULE_PRIORITY")" = 2 ]; then
  pass "两个 ALB 都未切换，旧网关规则在 $OLD_RULE_PRIORITY"
  for l in "$DEV_ALB_LISTENER_ARN" "$INT_ALB_LISTENER_ARN"; do
    used=$(aws elbv2 describe-rules --listener-arn "$l" --query "Rules[?Priority=='$OLD_RULE_PARKED_PRIORITY'].RuleArn" --output text)
    [ -z "$used" ] && pass "优先级 $OLD_RULE_PARKED_PRIORITY 在 ${l##*/app/} 上空闲" || fail "优先级 $OLD_RULE_PARKED_PRIORITY 在 ${l##*/app/} 上已被占用"
  done
else
  fail "两个 ALB 的状态不一致"
fi

echo "== 密钥与私有 DNS（--dry-run）"
scripts/put-secrets.sh --dry-run >/dev/null 2>&1 && pass "put-secrets --dry-run" || fail "put-secrets --dry-run"
scripts/private-dns.sh "$GATEWAY_HOST" --dry-run 2>&1 | grep -q '已指向 alb-internal' \
  && pass "$GATEWAY_HOST 已在 VPC 内解析到 alb-internal" || fail "$GATEWAY_HOST 的私有解析不符合预期"
scripts/private-dns.sh "$REGISTRY" --dry-run >/dev/null 2>&1 && pass "private-dns $REGISTRY --dry-run" || fail "private-dns $REGISTRY --dry-run"

echo "== 镜像仓库"
code=$(curl -s --noproxy '*' -m 10 -o /dev/null -w '%{http_code}' "https://$REGISTRY/v2/")
[ "$code" = 401 ] && pass "$REGISTRY 在线并要求认证" || fail "$REGISTRY 返回 $code"

echo "== CloudFormation 变更集预览"
# 已部署的栈预览的是更新变更集，不会删除栈
for which in network data gateway; do
  if bash -c '
    set -euo pipefail
    source scripts/deploy.sh
    # network 栈还不存在时，用默认安全组和占位 ARN 代替它的输出
    if [ "$1" != network ] && ! aws cloudformation describe-stacks --stack-name "$STACK_NETWORK" >/dev/null 2>&1; then
      stack_output() { case $2 in AlertTopicArn) echo "arn:aws-cn:sns:cn-northwest-1:$ACCOUNT_ID:placeholder" ;; *) echo sg-0b539e822235cdacf ;; esac; }
    fi
    main "$1" --image-tag 0123abc --dry-run' _ "$which" > /tmp/gwt-changeset-$which.log 2>&1; then
    pass "$which 栈变更集（$(grep -c '|  Add|' /tmp/gwt-changeset-$which.log) 个资源）"
  else
    fail "$which 栈变更集，见 /tmp/gwt-changeset-$which.log"
  fi
done
left=$(aws cloudformation list-stacks --stack-status-filter REVIEW_IN_PROGRESS \
  --query "StackSummaries[?starts_with(StackName,'echojoy-gateway')].StackName" --output text)
[ -z "$left" ] && pass "没有留下预览栈" || fail "留下了预览栈：$left"

printf '\n%s\n' "$([ $FAILURES -eq 0 ] && echo "全部通过" || echo "$FAILURES 项失败")"
[ $FAILURES -eq 0 ]

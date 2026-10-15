#!/usr/bin/env bash
# 调整共用 Web ACL `acl`，让 /api/v1* 的 LLM 请求能通过 dev-alb（计划文档 5.3 节）。
# 影响 dev-alb 上的所有域名。改动前把整个 ACL 备份到本地，可以用 restore 恢复。
#
# 用法：
#   scripts/waf-update.sh --dry-run [--only-php]   计算目标规则和 WCU，不修改
#   scripts/waf-update.sh apply [--only-php]       应用（P3 切换前执行）
#   --only-php 只删除 PHP 规则组（P1 执行，它会误拦向镜像仓库的推送）
#   scripts/waf-update.sh restore <file>   用备份恢复规则
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws python3

MAX_WCU=1500
BACKUP_DIR=$DEPLOY_DIR/.backup
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

get_acl() {
  aws wafv2 get-web-acl --scope REGIONAL --name "$WAF_ACL_NAME" --id "$WAF_ACL_ID" --output json
}

capacity() {
  aws wafv2 check-capacity --scope REGIONAL --rules "file://$1" --query Capacity --output text
}

rule_names() {
  python3 -c 'import json, sys; print(" ".join(r["Name"] for r in json.load(open(sys.argv[1]))))' "$1"
}

# update_rules <acl.json> <rules.json>：用新规则列表更新 ACL，其余字段保持原样。
update_rules() {
  python3 - "$1" "$2" > "$WORK/update.json" <<'EOF'
import json, sys
resp = json.load(open(sys.argv[1]))
acl = resp["WebACL"]
req = {
    "Name": acl["Name"],
    "Scope": "REGIONAL",
    "Id": acl["Id"],
    "DefaultAction": acl["DefaultAction"],
    "Rules": json.load(open(sys.argv[2])),
    "VisibilityConfig": acl["VisibilityConfig"],
    "LockToken": resp["LockToken"],
}
for key in ("Description", "CustomResponseBodies", "CaptchaConfig", "ChallengeConfig",
            "TokenDomains", "AssociationConfig"):
    if acl.get(key):  # 空值不能回传，例如 Description 为 ""
        req[key] = acl[key]
json.dump(req, sys.stdout)
EOF
  aws wafv2 update-web-acl --cli-input-json "file://$WORK/update.json" >/dev/null
}

cmd=${1:---dry-run}
RULE_ARGS=()
[ "${2:-}" = "--only-php" ] && RULE_ARGS=(--only-php)
case $cmd in
  --dry-run|apply)
    get_acl > "$WORK/acl.json"
    python3 -c 'import json, sys; json.dump(json.load(open(sys.argv[1]))["WebACL"]["Rules"], open(sys.argv[2], "w"))' \
      "$WORK/acl.json" "$WORK/current.json"
    python3 "$(dirname "${BASH_SOURCE[0]}")/waf_rules.py" "$GATEWAY_HOST" ${RULE_ARGS[@]+"${RULE_ARGS[@]}"} < "$WORK/acl.json" > "$WORK/target.json"
    current_wcu=$(capacity "$WORK/current.json")
    target_wcu=$(capacity "$WORK/target.json")
    log "当前 $(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))))' "$WORK/current.json") 条规则，$current_wcu WCU"
    log "目标 $(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))))' "$WORK/target.json") 条规则，$target_wcu WCU"
    log "目标规则：$(rule_names "$WORK/target.json")"
    [ "$target_wcu" -le $MAX_WCU ] || die "目标容量 $target_wcu WCU 超过 ${MAX_WCU}，会进入更贵的计费档位"
    if python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])) == json.load(open(sys.argv[2])) else 1)' \
      "$WORK/current.json" "$WORK/target.json"; then
      log "ACL 已经是目标状态，无需修改"
      exit 0
    fi
    [ "$cmd" = apply ] || { log "dry-run：未修改"; exit 0; }
    mkdir -p "$BACKUP_DIR"
    backup=$BACKUP_DIR/waf-acl-$(date +%Y%m%d-%H%M%S).json
    cp "$WORK/acl.json" "$backup"
    log "已备份到 $backup"
    update_rules "$WORK/acl.json" "$WORK/target.json"
    log "已更新 Web ACL ${WAF_ACL_NAME}；恢复：scripts/waf-update.sh restore $backup"
    ;;
  restore)
    [ -f "${2:-}" ] || die "用法：scripts/waf-update.sh restore <备份文件>"
    get_acl > "$WORK/acl.json"
    python3 -c 'import json, sys; json.dump(json.load(open(sys.argv[1]))["WebACL"]["Rules"], open(sys.argv[2], "w"))' \
      "$2" "$WORK/restore.json"
    update_rules "$WORK/acl.json" "$WORK/restore.json"
    log "已按 $2 恢复规则"
    ;;
  *)
    die "未知命令 $cmd"
    ;;
esac

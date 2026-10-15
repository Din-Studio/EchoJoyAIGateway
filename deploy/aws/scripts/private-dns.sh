#!/usr/bin/env bash
# 给一个域名建 VPC 私有托管区，A 别名记录指向 alb-internal，
# 让 VPC 内的访问走内网 ALB、不经过公网和 WAF（计划文档 5.3、9.1 节）。
# 做法与现有的 asset、payment、auth、file 和 lingying-gateway 相同。可重复执行。
#
# 用法：scripts/private-dns.sh <域名> [--dry-run]
# 例如：scripts/private-dns.sh docker.echojoy.cn
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require aws

host=${1:-}
[ -n "$host" ] || die "用法：scripts/private-dns.sh <域名> [--dry-run]"
dry_run=0
[ "${2:-}" = "--dry-run" ] && dry_run=1

alb=$(aws elbv2 describe-load-balancers --names alb-internal \
  --query 'LoadBalancers[0].[DNSName,CanonicalHostedZoneId]' --output text)
alb_dns=${alb%%	*}
alb_zone=${alb##*	}

zone=$(aws route53 list-hosted-zones-by-vpc --vpc-id "$VPC_ID" --vpc-region "$AWS_REGION" \
  --query "HostedZoneSummaries[?Name=='$host.'].HostedZoneId | [0]" --output text)
if [ "$zone" = "None" ] || [ -z "$zone" ]; then
  if [ $dry_run -eq 1 ]; then
    log "dry-run：将新建私有托管区 $host 并关联 ${VPC_ID}，A 别名指向 $alb_dns"
    exit 0
  fi
  zone=$(aws route53 create-hosted-zone --name "$host" \
    --vpc "VPCRegion=$AWS_REGION,VPCId=$VPC_ID" \
    --caller-reference "$host-$(date +%s)" \
    --hosted-zone-config "Comment=Private service DNS to alb-internal,PrivateZone=true" \
    --query HostedZone.Id --output text)
  log "已新建私有托管区 $zone"
fi

current=$(aws route53 list-resource-record-sets --hosted-zone-id "$zone" \
  --query "ResourceRecordSets[?Name=='$host.' && Type=='A'].AliasTarget.DNSName | [0]" --output text)
if [ "$current" = "$alb_dns." ]; then
  log "$host 已指向 alb-internal"
  exit 0
fi
if [ $dry_run -eq 1 ]; then
  log "dry-run：将把 $zone 中 $host 的 A 记录改为别名 $alb_dns"
  exit 0
fi
change=$(aws route53 change-resource-record-sets --hosted-zone-id "$zone" --change-batch "{
  \"Changes\": [{\"Action\": \"UPSERT\", \"ResourceRecordSet\": {
    \"Name\": \"$host.\", \"Type\": \"A\",
    \"AliasTarget\": {\"HostedZoneId\": \"$alb_zone\", \"DNSName\": \"$alb_dns.\", \"EvaluateTargetHealth\": true}}}]}" \
  --query ChangeInfo.Id --output text)
aws route53 wait resource-record-sets-changed --id "$change"
log "$host 已在 VPC 内解析到 alb-internal"

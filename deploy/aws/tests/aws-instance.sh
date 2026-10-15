#!/usr/bin/env bash
# shellcheck disable=SC2015 # pass 只打印不会失败，A && pass || fail 等价于 if-else
# 在一台临时 EC2 上测试本地测不了的部分：SSM 下发（ssm_run、remote_exports）、
# 数据卷挂载（mount_data_volume）、systemd timer（install_timer、export_function_script）、
# 以及 data.yaml 的 user data（安装 docker）。
#
# 只新建一台 t3.micro 和随实例删除的 1 GiB 数据卷：默认子网 1a、默认安全组（无入站），
# 借用现有的实例配置文件 ssm-rule-for-local-dev。结束时（含失败）终止实例。不改动任何现有资源。
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../scripts/lib.sh"
require aws python3

TEST_PROFILE=ssm-rule-for-local-dev
TEST_SG=sg-0b539e822235cdacf
FAILURES=0
pass() { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

INSTANCE=""
cleanup() {
  if [ -n "$INSTANCE" ]; then
    aws ec2 terminate-instances --instance-ids "$INSTANCE" >/dev/null
    aws ec2 wait instance-terminated --instance-ids "$INSTANCE"
    log "已终止 ${INSTANCE}（数据卷随实例删除）"
  fi
}
trap cleanup EXIT

# data.yaml 里 DataLaunchTemplate 的 user data，原样取出
USER_DATA=$(python3 - "$DEPLOY_DIR/data.yaml" <<'EOF'
import sys
lines = open(sys.argv[1]).read().split("\n")
start = next(i for i, l in enumerate(lines) if l.strip() == "Fn::Base64: |") + 1
indent = len(lines[start]) - len(lines[start].lstrip())
out = []
for l in lines[start:]:
    if l.strip() and len(l) - len(l.lstrip()) < indent:
        break
    out.append(l[indent:])
print("\n".join(out).rstrip())
EOF
)

AMI=$(aws ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
  --query Parameter.Value --output text)
INSTANCE=$(aws ec2 run-instances --image-id "$AMI" --instance-type t3.micro \
  --subnet-id "$SUBNET_A" --security-group-ids "$TEST_SG" --associate-public-ip-address \
  --iam-instance-profile "Name=$TEST_PROFILE" \
  --metadata-options HttpTokens=required,HttpEndpoint=enabled \
  --block-device-mappings '[{"DeviceName":"/dev/sdf","Ebs":{"VolumeSize":1,"VolumeType":"gp3","DeleteOnTermination":true}}]' \
  --user-data "$USER_DATA" \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=gwt-temp-deploy-test}]' \
  --query 'Instances[0].InstanceId' --output text)
log "已启动临时实例 $INSTANCE"
wait_ssm_online "$INSTANCE"

echo "== SSM 下发与变量传递"
# shellcheck disable=SC2034 # 由 remote_exports 间接引用
TRICKY_VALUE="a b'c\"d\$e;f"
out=$({ remote_exports TRICKY_VALUE; echo 'echo "$TRICKY_VALUE"'; } | ssm_run "$INSTANCE" "gwt export test" 2>&1)
printf '%s\n' "$out" | grep -qxF "$TRICKY_VALUE" && pass "remote_exports 原样传递含引号、空格、\$ 的值" || fail "变量传递：$out"
if echo 'exit 3' | ssm_run "$INSTANCE" "gwt failure test" >/dev/null 2>&1; then
  fail "远端命令失败时 ssm_run 应当返回失败"
else
  pass "远端命令失败时 ssm_run 返回失败"
fi

echo "== user data 与数据卷"
{
  cat "$REMOTE_DIR/host.sh"
  cat <<'EOF'
set -euo pipefail
cloud-init status --wait >/dev/null || true
systemctl is-active --quiet docker && echo "CHECK docker-active"
mount_data_volume /dev/sdf /data
mountpoint -q /data && echo "CHECK mounted"
echo hello > /data/marker
umount /data
mount_data_volume /dev/sdf /data
[ "$(cat /data/marker)" = hello ] && echo "CHECK not-reformatted"
mount_data_volume /dev/sdf /data
[ "$(grep -c ' /data xfs ' /etc/fstab)" = 1 ] && echo "CHECK fstab-single"
grep ' /data xfs ' /etc/fstab | grep -q '^UUID=.*nofail' && echo "CHECK fstab-uuid-nofail"

demo_job() { date +%s > "$1"; }
export_function_script /usr/local/bin/gwt-demo demo_job
install_timer gwt-demo '*:*:00' /usr/local/bin/gwt-demo /data/timer-ran
systemctl is-enabled --quiet gwt-demo.timer && echo "CHECK timer-enabled"
/usr/local/bin/gwt-demo /data/direct-run && [ -s /data/direct-run ] && echo "CHECK exported-script-runs"
EOF
} | ssm_run "$INSTANCE" "gwt mount test" > /tmp/gwt-mount.log 2>&1 || true
for c in docker-active mounted not-reformatted fstab-single fstab-uuid-nofail timer-enabled exported-script-runs; do
  grep -q "CHECK $c" /tmp/gwt-mount.log && pass "$c" || fail "${c}（见 /tmp/gwt-mount.log）"
done

echo "== 重启后"
aws ec2 reboot-instances --instance-ids "$INSTANCE"
sleep 30
wait_ssm_online "$INSTANCE"
sleep 70
echo '
mountpoint -q /data && echo "CHECK remounted"
[ "$(cat /data/marker)" = hello ] && echo "CHECK data-kept"
[ -s /data/timer-ran ] && echo "CHECK timer-fired"
' | ssm_run "$INSTANCE" "gwt reboot test" > /tmp/gwt-reboot.log 2>&1 || true
for c in remounted data-kept timer-fired; do
  grep -q "CHECK $c" /tmp/gwt-reboot.log && pass "$c" || fail "${c}（见 /tmp/gwt-reboot.log）"
done

printf '\n%s\n' "$([ $FAILURES -eq 0 ] && echo "全部通过" || echo "$FAILURES 项失败")"
[ $FAILURES -eq 0 ]

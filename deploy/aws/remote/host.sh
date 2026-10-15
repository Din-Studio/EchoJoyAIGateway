# shellcheck shell=bash
# 数据层实例共用的主机函数。由 bootstrap 脚本拼接后经 SSM 下发，在 AL2023 上以 root 执行。
# 也会被 tests/ 在本机 source，所以只写函数，不在顶层执行命令。

# wait_for_boot：SSM agent 可能先于 user data 跑完就上线，等 cloud-init 结束（docker 在 user data 里安装）。
wait_for_boot() {
  cloud-init status --wait >/dev/null || true
  command -v docker >/dev/null || { echo "docker is not installed; check /var/log/cloud-init-output.log" >&2; return 1; }
}

# mount_data_volume <device> <dir>：挂载数据卷。卷上已有文件系统时绝不重新格式化，
# 从快照恢复的卷可以直接挂上。
mount_data_volume() {
  local dev=$1 dir=$2 uuid i
  mountpoint -q "$dir" && return 0
  for i in $(seq 60); do
    [ -e "$dev" ] && break
    echo "waiting for $dev ($i/60)"
    sleep 5
  done
  [ -e "$dev" ] || { echo "data volume $dev is not attached" >&2; return 1; }
  if ! blkid "$dev" >/dev/null 2>&1; then
    mkfs.xfs -q "$dev"
  fi
  uuid=$(blkid -s UUID -o value "$dev")
  mkdir -p "$dir"
  grep -q "UUID=$uuid" /etc/fstab || echo "UUID=$uuid $dir xfs defaults,nofail 0 2" >> /etc/fstab
  mount "$dir"
}

# ssm_secret <name>：读取 $SSM_PREFIX/<name>。
ssm_secret() {
  aws ssm get-parameter --region "$AWS_REGION" --with-decryption \
    --name "$SSM_PREFIX/$1" --query Parameter.Value --output text
}

registry_login() {
  ssm_secret REGISTRY_PASSWORD | docker login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin >/dev/null
}

# file_digest <file>...：用于判断配置是否变化，Linux 和 macOS 都有 cksum。
file_digest() {
  cat "$@" | cksum | cut -d' ' -f1
}

# container_matches <name> <image> <digest>：容器在运行、镜像和配置摘要都相同时返回 0。
container_matches() {
  local state
  state=$(docker inspect -f '{{.Config.Image}} {{index .Config.Labels "conf-digest"}} {{.State.Running}}' "$1" 2>/dev/null) || return 1
  [ "$state" = "$2 $3 true" ]
}

# export_function_script <path> <entry-function> [helper-function...]：
# 把函数写成独立的可执行脚本，供 systemd timer 调用，入口函数接收脚本参数。
export_function_script() {
  local path=$1 entry=$2
  shift 2
  {
    echo '#!/bin/bash'
    echo 'set -euo pipefail'
    declare -f "$entry" "$@"
    echo "$entry \"\$@\""
  } > "$path"
  chmod 755 "$path"
}

# install_timer <name> <OnCalendar> <command...>：安装并启用 systemd timer。
install_timer() {
  local name=$1 calendar=$2
  shift 2
  cat > "/etc/systemd/system/$name.service" <<EOF
[Unit]
Description=$name

[Service]
Type=oneshot
ExecStart=$*
EOF
  cat > "/etc/systemd/system/$name.timer" <<EOF
[Unit]
Description=$name

[Timer]
OnCalendar=$calendar
Persistent=true

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now "$name.timer" >/dev/null
}

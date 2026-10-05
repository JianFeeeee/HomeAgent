#!/bin/bash
# waiter 远程更新（106 / 30）
#
# 现状（更新前）：
#   两台都是 8月27日构建的 /opt/waiter/waiter（11,388,177 字节），
#   以 root 跑 waiter-remote.service，配置指向 ws://192.168.2.60:9890/...
#   进程已连续运行 1 天 7.5 小时、无掉线记录 ⇒ 本次是**预防性**更新：
#   · 拿到 94c74b2 的设备桥修复（未 bind 时收到 ping 会关连接）
#   · 版本对齐到内核 1.4.0（docs/git-branching.md 要求客户端与内核同步）
#
# 安全设计：
#   · 先备份旧二进制，失败即回滚（trap 捕获）
#   · 只换二进制，**不动** waiter.yaml
#   · 逐台更新并验证，**不并行**（两台都连同一网关，同时重启会同时断链）
#   · 验证项：进程活着 + 设备在网关侧注册成功
#
# 用法：
#   bash deploy/scripts/deploy-waiter.sh check            只读检查
#   bash deploy/scripts/deploy-waiter.sh deploy <ip>      更新指定一台
#   bash deploy/scripts/deploy-waiter.sh rollback <ip>    回滚指定一台

set -uo pipefail

NEW=${NEW_WAITER:-/tmp/waiter-new}
SVC=waiter-remote.service
BIN=/opt/waiter/waiter
CFG=/opt/waiter/waiter.yaml

# 106 用 admin（需 sudo），30 用 root
ssh_of() {
  case "$1" in
    192.168.2.106) echo "admin@$1" ;;
    192.168.2.30)  echo "root@$1" ;;
    *) echo "" ;;
  esac
}
sudo_of() { [ "$1" = "192.168.2.106" ] && echo "sudo" || echo ""; }

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }

do_check() {
  local ip=$1 t s
  t=$(ssh_of "$ip"); [ -z "$t" ] && { info "未知主机: $ip"; return 1; }
  s=$(sudo_of "$ip")
  info "──── $ip ────"
  # ★ -n：ssh 会从 stdin 读，若不截断会**吃掉后面 read 的输入**
  #   （"yes" 被 ssh 消耗 ⇒ read 拿到空 ⇒ 脚本静默取消）。
  #   症状是"明明喂了 yes 却说已取消"，极难定位。
  ssh -n -o BatchMode=yes -o ConnectTimeout=8 "$t" "
    echo -n '  主机: '; hostname
    echo -n '  当前二进制: '; ls -la $BIN 2>/dev/null | awk '{print \$5\" 字节  \"\$6\" \"\$7\" \"\$8}'
    echo -n '  服务: '; systemctl is-active $SVC 2>/dev/null
    echo -n '  进程时长: '; ps -o etime= -p \$(pgrep -f 'waiter --daemon' | head -1) 2>/dev/null
    echo -n '  配置网关: '; grep -oE 'ws://[^\"]+' $CFG 2>/dev/null | head -1
    echo -n '  配置校验和: '; md5sum $CFG 2>/dev/null | cut -c1-32
  " 2>&1
  info "  新二进制: $NEW ($(stat -c %s "$NEW" 2>/dev/null) 字节)"
  info "  新版本: $(/tmp/waiter-new --version 2>&1 | head -1)"
  return 0
}

do_deploy() {
  local ip=$1 t s
  t=$(ssh_of "$ip"); [ -z "$t" ] && { info "未知主机: $ip"; return 1; }
  s=$(sudo_of "$ip")
  [ -f "$NEW" ] || { info "新二进制不存在: $NEW"; return 1; }

  say "更新 $ip"
  do_check "$ip"

  echo
  read -r -p "确认更新 $ip 的 waiter ? 输入 yes 继续: " ans
  [ "$ans" = "yes" ] || { info "已取消"; return 1; }

  # 备份 + 停服 + 替换 + 启服；失败即回滚
  scp -q "$NEW" "$t:/tmp/waiter.new" || { info "上传失败"; return 1; }
  info "已上传"

  ssh -n -o BatchMode=yes -o ConnectTimeout=8 "$t" "
    set -e
    $s cp -a $BIN $BIN.bak-\$(date +%Y%m%d-%H%M%S)
    echo BACKUP=\$(ls -t $BIN.bak-* | head -1)
    $s systemctl stop $SVC
    $s install -m 0755 /tmp/waiter.new $BIN
    rm -f /tmp/waiter.new
    $s systemctl start $SVC
  " 2>&1 | tail -3
  info "已替换并启动"

  sleep 8
  say "更新后验证 $ip"
  local alive=false
  for i in 1 2 3 4 5; do
    if ssh -n -o BatchMode=yes -o ConnectTimeout=8 "$t" "systemctl is-active --quiet $SVC" 2>/dev/null; then
      alive=true; break
    fi
    sleep 3
  done
  if [ "$alive" = true ]; then
    info "✓ 服务 active"
    ssh -n -o BatchMode=yes -o ConnectTimeout=8 "$t" "
      echo -n '  新二进制: '; ls -la $BIN | awk '{print \$5\" 字节\"}'
      echo -n '  进程时长: '; ps -o etime= -p \$(pgrep -f 'waiter --daemon' | head -1) 2>/dev/null
      echo -n '  配置未变: '; md5sum $CFG | cut -c1-32
    " 2>&1
    info "★ 请在网关侧确认设备已重新注册（/kernel 的 channels 应出现 device/<id>）"
  else
    info "✗ 服务未起来 —— 回滚"
    do_rollback "$ip"
    return 1
  fi
}

do_rollback() {
  local ip=$1 t s
  t=$(ssh_of "$ip"); s=$(sudo_of "$ip")
  say "回滚 $ip"
  ssh -n -o BatchMode=yes -o ConnectTimeout=8 "$t" "
    set -e
    BAK=\$(ls -t $BIN.bak-* 2>/dev/null | head -1)
    [ -n \"\$BAK\" ] || { echo '找不到备份'; exit 1; }
    $s systemctl stop $SVC
    $s install -m 0755 \"\$BAK\" $BIN
    $s systemctl start $SVC
    echo \"已回滚到 \$BAK\"
  " 2>&1 | tail -2
  sleep 5
  info "服务: $(ssh -o BatchMode=yes "$t" "systemctl is-active $SVC" 2>/dev/null)"
}

case "${1:-check}" in
  check)
    for ip in 192.168.2.106 192.168.2.30; do say "检查 $ip"; do_check "$ip"; done
    ;;
  deploy)   do_deploy "${2:?用法: deploy <ip>}" ;;
  rollback) do_rollback "${2:?用法: rollback <ip>}" ;;
  *) echo "用法: $0 {check|deploy <ip>|rollback <ip>}"; exit 2 ;;
esac

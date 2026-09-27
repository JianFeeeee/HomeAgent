#!/bin/bash
# 生产部署方案（**待确认，不自动执行**）
#
# 用法：
#   bash deploy-plan.sh check     # 只做部署前检查，不改任何东西
#   bash deploy-plan.sh backup    # 备份当前二进制与适配器
#   bash deploy-plan.sh deploy    # 替换二进制并重启（需显式确认）
#   bash deploy-plan.sh rollback  # 回滚到备份
#
# ★ 本脚本刻意**不包含**任何"自动回滚"逻辑：回滚要不要做、什么时候做，
#   是人的判断。脚本只负责把状态保全好，让回滚成为一条可执行的命令。

set -uo pipefail

BIN=/usr/local/bin/homed
DATA=/home/newqqagent
BAK=/var/tmp/homed-backup-$(date +%Y%m%d-%H%M%S)
NEW=${NEW_BIN:-/tmp/homed-ort}
ADAPTERS=$DATA/adapters
SVC=homeagent.service

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }

# ---------------------------------------------------------------- check
do_check() {
  say "部署前检查"
  info "服务状态: $(systemctl is-active $SVC)"
  info "当前二进制: $BIN ($(stat -c %s $BIN) 字节, $(stat -c %y $BIN | cut -d. -f1))"

  if [ ! -x "$NEW" ]; then
    info "★ 新二进制不存在: $NEW"
    return 1
  fi
  info "新二进制: $NEW ($(stat -c %s $NEW) 字节)"

  # ★ 最关键的一条：必须是 onnxruntime 构建，否则依存句法/多模态失效
  if ! go version -m "$NEW" 2>/dev/null | grep -Eq 'build[[:space:]]+-tags=.*onnxruntime'; then
    info "★★ 新二进制**不是** onnxruntime 构建 —— 拒绝部署"
    info "   （package-linux.sh:139 同样会拒绝；缺它会让依存句法与多模态失效）"
    return 1
  fi
  info "✓ onnxruntime 构建标签正确"

  # 运行期依赖
  if [ ! -f /opt/onnxruntime/libonnxruntime.so ] \
     && ! ls /usr/local/lib/libonnxruntime.so* /usr/lib/libonnxruntime.so* >/dev/null 2>&1; then
    info "★ 找不到 libonnxruntime.so —— provider 会降级"
  else
    info "✓ libonnxruntime.so 就位"
  fi

  # 模型资产（运行期目录，不影响编译）
  local mdl=$(du -sh $DATA/models 2>/dev/null | awk '{print $1}')
  info "模型资产: ${mdl:-无}（运行期目录，部署不改动）"

  # 适配器：d3eaff4 的新逻辑在"无历史清单"时不动文件
  if [ -f "$ADAPTERS/.bundled" ]; then
    info "适配器清单: 存在（升级时落后的会被更新，用户改过的会保留）"
  else
    info "适配器清单: 无 ⇒ 首次升级**不动**任何已存在的适配器文件"
  fi
  info "适配器文件数: $(ls $ADAPTERS/*.lua 2>/dev/null | wc -l)"
  return 0
}

# ---------------------------------------------------------------- backup
do_backup() {
  say "备份"
  mkdir -p "$BAK"
  cp -a "$BIN" "$BAK/homed.bin"
  [ -d "$ADAPTERS" ] && cp -a "$ADAPTERS" "$BAK/adapters"
  cp -a /etc/systemd/system/$SVC "$BAK/" 2>/dev/null || true
  info "已备份到: $BAK"
  info "  homed.bin / adapters / unit 文件"
  # 回滚命令
  cat > "$BAK/ROLLBACK.sh" <<RB
#!/bin/bash
# 回滚到 $BAK
set -e
systemctl stop $SVC
cp -a $BAK/homed.bin $BIN
[ -d $BAK/adapters ] && rm -rf $ADAPTERS && cp -a $BAK/adapters $ADAPTERS
systemctl start $SVC
systemctl is-active $SVC
RB
  chmod +x "$BAK/ROLLBACK.sh"
  info "回滚命令已生成: bash $BAK/ROLLBACK.sh"
}

# ---------------------------------------------------------------- deploy
do_deploy() {
  say "部署"
  do_check || { info "检查未通过，中止"; return 1; }
  echo
  read -r -p "确认替换 $BIN 并重启 $SVC ? 输入 yes 继续: " ans
  [ "$ans" = "yes" ] || { info "已取消"; return 1; }

  do_backup
  say "替换二进制"
  systemctl stop "$SVC"
  # ★ install 而非 cp：保留 setuid/权限语义，且原子替换
  install -m 0755 "$NEW" "$BIN"
  info "已安装: $BIN ($(stat -c %s $BIN) 字节)"
  systemctl start "$SVC"
  sleep 8
  if systemctl is-active --quiet "$SVC"; then
    info "✓ 服务已启动: $(systemctl is-active $SVC)"
  else
    info "✗ 服务启动失败 —— 回滚：bash $BAK/ROLLBACK.sh"
    journalctl -u "$SVC" --since '-2 min' --no-pager | tail -20
    return 1
  fi
  say "部署后验证"
  info "等 20s 让 agent 起来，然后看 /kernel 状态"
}

# ---------------------------------------------------------------- rollback
do_rollback() {
  say "回滚"
  local latest
  latest=$(ls -dt /var/tmp/homed-backup-* 2>/dev/null | head -1)
  if [ -z "$latest" ]; then info "找不到备份"; return 1; fi
  info "使用备份: $latest"
  systemctl stop "$SVC"
  cp -a "$latest/homed.bin" "$BIN"
  [ -d "$latest/adapters" ] && { rm -rf "$ADAPTERS"; cp -a "$latest/adapters" "$ADAPTERS"; }
  systemctl start "$SVC"
  sleep 5
  info "回滚后: $(systemctl is-active $SVC)"
}

case "${1:-check}" in
  check)    do_check ;;
  backup)   do_backup ;;
  deploy)   do_deploy ;;
  rollback) do_rollback ;;
  *) echo "用法: $0 {check|backup|deploy|rollback}"; exit 2 ;;
esac

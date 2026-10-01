#!/usr/bin/env bash
# 重置一个测试实例到「干净单进程」状态，供跑分使用。
#
# 为什么要这个脚本（2026-10-01 实测两次踩坑）：
#
#   1. **只 kill pid 文件里的那一个是不够的。** /var/tmp/ha-c 上曾同时存活 9 个
#      homed 进程（都 -data /var/tmp/ha-c），共享同一个 memory/graph.db。
#      手工清理时只杀了 pid 文件那个，其余仍在跑、持有旧内存状态并继续写库 ——
#      表现为「我明明清了库，实例却还报 384 实体 / 陶瓷（上一轮测试的废话）」。
#
#   2. **删库比清目录可靠。** 手工 rm -rf 一堆子目录容易漏（media.db、context.json、
#      WAL 文件…），而核心启动时会自己建 schema —— 删掉即可，不必手工清干净。
#
# 用法：
#   ./reset-instance.sh <名字>              # 仅清理，不启动
#   ./reset-instance.sh <名字> --start      # 清理后重新启动
#   ./reset-instance.sh <名字> --start --force   # 停进程前先确认没有跑分在用本实例
#
# ★ 为什么要 --force（2026-10-01 踩到）：清理=杀进程+删库，**会把正在跑分的
#   实例连同数据一起毁掉**。实测我为了验证脚本，当着跑分的面把它依赖的实例杀了。
#   因此：检测到本实例上有跑分进程（memory_recall.py 指向本 cli.sock）时拒绝执行，
#   除非显式 --force。
set -uo pipefail

NAME="${1:?用法: reset-instance.sh <名字> [--start] [--force]}"
START="${2:-}"
FORCE="${3:-}"

if [ -n "$FORCE" ] && [ "$FORCE" != "--force" ]; then
  printf '✗ 未知第三参数: %s（只接受 --force）\n' "$FORCE" >&2
  exit 2
fi
DATA="/var/tmp/ha-${NAME}"
BIN="${BIN:-/home/program/TrueAgent/build/homed}"
PORT="${PORT:-18084}"

# 按 -data 精确匹配，只杀这个测试实例；绝不碰生产（-data /home/newqqagent）
ha_pids() { pgrep -f "homed -data $DATA( |$)" || true; }

# 跑分占用门禁：memory_recall.py / bench.py 引用本实例 socket 时不许清理
if [ "$FORCE" != "--force" ]; then
  BUSY=$(ps -eo pid,cmd | grep -a 'memory_recall\|bench\.py' | grep -a -- "$DATA" | grep -v grep | awk '{print $1}' || true)
  if [ -n "$BUSY" ]; then
    printf '✗ 拒绝执行：检测到跑分进程正用本实例（pid %s）\n' "$(echo $BUSY)"
    printf '  确认要清理请加 --force（会毁掉正在跑的数据）\n'
    exit 3
  fi
fi

PIDS=$(ha_pids)
if [ -n "$PIDS" ]; then
  printf '停 %s 个残留进程: %s\n' "$(echo "$PIDS" | wc -l)" "$(echo $PIDS)"
  # shellcheck disable=SC2086
  kill $PIDS 2>/dev/null
  sleep 5
  # shellcheck disable=SC2086
  kill -9 $PIDS 2>/dev/null
  sleep 2
  LEFT=$(ha_pids | grep -c . || true)
  [ "$LEFT" -eq 0 ] || { printf '✗ 仍有 %s 个进程存活，检查后再试\n' "$LEFT"; exit 1; }
fi

# 删库，让核心启动时自建
rm -f  "$DATA"/memory/graph.db* \
      "$DATA"/memory/media/media.db* \
      "$DATA"/memory/context.json
rm -rf "$DATA"/memory/documents "$DATA"/memory/text \
       "$DATA"/memory/raw "$DATA"/memory/scenes \
       "$DATA"/agentfs/* "$DATA"/snapshots/* 2>/dev/null
rm -f  "$DATA"/webui_chat_history.json 2>/dev/null

ENT=$(timeout 5 sqlite3 "$DATA/memory/graph.db" "select count(*) from entities;" 2>/dev/null || echo "?")
printf '已清理 %s（entities=%s）\n' "$DATA" "${ENT:-无库}"

[ "$START" = "--start" ] || exit 0

# 启动后自检：单进程 + ONNX 加载 + 库为空
cd "$DATA" && nohup setsid "$BIN" -data "$DATA" -webui "127.0.0.1:$PORT" \
  >"$DATA/boot.log" 2>&1 &
sleep 30
N=$(ha_pids | grep -c . || true)
printf '进程数 %s%s\n' "$N" "$( [ "$N" = 1 ] && printf ' ✓' || printf ' ✗ 期望 1' )"
grep -a 'multimodal space active' "$DATA/boot.log" | tail -1
timeout 5 sqlite3 "$DATA/memory/graph.db" "select 'entities=' || count(*) from entities;" 2>/dev/null || echo 'entities=?（库锁超时）'

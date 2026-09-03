#!/usr/bin/env bash
# 子进程插件常驻开销实测（Part 6.3 验收项）。
#
# 对照基线：docs/zh/experiments/plugin-arch 实验 5 实测 17 子进程
# PSS=12.9MB / RSS=29.1MB / 线程=84（原文档估计 50-70MB 偏高）。
#
# 用法：./measure-plugin-overhead.sh <homed-pid>
set -uo pipefail

pid=${1:-}
if [ -z "$pid" ]; then
  echo "用法: $0 <homed-pid>" >&2
  exit 1
fi
if [ ! -d "/proc/$pid" ]; then
  echo "进程 $pid 不存在" >&2
  exit 1
fi

# homed 本体
homed_rss=$(awk '/^VmRSS:/ {print $2}' "/proc/$pid/status")
homed_thr=$(awk '/^Threads:/ {print $2}' "/proc/$pid/status")

echo "=== homed 本体 ==="
printf "RSS=%s kB  线程=%s\n" "$homed_rss" "$homed_thr"

# 插件子进程：homed 的直接子进程中执行 plugin.bin 的
echo
echo "=== 插件子进程 ==="
total_rss=0
total_pss=0
total_thr=0
count=0

for child in $(pgrep -P "$pid" 2>/dev/null); do
  exe=$(readlink "/proc/$child/exe" 2>/dev/null || true)
  case "$exe" in
    *plugin.bin*) ;;
    *) continue ;;
  esac

  thr=$(awk '/^Threads:/ {print $2}' "/proc/$child/status" 2>/dev/null || echo 0)
  # RSS 与 PSS 统一从 smaps_rollup 读，保证口径一致。
  # 混用 status 的 VmRSS 与 smaps 的 Pss 会得出 PSS > RSS 的荒谬结果——
  # 两者对共享内存段（Pss_Shmem）的计入方式不同。
  rss=$(awk '/^Rss:/ {print $2}' "/proc/$child/smaps_rollup" 2>/dev/null || echo 0)
  pss=$(awk '/^Pss:/ {print $2}' "/proc/$child/smaps_rollup" 2>/dev/null || echo 0)
  if [ -z "$rss" ] || [ "$rss" = "0" ]; then
    rss=$(awk '/^VmRSS:/ {print $2}' "/proc/$child/status" 2>/dev/null || echo 0)
  fi
  binsz=$(stat -c%s "$(readlink "/proc/$child/exe" 2>/dev/null)" 2>/dev/null || echo 0)
  name=$(basename "$(readlink "/proc/$child/cwd" 2>/dev/null || echo unknown)")

  printf "  %-16s pid=%-8s RSS=%-8s PSS=%-8s 线程=%-3s 二进制=%s MB\n" \
    "$name" "$child" "$rss" "$pss" "$thr" \
    "$(awk -v b="$binsz" 'BEGIN{printf "%.1f", b/1048576}')"
  total_rss=$((total_rss + rss))
  total_pss=$((total_pss + pss))
  total_thr=$((total_thr + thr))
  count=$((count + 1))
done

echo
echo "=== 合计（$count 个插件进程）==="
awk -v rss="$total_rss" -v pss="$total_pss" -v thr="$total_thr" -v n="$count" '
BEGIN {
  printf "RSS=%d kB (%.1f MB)\n", rss, rss/1024
  printf "PSS=%d kB (%.1f MB)\n", pss, pss/1024
  printf "线程=%d\n", thr
  if (n > 0) printf "均摊 RSS=%.2f MB  PSS=%.2f MB  线程=%.1f\n", rss/1024/n, pss/1024/n, thr/n
}'

echo
echo "注：RSS/PSS 均取自 smaps_rollup，口径一致（PSS ≤ RSS）。"
echo "PSS 低于 RSS 的部分即 Go runtime 只读代码页在进程间的共享收益。"

echo
echo "对照实验 5 基线：17 进程 RSS=29.1MB PSS=12.9MB 线程=84"
echo
echo "⚠️ 该基线用的是 2.68MB 的最小插件；真实插件 3.3~15.2MB（browser 依赖最多）。"
echo "   RSS 随二进制体积线性增长，故不可直接与基线数字比较——"
echo "   要比的是「均摊线程数」与「PSS/RSS 比值（共享收益）」这两个结构性指标。"

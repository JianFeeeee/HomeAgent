#!/usr/bin/env bash
# 批量重编外部插件为子进程模式（Part 6.3）。
#
# 用法：./rebuild-plugins.sh <插件名>...
#
# 关键性质：**不修改任何插件源码**。每个插件只需用新版 plugindev 重编，
# plg.json 的 entry 仍写着 "plugin.so" 也无妨——工具链已不看这个字段。
set -uo pipefail

PLUGINDEV=${PLUGINDEV:-/tmp/plugindev}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)/third_party/homeagent-sdk/example"}
export GOCACHE=${GOCACHE:-/tmp/gocache}
export GOPATH=${GOPATH:-/tmp/gopath}

if [ ! -x "$PLUGINDEV" ]; then
  echo "plugindev 不存在或不可执行: $PLUGINDEV" >&2
  exit 1
fi

ok=0
fail=0
failed_names=""

for name in "$@"; do
  dir="$EXAMPLE_DIR/$name"
  if [ ! -d "$dir" ]; then
    echo "✗ $name: 目录不存在"
    fail=$((fail + 1))
    failed_names="$failed_names $name"
    continue
  fi

  # 清理旧 C ABI 产物：同目录残留 .so 不影响构建，但会让人误以为还在用旧通道
  rm -rf "$dir/build" "$dir/dist"

  out=$(cd "$dir" && "$PLUGINDEV" build 2>&1)
  rc=$?

  # 判定成功的依据是产物存在，而非退出码：plugindev 对部分错误只打印不退出
  if [ $rc -eq 0 ] && ls "$dir"/build/plugin.bin* >/dev/null 2>&1; then
    n=$(ls "$dir"/build/plugin.bin* 2>/dev/null | wc -l)
    hmap=$(ls "$dir"/dist/*.hmap 2>/dev/null | head -1)
    printf "✓ %-14s %s 个平台产物  %s\n" "$name" "$n" "$(basename "${hmap:-无 hmap}")"
    ok=$((ok + 1))
  else
    printf "✗ %-14s 构建失败\n" "$name"
    echo "$out" | tail -6 | sed 's/^/    /'
    fail=$((fail + 1))
    failed_names="$failed_names $name"
  fi
done

echo
echo "成功 $ok / 失败 $fail"
[ -n "$failed_names" ] && echo "失败:$failed_names"
exit $([ $fail -eq 0 ] && echo 0 || echo 1)

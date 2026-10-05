#!/usr/bin/env bash
# 迁移后验证 —— 在副本上跑，逐项判定
#
# ★ 验证的目标不是「迁移成功」（那由命令自己报），
#   而是「迁移后的库还能不能用」——
#   召回、结构、场景引用、媒体挂点。
#
# 用法：verify-migration.sh <db路径>
set -uo pipefail

DB="${1:?用法: verify-migration.sh <db路径>}"
fail=0

say() { printf '\n──── %s ────\n' "$1"; }
check() {
  local name="$1" expect="$2" actual="$3"
  if [ "$actual" = "$expect" ]; then
    printf '  ✓ %-34s %s\n' "$name" "$actual"
  else
    printf '  ✗ %-34s 期望 %s，实际 %s\n' "$name" "$expect" "$actual"
    fail=1
  fi
}

say "1. 旧表未被改动（迁移不得动源表）"
for t in entities relations sentences; do
  n=$(sqlite3 "$DB" "SELECT COUNT(*) FROM $t" 2>/dev/null || echo ERR)
  printf '  · %-10s %s\n' "$t" "$n"
done

say "2. 块与边"
blocks=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_blocks" 2>/dev/null)
withvec=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_blocks WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'" 2>/dev/null)
edges=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_block_edges" 2>/dev/null)
printf '  · 块 %s（带向量 %s）\n  · 边 %s\n' "$blocks" "$withvec" "$edges"
[ "$blocks" -ge 1294 ] || { echo "  ✗ 块数不足（旧实体 1294）"; fail=1; }
[ "$edges" -ge 959 ]   || { echo "  ✗ 边数不足（旧关系去重后 959）"; fail=1; }

say "3. ★ 悬空端点（迁移最常见的失败形态）"
d1=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_block_edges e
  WHERE e.source_kind='block' AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.id=e.source_id)" 2>/dev/null)
d2=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_block_edges e
  WHERE e.target_kind='block' AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.id=e.target_id)" 2>/dev/null)
printf '  · 源端悬空 %s， 目标端悬空 %s\n' "$d1" "$d2"
check "悬空边" "0" "$((d1 + d2))"

say "4. ★ 向量完整性（维度与 fingerprint 一致性）"
dims=$(sqlite3 "$DB" "SELECT DISTINCT json_array_length(vector) FROM memory_blocks
  WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'" 2>/dev/null | tr '\n' ',' )
fps=$(sqlite3 "$DB" "SELECT DISTINCT fingerprint FROM memory_blocks
  WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'" 2>/dev/null | tr '\n' ',')
printf '  · 向量维度集合 %s\n  · fingerprint 集合 %s\n' "$dims" "$fps"
n_dim=$(sqlite3 "$DB" "SELECT COUNT(DISTINCT json_array_length(vector)) FROM memory_blocks
  WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'" 2>/dev/null)
n_fp=$(sqlite3 "$DB" "SELECT COUNT(DISTINCT fingerprint) FROM memory_blocks
  WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'" 2>/dev/null)
check "唯一向量维度数" "1" "$n_dim"
check "唯一 fingerprint 数" "1" "$n_fp"

say "5. scene_refs 迁移（旧 kind 必须归零）"
sqlite3 "$DB" "SELECT kind, COUNT(*) FROM scene_refs GROUP BY kind ORDER BY kind" 2>/dev/null | sed 's/^/  · /'
old=$(sqlite3 "$DB" "SELECT COUNT(*) FROM scene_refs WHERE kind IN ('entity','relation')" 2>/dev/null)
check "旧 kind 残留" "0" "$old"
sb=$(sqlite3 "$DB" "SELECT COUNT(*) FROM scene_refs sr WHERE sr.kind='block'
  AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.id=sr.ref_text)" 2>/dev/null)
se=$(sqlite3 "$DB" "SELECT COUNT(*) FROM scene_refs sr WHERE sr.kind='edge'
  AND NOT EXISTS(SELECT 1 FROM memory_block_edges e WHERE e.id=sr.ref_id)" 2>/dev/null)
check "悬空 block 引用" "0" "$sb"
check "悬空 edge 引用" "0" "$se"

say "6. centroid（中心化向量必需）"
if ! sqlite3 "$DB" "SELECT 1 FROM graph_centroid LIMIT 1" >/dev/null 2>&1; then
  echo "  ✗ graph_centroid 表不存在（中心化向量必需，缺它召回质量会显著下降）"
  fail=1
else
  cn=$(sqlite3 "$DB" "SELECT COUNT(*) FROM graph_centroid" 2>/dev/null)
  printf '  · graph_centroid 行数 %s\n' "$cn"
  # 中心为空 ⇒ 中心化退化为恒等 ⇒ 与不中心化等价
  [ "$cn" -gt 0 ] || { echo "  ✗ 中心为空"; fail=1; }
fi

say "7. 关系边属性（confidence / session 不得全空）"
noc=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_block_edges
  WHERE edge_type != 'contains' AND (confidence IS NULL OR confidence = 0)" 2>/dev/null)
tot=$(sqlite3 "$DB" "SELECT COUNT(*) FROM memory_block_edges WHERE edge_type != 'contains'" 2>/dev/null)
printf '  · 关系边 %s，其中 confidence 为 0 的 %s\n' "$tot" "$noc"

printf '\n════════════════════════════\n'
if [ "$fail" = 0 ]; then
  printf '  ✅ 结构验证全部通过\n'
else
  printf '  ❌ 有验证项未通过\n'
fi
exit $fail

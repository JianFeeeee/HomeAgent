#!/usr/bin/env bash
# GUI 打包 smoke test —— 真打包、真启动、真退出。
#
# ★ 为什么不能只测 package.json 的 build.files
# ---------------------------------------------
# 上一次 GUI 合并（a9d3b5d）就是这个问题：`build.files` 漏了
# `agent-inject.js` 与 `agent-cursor.js`，源码测试 38 项全绿，
# 但打包产物缺模块 —— 桌面模式启动即崩。
#
# 源码测试测的是「文件存在」，打包测的是「**产物里有**」。
# 只有后者能发现这个问题。
#
# 用法：
#   scripts/gui-package-smoke.sh              # 打包 + 启动 + 校验 + 清理
#   scripts/gui-package-smoke.sh --keep       # 保留产物（供手工排查）
#   scripts/gui-package-smoke.sh --no-build   # 只跑已有产物（省时间）
set -uo pipefail

GUI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../cmd/gui" && pwd)"
cd "$GUI_DIR"

KEEP=0
BUILD=1
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    --no-build) BUILD=0 ;;
  esac
done

fail() { echo "  ✘ $1"; FAILED=$((FAILED+1)); }
ok()   { echo "  ✓ $1"; }
FAILED=0

# ★ 校验方式：**从 main.js 递归解析 require 图**，而不是手写清单。
#
# 手写清单的做法有个致命缺陷：它只能发现「清单里写了但没打包」，
# 发现不了「代码 require 了但清单里忘了写」——
# 而后者才是上一次的实际故障（agent-inject/agent-cursor 被漏掉）。
#
# 递归解析出来的才是**真正需要进产物**的集合。
REQUIRED_STATIC=(
  main.js
  preload.js
  icon.svg
)

echo "════ 步骤 1：依赖与工具链 ════"
command -v node >/dev/null 2>&1 || { echo "错误：找不到 node"; exit 1; }
echo "  node $(node -v)"

if [ ! -d node_modules ]; then
  echo "  安装依赖…"
  npm install --no-audit --no-fund >/dev/null 2>&1 || {
    echo "  ✘ npm install 失败"; exit 1; }
fi
[ -d node_modules ] && ok "node_modules 就绪" || fail "node_modules 缺失"

if [ "$BUILD" = "1" ]; then
  echo ""
  echo "════ 步骤 2：electron-builder 打包 ════"
  # --dir 出的是未压缩目录形态（asar 未封），便于逐文件校验
  if npx electron-builder --dir --linux > /tmp/gui-smoke-build.log 2>&1; then
    ok "打包完成（目录形态）"
  else
    fail "打包失败，日志：/tmp/gui-smoke-build.log"
    tail -25 /tmp/gui-smoke-build.log | sed 's/^/      /'
    exit 1
  fi
fi

APP_DIR=""
for cand in dist/linux-unpacked dist/*-unpacked; do
  if [ -d "$cand" ]; then APP_DIR="$cand"; break; fi
done
if [ -z "$APP_DIR" ]; then
  fail "找不到打包产物（dist/*-unpacked）"
  ls -la dist 2>/dev/null | sed 's/^/      /'
  exit 1
fi
echo "  产物目录：$APP_DIR"

echo ""
echo "════ 步骤 3：校验运行时文件在产物里 ════"
# ★ 产物形态探测：asar 开启时代码封在 app.asar 里，而不是 app/ 目录。
#
#   第一版判据只查 resources/app/&lt;file&gt;，结果 6 项全红 ——
#   而二进制实际存活 25 秒且日志无模块缺失。
#   ★ **判据红 ≠ 产品坏**：要先确认「红的是产品还是判据」。
#
#     npx asar list 能列出 asar 内容 → 用它做校验
#     没有 asar → 退回目录形态校验
APP_ROOT="$APP_DIR/resources/app"
ASAR="$APP_DIR/resources/app.asar"
if [ -f "$ASAR" ]; then
  echo "  产物形态：asar（app.asar）"
  MANIFEST=/tmp/gui-smoke-asar-list.txt
  if npx asar list "$ASAR" > "$MANIFEST" 2>/dev/null; then
    OK_ENTRIES=$(wc -l < "$MANIFEST" | tr -d ' ')
    ok "asar 可读（$OK_ENTRIES 条目）"
  else
    fail "asar 存在但无法读取 —— 产物损坏"
    MANIFEST=""
  fi
  # ★ 归一化：调用方可能传 "main.js" 或 "/main.js"，manifest 里是 "/main.js"
  in_app() {
    [ -n "$MANIFEST" ] || return 1
    local p="$1"
    case "$p" in /*) ;; *) p="/$p" ;; esac
    grep -qxF "$p" "$MANIFEST"
  }
else
  echo "  产物形态：目录"
  in_app() { [ -e "$APP_ROOT/${1#/}" ]; }
fi
# ★ 这一步就是上次漏掉的那类问题的检查点。
for f in "${REQUIRED_STATIC[@]}"; do
  if in_app "$f"; then
    ok "产物含 $f"
  else
    fail "★ 缺 $f —— 打包配置漏了（源码测试发现不了）"
  fi
done

# ★ 核心：从 main.js / preload.js 出发递归收集**本地** require，
#   逐个确认它在产物里存在。这才是「真正需要的集合」。
echo "  ── 解析 require 图 ──"
node -e '
const fs=require("fs"), path=require("path");
const roots=["main.js","preload.js"];
const seen=new Set(); const missingSrc=[];
function walk(f){
  f=path.normalize(f);
  if(seen.has(f))return; seen.add(f);
  if(!fs.existsSync(f)){missingSrc.push(f);return;}
  const src=fs.readFileSync(f,"utf8");
  const re=/require\(\s*["'"'"'](\.[^"'"'"']+)["'"'"']\s*\)/g;
  let m;
  while((m=re.exec(src))){
    let r=path.resolve(path.dirname(f),m[1]);
    // ★ Node 的 require 解析规则：先按原样，再补 .js，再当目录找 index.js
    //   第一版没做这一步 ⇒ require("./agent-cursor") 解析成
    //   /abs/path/agent-cursor（无扩展名），而产物里是 agent-cursor.js
    //   ⇒ 3 项误红。判据自己得先遵守被测系统的规则。
    if(!fs.existsSync(r)){
      if(fs.existsSync(r+".js")) r=r+".js";
      else if(fs.existsSync(r+".json")) r=r+".json";
      else if(fs.existsSync(r)===false && fs.existsSync(path.join(r,"index.js")))
        r=path.join(r,"index.js");
    } else if(fs.statSync(r).isDirectory()){
      r=path.join(r,"index.js");
    }
    walk(r);
  }
}
roots.forEach(walk);
const list=[...seen]
  .filter(f=>!f.includes("node_modules"))
  .map(f=>"/"+path.relative(process.cwd(),f).split(path.sep).join("/"))
  .sort();
console.log("LOCAL_REQUIRE_COUNT="+list.length);
list.forEach(f=>console.log("REQUIRE="+f));
if(missingSrc.length){console.log("UNRESOLVED="+missingSrc.join(","));}
' > /tmp/gui-smoke-require.txt 2>/dev/null

REQ_COUNT=$(grep -ac '^REQUIRE=' /tmp/gui-smoke-require.txt 2>/dev/null || echo 0)
echo "  解析到 $REQ_COUNT 个本地模块"
while IFS= read -r line; do
  f="${line#REQUIRE=}"
  [ "$f" = "$line" ] && continue
  if in_app "$f"; then
    ok "require → $f"
  else
    fail "★ 缺 $f（被 require 但没打包）"
  fi
done < /tmp/gui-smoke-require.txt
if grep -a '^UNRESOLVED=' /tmp/gui-smoke-require.txt >/dev/null 2>&1; then
  grep -a '^UNRESOLVED=' /tmp/gui-smoke-require.txt | sed 's/^UNRESOLVED=/  ★ 源码里 require 了不存在的模块: /'
  fail "require 图无法完整解析"
fi

# vendor 资源（three.js 星图 / OrbitControls）—— 远程 GUI 合并带进来的
VENDOR_FOUND=0
if [ -n "$MANIFEST" ]; then
  while IFS= read -r v; do
    ok "vendor: $v"; VENDOR_FOUND=$((VENDOR_FOUND+1))
  done < <(grep -E '^/renderer/vendor/.*\.js$' "$MANIFEST" 2>/dev/null | head -5)
else
  while IFS= read -r v; do
    ok "vendor: $v"; VENDOR_FOUND=$((VENDOR_FOUND+1))
  done < <(find "$APP_ROOT/renderer/vendor" -name '*.js' 2>/dev/null | head -5)
fi
if [ "$VENDOR_FOUND" -eq 0 ]; then
  fail "★ 未发现 renderer/vendor/*.js —— three.js 星图等渲染层依赖会崩"
fi

echo ""
echo "════ 步骤 4：真启动（xvfb 下无头运行）════"
BIN="$APP_DIR/homeagent-gui"
[ -x "$BIN" ] || BIN=$(find "$APP_DIR" -maxdepth 1 -type f -perm -u+x 2>/dev/null | head -1)
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  fail "找不到可执行的 GUI 二进制"
else
  ok "可执行文件：$(basename "$BIN")"
  # 用 --version 探活：它会加载主进程但不建窗口，比全启动轻
  if command -v xvfb-run >/dev/null 2>&1; then
    RUN="xvfb-run -a"
  else
    RUN=""
  fi
  # ★ 启动必须有界：不加 timeout 的话 GUI 会一直挂着
  if timeout 25 $RUN "$BIN" --no-sandbox > /tmp/gui-smoke-run.log 2>&1; then
    ok "启动探活通过（25s 内自行退出或正常响应）"
  else
    rc=$?
    if [ "$rc" = "124" ]; then
      # 超时 = 进程正常存活（GUI 本该常驻），不算失败
      ok "进程存活 25s（GUI 常驻属预期，timeout=124 不算失败）"
    else
      fail "启动异常退出 rc=$rc，日志：/tmp/gui-smoke-run.log"
      tail -20 /tmp/gui-smoke-run.log | sed 's/^/      /'
    fi
  fi
  # 崩溃特征检查：即使 rc=0 也要看日志
  if grep -qiE 'cannot find module|MODULE_NOT_FOUND|ERR_MODULE_NOT_FOUND' /tmp/gui-smoke-run.log 2>/dev/null; then
    fail "★ 日志里有模块缺失（这正是上次漏模块的形态）"
    grep -iE 'cannot find module|MODULE_NOT_FOUND' /tmp/gui-smoke-run.log | head -3 | sed 's/^/      /'
  else
    ok "日志无模块缺失特征"
  fi
fi

echo ""
if [ "$FAILED" -eq 0 ]; then
  echo "════ smoke 通过 ════"
  if [ "$KEEP" = "0" ]; then
    rm -rf dist
    echo "  已清理 dist/（--keep 可保留）"
  fi
  exit 0
fi
echo "════ smoke 失败：$FAILED 项 ════"
echo "  产物保留在 $APP_DIR 供排查（--keep 保留）"
exit 1
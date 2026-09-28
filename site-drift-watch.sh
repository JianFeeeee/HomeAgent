#!/usr/bin/env bash
# 站点漂移巡检：有差异才提醒，绝不自动部署。
#
# 老大定的档位是「有差异就提醒」而不是「自动推」——文档站发错了是公开可见的，
# 宁可等人点一下。所以这个脚本**只读不动**：不构建、不上传、不碰线上任何文件。
#
# 判三类信号：
#   1. 线上漂移：.106 上的站 ≠ 本机产物/site 源（逐字节 md5 清单比对）
#   2. 源码漂移：git HEAD 比产物新 ⇒ 提交了但没重新构建部署
#   3. 探活失败：.106 或 NapCat 挂了（这类比文档漂移紧急，必须报）
#
# 为什么不直接调 deploy-sdk-site.sh --check 再 grep 输出：那脚本的输出是给人看的
# 彩色文本，拿来当机器判断依据太脆（改个文案就失效）。md5 清单逻辑很短，
# 这里复刻一份，注释指向 deploy-sdk-site.sh 保持同步。
#
# 去重：差异持续存在时，每轮都发会把老大刷屏。所以按「差异指纹」去重——
# 差异内容变了才发新的；差异没了发一条「已恢复」；一直没变就闭嘴。
#
# 定时：cron 7,37 * * * *（错开整点，避开 acme 等已排满 :00 的任务）

set -uo pipefail

SDK_DIR=/home/program/TrueAgent/third_party/homeagent-sdk
BUILD_DIR="$SDK_DIR/site_build"
INTRO_SRC=/home/program/TrueAgent/site
HOST=192.168.2.106
SSH="ssh -n -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no admin@$HOST"
SITES=/vol1/docker/navi-data/sites

NAPCAT=${NAPCAT_URL:-http://192.168.2.106:25570}
QQ_USER=${QQ_USER:-2198972886}

STATE_DIR=/home/program/TrueAgent/.drift-watch
STATE_FILE="$STATE_DIR/state"
LOCK="$STATE_DIR/lock"
LOG=/var/log/site-drift-watch.log

# 差异清单最多列这么行，剩下的折叠计数（QQ 消息不宜过长）
MAX_LINES=12

mkdir -p "$STATE_DIR"
exec 9>"$LOCK"
flock -n 9 || { echo "[$(date '+%F %T')] 上一轮还在跑，跳过" >>"$LOG"; exit 0; }

say()  { printf '\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
log()  { printf '[%s] %s\n' "$(date '+%F %T')" "$*" >>"$LOG"; }

# ── 通知（直连 NapCat，cron 不需要唤醒 agent）────────────────────────
#
# 为什么不走 agent：叫醒 agent 发消息要过 LLM 一趟，慢且烧 token；
# NapCat 的 HTTP 接口是现成的，直发即时且零成本。
notify() {
  local msg="$1" payload resp code
  payload=$(jq -nc --arg m "$msg" --argjson u "$QQ_USER" '{user_id:$u, message:$m}')
  resp=$(curl -s -m 20 -X POST "$NAPCAT/send_private_msg" \
           -H 'Content-Type: application/json' -d "$payload" 2>/dev/null)
  code=$(printf '%s' "$resp" | jq -r '.retcode // -1' 2>/dev/null)
  if [ "$code" = "0" ]; then
    info "已通知老大（$(printf '%s' "$resp" | jq -r '.data.message_id // "?"')）"
    return 0
  fi
  info "✗ 通知发送失败：$(printf '%s' "$resp" | head -c 200)"
  log "notify failed: $resp"
  return 1
}

# ── md5 清单（与 deploy-sdk-site.sh 保持一致）─────────────────────────
norm_md5() { sed 's|  \./|  |'; }

list_md5() {  # list_md5 <远端目录> <排除项...>
  local dir="$1"; shift
  local excl=()
  local e
  for e in "$@"; do excl+=(-not -path "$e"); done
  $SSH "cd $dir && sudo -n find . -type f -print0 | sort -z | sudo -n xargs -0 md5sum" 2>/dev/null | norm_md5
}

sdk_diff() {
  diff <( cd "$BUILD_DIR" && find . -type f -print0 | sort -z | xargs -0 md5sum | norm_md5 ) \
       <( list_md5 "$SITES/sdk" ) 2>/dev/null
}

intro_diff() {
  # README.md 是仓库说明不是站点资源，故意排除（见 deploy-sdk-site.sh 注释）
  diff <( cd "$INTRO_SRC" && find index.html assets -type f -print0 | sort -z | xargs -0 md5sum | norm_md5 ) \
       <( list_md5 "$SITES/introduce" -not -path './README.md' ) 2>/dev/null
}

# ── 探活 ────────────────────────────────────────────────────────────
probe() {
  local url="$1"
  curl -s -m 15 -X POST "$url" 2>/dev/null | jq -r '.retcode // -1' 2>/dev/null
}

# ── 主流程 ──────────────────────────────────────────────────────────
FORCE=0; DRY=0
for a in "$@"; do
  case "$a" in
    --test)  FORCE=1 ;;
    --dry-run) DRY=1 ;;
  esac
done

say "巡检 $HOST 的两个站（只读，不部署）"

# 1) NapCat 探活（通知通路本身也得是活的，否则有差异也通知不到）
nap=off
if [ "$(probe "$NAPCAT/get_status")" = "0" ]; then
  nap=on; info "✓ NapCat 通知通路在线"
else
  info "✗ NapCat 不可达（$NAPCAT）——有差异也发不出通知"
fi

# 2) 线上漂移
reasons=(); details=""

# .106 整体可达性：连不上是最高优先级故障
if ! $SSH "true" 2>/dev/null; then
  info "✗ $HOST SSH 不可达（admin@，BatchMode）"
  reasons+=("🖥 $HOST SSH 连不上，站点状态未知")
  sdk_diff() { echo "__UNREACHABLE__"; }
fi

sdkout=$(sdk_diff)
if [ "$sdkout" = "__UNREACHABLE__" ]; then
  :
elif [ -n "$sdkout" ]; then
  n=$(printf '%s\n' "$sdkout" | wc -l)
  reasons+=("📄 sdk 站与本地产物有差异（$n 行）")
  details+="【sdk 站】"$'\n'"$(printf '%s\n' "$sdkout" | head -$MAX_LINES)"
  [ "$n" -gt "$MAX_LINES" ] && details+=$'\n'"…还有 $((n - MAX_LINES)) 行"
  details+=$'\n\n'
  info "✗ sdk 有差异：$n 行"
else
  info "✓ sdk 站与本地产物逐字节一致"
fi

introout=$(intro_diff)
if [ "$introout" = "__UNREACHABLE__" ]; then
  :
elif [ -n "$introout" ]; then
  n=$(printf '%s\n' "$introout" | wc -l)
  reasons+=("🎨 introduce 站与本机源有差异（$n 行）")
  details+="【introduce 站】"$'\n'"$(printf '%s\n' "$introout" | head -$MAX_LINES)"
  [ "$n" -gt "$MAX_LINES" ] && details+=$'\n'"…还有 $((n - MAX_LINES)) 行"
  details+=$'\n\n'
  info "✗ introduce 有差异：$n 行"
else
  info "✓ introduce 站与本机源逐字节一致"
fi

# 3) 源码漂移：提交了但没重新构建
head_ts=$( cd "$SDK_DIR" && git log -1 --format=%ct 2>/dev/null )
build_ts=0
[ -d "$BUILD_DIR" ] && build_ts=$(stat -c %Y "$BUILD_DIR/index.html" 2>/dev/null || echo 0)
if [ -n "$head_ts" ] && [ "$build_ts" -gt 0 ] && [ "$head_ts" -gt "$build_ts" ]; then
  hs=$(cd "$SDK_DIR" && git log -1 --format='%h %s' 2>/dev/null)
  reasons+=("🧱 sdk 源码已提交但产物没重新构建（$hs）")
  details+="【源码漂移】"$'\n'"HEAD $head_ts > 产物 $build_ts"$'\n'"$(cd "$SDK_DIR" && git log -1 --format='%h %ad %s' --date=iso 2>/dev/null)"$'\n\n'
  info "✗ 源码比产物新，需重新构建"
else
  info "✓ 源码与产物无漂移"
fi

# ── 去重与通知 ──────────────────────────────────────────────────────
fingerprint=$(printf '%s\n' "${reasons[@]:-}" "$details" | md5sum | cut -c1-16)
prev=""; prev_state=""
[ -f "$STATE_FILE" ] && { prev_state=$(jq -r '.state // ""' "$STATE_FILE" 2>/dev/null); prev=$(jq -r '.fingerprint // ""' "$STATE_FILE" 2>/dev/null); }

if [ "$FORCE" = "1" ]; then
  info "--test：强制通知一次"
  [ "$DRY" = "1" ] && { say "（dry-run，不实际发送）"; }
  [ "$DRY" != "1" ] && notify "【站点巡检 · 测试】通知通路自检（当前差异条数：${#reasons[@]}）——看到这条说明巡检能及时叫你。"
  exit 0
fi

if [ "${#reasons[@]}" -eq 0 ]; then
  if [ "$prev_state" = "alert" ]; then
    msg="✅【站点巡检】已恢复正常：sdk 与 introduce 两站都和本机逐字节一致，源码也无漂移。刚才的差异自己好了（或你部署过了）。"
    [ "$DRY" = "1" ] || notify "$msg"
    log "recovered (was alert), notified=$([ "$DRY" = "1" ] && echo dry || echo yes)"
  else
    info "无差异，按约定保持安静（不打扰老大）"
    log "clean"
  fi
  printf '{"state":"clean","fingerprint":"%s","ts":"%s"}\n' "$fingerprint" "$(date -Is)" >"$STATE_FILE.tmp" && mv "$STATE_FILE.tmp" "$STATE_FILE"
  exit 0
fi

# 有差异
if [ "$fingerprint" = "$prev" ] && [ "$prev_state" = "alert" ]; then
  info "差异与上次通知完全相同（指纹 $fingerprint），不重复打扰"
  log "alert-unchanged fp=$fingerprint"
  exit 0
fi

say "有差异，准备通知"
msg="🚨【站点巡检】发现 ${#reasons[@]} 处漂移（只提醒，未自动部署）"$'\n\n'
for r in "${reasons[@]}"; do msg+="· $r"$'\n'; done
msg+=$'\n'"$details"
msg+=$'\n'"—— 要部署就跑：/home/program/TrueAgent/deploy-sdk-site.sh"
msg+=$'\n'"   只看差异：/home/program/TrueAgent/deploy-sdk-site.sh --check"
msg+=$'\n'"   回滚：--rollback <备份目录名>"

if [ "$DRY" = "1" ]; then
  say "（dry-run，以下内容本该发出）"; printf '%s\n' "$msg"
else
  notify "$msg" && printf '{"state":"alert","fingerprint":"%s","ts":"%s"}\n' "$fingerprint" "$(date -Is)" >"$STATE_FILE.tmp" && mv "$STATE_FILE.tmp" "$STATE_FILE"
fi
log "alert fp=$fingerprint reasons=${#reasons[@]}"

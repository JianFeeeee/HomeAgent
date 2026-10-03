#!/usr/bin/env bash
# 起一个配置好的隔离实例（供标定/对比用）。
#
# 为什么需要脚本：手工起实例踩过三次同样的坑 ——
#   1. 忘了配 llmsproxy（于是打到真实 DeepSeek，报 401）；
#   2. 忘了设 context_window（于是按模型名推断，两侧窗口不一致）；
#   3. 改了配置忘了重启（provider 在启动时构建，/settings set 落库但不生效）。
#
# 用法：
#   ./spawn-instance.sh <名字> <webui端口> [context_window]
#   ./spawn-instance.sh b 18083 200000
#
# 端口分配约定（避免撞车）：
#   18082=A 18083=B 18084=C …（webui，可自由分配）
#   注意：pluginmgr(9876)/remotedevice(9890)/kbtree(9892) 是**写死的**，
#   同机第二个实例会在它们上 bind 失败（那几个插件降级，内核其余部分照常）。
#   对经 cli.sock 的标定无影响，但别以为实例是"完全隔离"的。
set -uo pipefail

NAME="${1:?用法: spawn-instance.sh <名字> <webui端口> [context_window]}"
PORT="${2:?缺 webui 端口}"
CTX="${3:-200000}"

BIN="${BIN:-/home/program/TrueAgent/build/homed}"
DATA="/var/tmp/ha-${NAME}"
PROD_CFG="/home/newqqagent/config.db"

say() { printf '  %s\n' "$*"; }

if [ ! -x "$BIN" ]; then
  say "★ 二进制不存在: $BIN（先 make build）"; exit 1
fi

# ★ 跑分前必须核对内核版本 —— 否则测的是旧内核，分数毫无意义
#
# 实测踩过（2026-10-04）：build/homed 是 Oct 1 的，而当天有 19 个
# memory 层提交（融合召回 / 拒答判据 / 仲裁 / schema 退场）。
# 直接跑分测的**完全是三天前的内核**，当天的工作一点没测到 ——
# 而 6/6 的满分让人以为改动被验证过了。
#
# 用 --allow-stale 显式跳过（只在你确实想测旧版本时）。
if [ "${ALLOW_STALE:-0}" != "1" ]; then
  BIN_REV="$(go version -m "$BIN" 2>/dev/null | sed -n 's/.*vcs.revision=\([0-9a-f]*\).*/\1/p' | head -1)"
  # ★ 两者都要短形式：go version -m 给的是完整 40 位，
  #   而 rev-parse --short 给短形式 ⇒ 不统一就永远「不匹配」。
  HEAD_REV="$(git -C /home/program/TrueAgent rev-parse --short=8 HEAD 2>/dev/null)"
  BIN_REV="${BIN_REV:0:8}"
  BIN_TIME="$(stat -c %y "$BIN" 2>/dev/null | cut -d. -f1)"
  if [ -z "$BIN_REV" ] || [ -z "$HEAD_REV" ]; then
    say "· 读不到版本信息（未装 git 或非 git 构建），跳过核对"
  elif [ "$BIN_REV" != "$HEAD_REV" ]; then
    say "★ 内核版本不符，拒绝启动跑分实例"
    say "    二进制 $BIN_REV ($BIN_TIME)"
    say "    HEAD   $HEAD_REV"
    say "    改动可能没被测到。重跑：go build -tags onnxruntime -o build/homed ./cmd/homed"
    say "    确实想测旧版本：ALLOW_STALE=1 $0 $*"
    exit 2
  else
    say "✓ 内核版本匹配 $BIN_REV（$BIN_TIME）"
  fi
fi

say "实例: name=$NAME data=$DATA webui=127.0.0.1:$PORT ctx=$CTX"

# ── 首次启动生成 config.db ──
if [ ! -f "$DATA/config.db" ]; then
  rm -rf "$DATA"; mkdir -p "$DATA"
  ( cd "$DATA" && nohup "$BIN" -data "$DATA" -webui "127.0.0.1:$PORT" \
      >"$DATA/boot.log" 2>&1 & echo $! > "$DATA/pid" )
  sleep 9
  if [ -f "$DATA/pid" ]; then kill "$(cat "$DATA/pid")" 2>/dev/null; fi
  sleep 2
  say "已生成 config.db（首次启动）"
fi

# ── 停机后直改 sqlite：指向 llmsproxy（开机构建 provider，必须在启动前改）──
KEY=$(sqlite3 "$PROD_CFG" "SELECT value FROM config WHERE key='core.llm.api_key';")
[ -n "$KEY" ] || { say "★ 取不到 llmsproxy key"; exit 1; }

sqlite3 "$DATA/config.db" <<SQL
UPDATE config SET value='llmsproxy' WHERE key='core.llm.provider';
UPDATE config SET value='http://127.0.0.1:8081/v1' WHERE key='core.llm.base_url';
UPDATE config SET value='$KEY' WHERE key='core.llm.api_key';
UPDATE config SET value='AUTO' WHERE key='core.llm.model';
UPDATE config SET value='openai' WHERE key='core.llm.adapter';
UPDATE config SET value='http://127.0.0.1:8081/v1' WHERE key='core.llm.sources.deepseek.base_url';
UPDATE config SET value='$KEY' WHERE key='core.llm.sources.deepseek.api_key';
UPDATE config SET value='AUTO' WHERE key='core.llm.sources.deepseek.model';
UPDATE config SET value='openai' WHERE key='core.llm.sources.deepseek.adapter';
INSERT OR REPLACE INTO config(key,value) VALUES('core.llm.sources.deepseek.context_window','$CTX');
SQL
say "已配置 llmsproxy + context_window=$CTX"

# ── 启动 ──
( cd "$DATA" && nohup "$BIN" -data "$DATA" -webui "127.0.0.1:$PORT" \
    >"$DATA/run.log" 2>&1 & echo $! > "$DATA/pid" )
sleep 10

if ! ss -ltn 2>/dev/null | grep -q ":$PORT"; then
  say "★ 端口 $PORT 没起来，看 $DATA/run.log"; tail -5 "$DATA/run.log"; exit 1
fi

WEBKEY=$(sqlite3 "$DATA/config.db" "SELECT value FROM config_webui WHERE key='api_key';")
CLIKEY=$(sqlite3 "$DATA/config.db" "SELECT value FROM config_cli WHERE key='api_key';")
[ -n "$CLIKEY" ] || CLIKEY="$WEBKEY"   # cli 空时回落到 webui key（cliAPIKey 的兜底）

grep -E "main agent started" "$DATA/run.log" | tail -1 | sed 's/^/  /'

# ── 稠密向量空间自检 ──
# 2026-09-30 教训：模型目录缺失时内核静默降级（TF-IDF fallback），跑分照跑，
# 50 分钟测的是残废配置。跑分实例必须断言 multimodal space active。
MODEL_DIR="$DATA/models/chinese-clip-vit-b16-onnx"
if [ ! -d "$MODEL_DIR" ]; then
  if [ -d "/usr/lib/homeagent/models/chinese-clip-vit-b16-onnx" ]; then
    mkdir -p "$DATA/models"
    ln -s /usr/lib/homeagent/models/chinese-clip-vit-b16-onnx "$MODEL_DIR"
    say "模型缺失 → 已从系统目录 symlink：$MODEL_DIR"
    # 关停重启（provider 启动时构建，改了必须重启）
    kill "$(cat "$DATA/pid")" 2>/dev/null; sleep 3
    ( cd "$DATA" && nohup "$BIN" -data "$DATA" -webui "127.0.0.1:$PORT" \
        >"$DATA/run.log" 2>&1 & echo $! > "$DATA/pid" )
    sleep 10
  else
    say "★ 模型目录缺失且系统目录也没有：$MODEL_DIR"
    say "  跑分将测到禁用稠密检索的残废配置。先解决模型："
    say "  ① python3 scripts/export_chineseclip_onnx.py  ② 或从生产实例拷贝  ③ 或装发行包"
    exit 1
  fi
fi
if ! grep -q 'multimodal space active' "$DATA/run.log"; then
  say "★ 多模态向量空间未激活（跑分无效，拒跑）。启动日志："
  grep -i 'multimodal\|chineseclip' "$DATA/run.log" | sed 's/^/  /'
  exit 1
fi
grep -E 'multimodal space active' "$DATA/run.log" | tail -1 | sed 's/^/  /'

say "pid=$(cat "$DATA/pid")  cli.sock=$DATA/cli.sock"
say "api_key=$CLIKEY"
echo "$CLIKEY" > "$DATA/apikey"
say "就绪。api_key 已写入 $DATA/apikey"

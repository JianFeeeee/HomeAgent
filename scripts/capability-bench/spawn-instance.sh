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
say "pid=$(cat "$DATA/pid")  cli.sock=$DATA/cli.sock"
say "api_key=$CLIKEY"
echo "$CLIKEY" > "$DATA/apikey"
say "就绪。api_key 已写入 $DATA/apikey"

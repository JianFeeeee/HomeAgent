#!/usr/bin/env bash
# HomeAgent 首次初始化脚本
# 在安装后执行，生成凭据并初始化数据库
set -e

DATA_DIR="${HOMEAGENT_DATA:-/var/lib/homeagent}"
CRED_FILE="${DATA_DIR}/credentials.txt"
CONFIG_DB="${DATA_DIR}/config.db"
WAITER_CONF="${DATA_DIR}/waiter.yaml"
INITCONFIG_BIN="/usr/bin/initconfig"

# 如果已经初始化过，跳过
if [ -f "$CONFIG_DB" ] && [ -f "$CRED_FILE" ]; then
  exit 0
fi

mkdir -p "$DATA_DIR"

# 生成随机凭据
API_KEY=$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d '-' || echo "homeagent$(date +%s)")
WEBUI_USER="${WEBUI_USER:-admin}"
WEBUI_PASS="${WEBUI_PASS:-$(openssl rand -hex 12 2>/dev/null || echo "homeagent")}"

# 初始化数据库
if [ -x "$INITCONFIG_BIN" ]; then
  "$INITCONFIG_BIN" \
    -data "$DATA_DIR" \
    -username "$WEBUI_USER" \
    -password "$WEBUI_PASS" \
    -apikey "$API_KEY" 2>/dev/null
fi

# 保存凭据
cat > "$CRED_FILE" << CRED
===================================
  HomeAgent 初始配置信息
  请妥善保管，安装后仅此一份
===================================
WebUI 地址:    http://localhost:8080
API Key:       ${API_KEY}
WebUI 用户名:  ${WEBUI_USER}
WebUI 密码:    ${WEBUI_PASS}
CLI Socket:    ${DATA_DIR}/cli.sock
===================================
CRED
chmod 600 "$CRED_FILE"

# 配置 waiter CLI
cat > "$WAITER_CONF" << WAITER
socket: "${DATA_DIR}/cli.sock"
api_key: ${API_KEY}
default: local
connections:
  - name: local
    socket: "${DATA_DIR}/cli.sock"
    api_key: ${API_KEY}
WAITER

echo ""
echo "============================================"
echo "  HomeAgent 初始化完成"
echo "============================================"
cat "$CRED_FILE"

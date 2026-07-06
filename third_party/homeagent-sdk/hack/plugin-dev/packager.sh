#!/usr/bin/env bash
set -euo pipefail

# HomeAgent 插件打包工具
# 将插件目录打包为 .hmap 分发包
# 用法: ./packager.sh <plugin-dir> [输出路径]
# 示例: ./packager.sh ./plugins/myplugin ./dist/myplugin-1.0.0.hmap

PLUGIN_DIR="${1:-}"
OUTPUT="${2:-}"

if [ -z "$PLUGIN_DIR" ]; then
	echo "用法: $0 <plugin-dir> [输出路径]"
	echo "示例: $0 ./plugins/myplugin ./dist/myplugin-1.0.0.hmap"
	exit 1
fi

PLUGIN_DIR="$(realpath "$PLUGIN_DIR")"
PLUGIN_NAME="$(basename "$PLUGIN_DIR")"

# 验证
if [ ! -f "$PLUGIN_DIR/plugin.json" ]; then
	echo "错误: 不存在 plugin.json: $PLUGIN_DIR"
	exit 1
fi

VERSION="$(python3 -c "import json; print(json.load(open('$PLUGIN_DIR/plugin.json'))['version'])" 2>/dev/null || echo "unknown")"

if [ -z "$OUTPUT" ]; then
	mkdir -p dist
	OUTPUT="$(realpath "dist/${PLUGIN_NAME}-${VERSION}.hmap")"
fi

echo "🔨 打包插件: $PLUGIN_NAME v$VERSION"
echo "   源目录: $PLUGIN_DIR"
echo "   输出:   $OUTPUT"

# 检查入口文件
ENTRY="$(python3 -c "import json; print(json.load(open('$PLUGIN_DIR/plugin.json'))['entry'])" 2>/dev/null || true)"
if [ -n "$ENTRY" ] && [ ! -f "$PLUGIN_DIR/$ENTRY" ]; then
	echo "⚠️  入口文件不存在: $ENTRY"
	echo "   请先编译: cd $PLUGIN_DIR && make"
	exit 1
fi

# 检查已编译的 .so
if [ -f "$PLUGIN_DIR/plugin.so" ] && [ "$(stat -c %Y "$PLUGIN_DIR/plugin.so" 2>/dev/null)" -lt "$(stat -c %Y "$PLUGIN_DIR/plugin.go" 2>/dev/null)" ]; then
	echo "⚠️  plugin.so 比 plugin.go 旧，建议重新编译"
	echo "   请执行: cd $PLUGIN_DIR && make"
fi

cd "$PLUGIN_DIR"
zip -r "$OUTPUT" . -x "*.git*" "Makefile" ".gitignore" "*.go" "go.mod" "go.sum" "*.test" "testdata/*" "_*" 2>&1 | tail -3

echo ""
echo "✅ 打包完成: $OUTPUT"
echo "   大小: $(ls -lh "$OUTPUT" | awk '{print $5}')"
echo ""
echo "安装方式:"
echo "  1. WebUI 插件管理 → 上传安装"
echo "  2. AI 对话: 使用 plugin_install 工具并上传 URL"

#!/usr/bin/env bash
set -euo pipefail

# HomeAgent 插件脚手架生成工具
# 用法: ./scaffold.sh <plugin-name> [输出目录]
# 示例: ./scaffold.sh myplugin ./plugins/myplugin

NAME="${1:-}"
OUTDIR="${2:-./plugins/$NAME}"

if [ -z "$NAME" ]; then
	echo "用法: $0 <plugin-name> [输出目录]"
	echo "示例: $0 myplugin ./plugins/myplugin"
	exit 1
fi

if [ -d "$OUTDIR" ]; then
	echo "错误: 目标目录已存在: $OUTDIR"
	exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEMPLATE_DIR="$SCRIPT_DIR/templates"

mkdir -p "$OUTDIR"

# 替换模板中的占位符
sed -e "s/{{.Name}}/$NAME/g" \
	-e "s/{{.Version}}/0.1.0/g" \
	-e "s/{{.Description}}//g" \
	-e "s/{{.Author}}//g" \
	"$TEMPLATE_DIR/plugin.json.tmpl" > "$OUTDIR/plugin.json"

cp "$TEMPLATE_DIR/plugin.go.tmpl" "$OUTDIR/plugin.go"
cp "$TEMPLATE_DIR/Makefile.tmpl" "$OUTDIR/Makefile"
cp "$TEMPLATE_DIR/gitignore.tmpl" "$OUTDIR/.gitignore"

echo "✅ 插件脚手架已生成: $OUTDIR"
echo ""
echo "下一步:"
echo "  1. 编辑 $OUTDIR/plugin.go 实现业务逻辑"
echo "  2. 编辑 $OUTDIR/plugin.json 完善元信息"
echo "  3. cd $OUTDIR && make          # 编译 plugin.so"
echo "  4. make package                # 打包为 .hmap 分发包"
echo "  5. 通过 WebUI 或 plugin_install 工具安装"

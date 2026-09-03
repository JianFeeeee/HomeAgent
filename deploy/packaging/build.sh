#!/usr/bin/env bash
set -euo pipefail

# 本脚本位于 deploy/packaging/，故仓库根在上两级。
#
# v0.7.2 的根目录清理把 package/build.sh 移到 deploy/packaging/build.sh
# （深度 1 → 2 层），但这行的 ".." 没跟着改成 "../.."，于是 PROJECT_ROOT
# 变成了 <repo>/deploy：产物落进 deploy/build/、GUI 去找 deploy/cmd/gui。
# 跨平台构建从那次起一直是坏的（Makefile 的单平台 build 不走这里，所以没暴露）。
PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD_DIR="${PROJECT_ROOT}/build"
VERSION="${VERSION:-$(git -C "$PROJECT_ROOT" describe --tags --dirty 2>/dev/null || echo "0.8.0")}"
COMMIT="${COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse --short HEAD 2>/dev/null || echo "unknown")}"
BUILD_TIME="${BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}"
GO="${GO:-$(command -v go 2>/dev/null || echo "go")}"
LDFLAGS="-X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Version=${VERSION} -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.Commit=${COMMIT} -X gitcode.com/JianFeeeee/HomeAgent/internal/meta.BuildTime=${BUILD_TIME}"

TARGET="${1:-native}"
COMPONENT="${2:-all}"

# ---- platform matrix ----
# homed:   linux/amd64 + linux/arm64 (CGO), macOS native-only (no osxcross),
#          windows/amd64 (MinGW)
# waiter:  all platforms (CGO-free, raw terminal mode is a no-op on non-Linux)
# gui:     electron-builder handles cross-platform natively

case "$TARGET" in
  native)   GOOS="" GOARCH="" ;;
  linux/amd64)  GOOS=linux   GOARCH=amd64  CC="${CC:-}" ;;
  linux/arm64)  GOOS=linux   GOARCH=arm64  CC="${CC:-aarch64-linux-gnu-gcc}" ;;
  darwin/amd64) GOOS=darwin  GOARCH=amd64  CC="${CC:-}" ;;
  darwin/arm64) GOOS=darwin  GOARCH=arm64  CC="${CC:-}" ;;
  windows/amd64) GOOS=windows GOARCH=amd64 CC="${CC:-x86_64-w64-mingw32-gcc}" ;;
  all)
    "$0" linux/amd64   "$COMPONENT"
    "$0" linux/arm64   "$COMPONENT"
    "$0" darwin/amd64  "$COMPONENT"
    "$0" darwin/arm64  "$COMPONENT"
    "$0" windows/amd64 "$COMPONENT"
    exit 0
    ;;
  *)
    echo "Unknown target: $TARGET"
    echo "Usage: $0 [native|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|all]"
    echo "       [all|homed|waiter|initconfig|gui]"
    exit 1
esac

if [ -n "${GOOS:-}" ]; then
  SUFFIX="${GOOS}_${GOARCH}"
  export GOOS GOARCH
fi
if [ -n "${CC:-}" ]; then
  export CC
fi
if [ -n "${CXX:-}" ]; then
  export CXX
fi
export CGO_ENABLED="${CGO_ENABLED:-1}"

mkdir -p "$BUILD_DIR"

# ---- homed (CGO, sqlite3) ----
build_homed() {
  local out="$BUILD_DIR/homed${SUFFIX:+_$SUFFIX}"
  local plat="${GOOS:-linux}/${GOARCH:-amd64}"

  if [ "$GOOS" = "darwin" ] && [ "${CC:-}" = "" ] && [ "$(uname)" != "Darwin" ]; then
    echo "[SKIP] homed ${plat} — requires native macOS build (CGO + sqlite3, no osxcross)"
    return
  fi
  if [ "$GOOS" = "windows" ]; then
    out="${out}.exe"
  fi
  echo "[BUILD] homed ${plat} → $out"
  # Go 用 CC 驱动 CGO 编译与链接，用 CC 指定的交叉工具链来决定目标架构。
  # 必须同时 export CC 给 Go 的 CGO 代码生成器，否则 CGO_ENABLED=1 下的
  # 目标文件与 host 的 ld 不兼容（如 arm64 的 .o 给了 x86_64 的 ld）。
  local _cc="${CC:-cc}"
  CGO_ENABLED=1 CC="$_cc" "$GO" build -trimpath -installsuffix dynlink \
    -ldflags "$LDFLAGS" -o "$out" ./cmd/homed/
  echo "  OK ($(file "$out" | sed 's/.*: //') | $(du -h "$out" | cut -f1))"
}

# ---- waiter (cross-platform, CGO-free) ----
build_waiter() {
  local plat="${GOOS:-linux}/${GOARCH:-amd64}"
  local out="$BUILD_DIR/waiter${SUFFIX:+_$SUFFIX}"
  if [ "$GOOS" = "windows" ]; then out="${out}.exe"; fi

  echo "[BUILD] waiter ${plat} → $out"
  CGO_ENABLED=0 "$GO" build -trimpath -installsuffix dynlink \
    -ldflags "$LDFLAGS" -o "$out" ./cmd/waiter/
  echo "  OK ($(du -h "$out" | cut -f1))"
}

# ---- initconfig (CGO-free 配置初始化器) ----
#
# NSIS 安装包（installer.nsi:220 File "..\build\initconfig.exe"）与
# package-linux.sh 的 stage_variant 都引用它，但此前 build.sh 从不构建它——
# Windows 安装包构建会直接失败在缺文件上。
build_initconfig() {
  local plat="${GOOS:-linux}/${GOARCH:-amd64}"
  local out="$BUILD_DIR/initconfig${SUFFIX:+_$SUFFIX}"
  if [ "$GOOS" = "windows" ]; then out="${out}.exe"; fi

  echo "[BUILD] initconfig ${plat} → $out"
  CGO_ENABLED=0 "$GO" build -trimpath -installsuffix dynlink \
    -ldflags "$LDFLAGS" -o "$out" ./cmd/initconfig/
  echo "  OK ($(du -h "$out" | cut -f1))"
}

# ---- gui (Electron) ----
build_gui() {
  if [ -n "${GOOS:-}" ] && [ "$GOOS" != "$("$GO" env GOOS)" ]; then
    echo "[SKIP] gui ${GOOS}/${GOARCH} — electron-builder handles cross-platform natively; run 'all' on CI host"
    return
  fi

  local gui_dir="$PROJECT_ROOT/cmd/gui"
  echo "[BUILD] gui → $BUILD_DIR/"

  if [ ! -d "$gui_dir/node_modules" ]; then
    echo "  npm install..."
    (cd "$gui_dir" && npm install --production)
  fi

  # 不传 --config：electron-builder 默认从 package.json 的 "build" 键读配置。
  # 传 --config package.json 会让它把**整个** package.json 当配置校验，
  # 于是 devDependencies / build / scripts 全被判为 "unknown property" 而失败。
  (cd "$gui_dir" && npx electron-builder \
    --linux --win --mac \
    --x64 --arm64 \
    -p never \
    -o "$BUILD_DIR")
  echo "  OK"
}

# ---- dispatch ----
case "$COMPONENT" in
  all)   build_homed; build_waiter; build_initconfig; build_gui ;;
  homed) build_homed ;;
  waiter) build_waiter ;;
  initconfig) build_initconfig ;;
  gui)   build_gui ;;
  *)
    echo "Unknown component: $COMPONENT"
    exit 1
esac

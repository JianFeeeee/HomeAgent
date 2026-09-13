#!/usr/bin/env bash
# 构建 Windows 安装器（NSIS）。
#
# ❗安装器**不往 Windows 装 homed**：homed 依赖 fd 继承与统一共享内存区的段内偏移
# 解引用，Windows 句柄模型无法表达（见 cmd/homed/platform_windows.go）。所以安装器的
# 职责是**引导 WSL2，并把 Linux 包送进发行版里按 Linux 方式安装**
# （deploy/packaging/windows/install-via-wsl.ps1）。
#
# 用法: VERSION=1.3.10 bash deploy/packaging/package-windows.sh <server|client|full> [arch]
# 前置: 先产出对应的 Linux 包（VERSION=x bash deploy/packaging/package-linux.sh amd64）
#
# 为什么不复用 build.sh 的 stage_linux_payload：那一段把 dist/linux 下**所有** deb+tar
# 都塞进 payload，而 server/full 的 deb 各带 ~719MB 的 Chinese-CLIP 模型 ⇒ 任何变体的
# 安装器都会膨胀到 ~2.4GB。WSL 侧脚本只取 payload 里的**第一个** .deb
# （install-via-wsl.ps1:141），所以这里按变体只放对应的那一个包。
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# 三个目录都可覆盖：发布件常在 tag 的干净 worktree 里构建，而这个脚本本身
# 可能只存在于 main（例如刚补的驱动脚本还没进 tag）——那种情况下用主仓的脚本 +
# DIST_LINUX/BUILD_DIR/DIST_RELEASE 指向 worktree，避免"脚本不存在"或产物错位。
BUILD_DIR="${BUILD_DIR:-$PROJECT_ROOT/build}"
DIST_LINUX="${DIST_LINUX:-$PROJECT_ROOT/dist/linux}"
DIST_RELEASE="${DIST_RELEASE:-$PROJECT_ROOT/dist/release}"
# ❗NSIS 的 `File` 路径是**相对 .nsi 所在目录**解析的：在 tag 的 worktree 里构建时，
# 必须用**该 tag 里的** installer.nsi，否则它会去主仓的 build/linux-payload 找载荷
# （实测报 `File: "..\..\build\linux-payload\*.*" -> no files found`）。
# 用 tag 里的 .nsi 也正是"发布件与当时的脚本同源"的正确做法。
NSI="${NSI:-$PROJECT_ROOT/deploy/packaging/installer.nsi}"
if [ ! -f "$NSI" ]; then
  echo "[FAIL] 找不到 NSIS 脚本: $NSI" >&2
  exit 1
fi

VARIANT="${1:-server}"
ARCH="${2:-amd64}"
VERSION="${VERSION:-$(git -C "$PROJECT_ROOT" describe --tags 2>/dev/null || echo 0.0.0)}"
VERSION="${VERSION#v}"

case "$VARIANT" in
  server) DEB_GLOB="homeagent-server_${VERSION}_${ARCH}.deb"; SUFFIX="Server"; WANT_GUI=0; WANT_WAITER=0 ;;
  full)   DEB_GLOB="homeagent-full_${VERSION}_${ARCH}.deb";   SUFFIX="Full";   WANT_GUI=1; WANT_WAITER=1 ;;
  client) DEB_GLOB="homeagent-client_${VERSION}_${ARCH}.deb"; SUFFIX="Client"; WANT_GUI=1; WANT_WAITER=1 ;;
  *) echo "用法: $0 <server|client|full> [arch]" >&2; exit 2 ;;
esac

DEB="$(ls -1 "$DIST_LINUX/deb/$DEB_GLOB" "$DIST_LINUX/$DEB_GLOB" 2>/dev/null | head -1 || true)"
if [ -z "$DEB" ]; then
  echo "[FAIL] 找不到 $DEB_GLOB" >&2
  echo "       先产出 Linux 包：VERSION=$VERSION bash deploy/packaging/package-linux.sh $ARCH" >&2
  echo "       （安装器的作用是把 Linux 包送进 WSL2，所以必须先有 Linux 包）" >&2
  exit 1
fi

# client/full 还要带 Windows GUI（HAS_GUI=1）。本机缺 electron-builder，若 build/ 下
# 没有可用的 win32-x64 payload 就**明确失败**，不产出"装完没有界面"的半残包。
if [ "$WANT_GUI" = 1 ]; then
  if [ -z "$(ls -1 "$BUILD_DIR"/homeagent-gui-win32-x64/*.exe 2>/dev/null | head -1 || true)" ]; then
    echo "[FAIL] 变体 $VARIANT 需要 Windows GUI payload（build/homeagent-gui-win32-x64/*.exe）" >&2
    echo "       本机无 electron-builder：npm i -g electron-builder &&" >&2
    echo "       bash deploy/packaging/build.sh windows/amd64 gui" >&2
    echo "       （只装内核+CLI 的 WSL 场景请用 server 变体）" >&2
    exit 1
  fi
fi

if [ "$WANT_WAITER" = 1 ]; then
  echo "[BUILD] waiter.exe（Windows 侧 CLI；CGO 关闭，跨平台安全）"
  ( cd "$PROJECT_ROOT" && GOOS=windows GOARCH="$ARCH" CGO_ENABLED=0 \
      go build -buildvcs=false -trimpath -o "$BUILD_DIR/waiter.exe" ./cmd/waiter )
fi

# ---- 变体定向 payload：只放本变体那一个 Linux 包 ----
rm -rf "$BUILD_DIR/linux-payload"
mkdir -p "$BUILD_DIR/linux-payload"
cp "$DEB" "$BUILD_DIR/linux-payload/"
echo "[STAGE] payload ← $(basename "$DEB")（$(du -h "$DEB" | cut -f1)）"

if [ -z "$(ls -1 "$BUILD_DIR/linux-payload" 2>/dev/null | head -1 || true)" ]; then
  echo "[FAIL] payload 为空：$BUILD_DIR/linux-payload" >&2
  exit 1
fi
echo "[BUILD] makensis -DVARIANT=$VARIANT -DPRODUCT_VERSION=$VERSION（nsi: $NSI）"
makensis -V2 -DVARIANT="$VARIANT" -DPRODUCT_VERSION="$VERSION" "$NSI"

OUT="$BUILD_DIR/HomeAgent_v${VERSION}_${SUFFIX}_win64.exe"
if [ ! -f "$OUT" ]; then
  echo "[FAIL] 未找到产物 $OUT" >&2
  exit 1
fi
mkdir -p "$DIST_RELEASE"
cp "$OUT" "$DIST_RELEASE/"
echo "[OK] $(basename "$OUT")（$(du -h "$OUT" | cut -f1)）→ $DIST_RELEASE/"
echo "     它会在 Windows 侧引导 WSL2，并把 $(basename "$DEB") 送进去安装（homed 跑在 WSL 里）。"

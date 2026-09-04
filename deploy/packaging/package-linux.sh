#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD_DIR="${PROJECT_ROOT}/build"
DIST_DIR="${PROJECT_ROOT}/dist/linux"
VERSION="${VERSION:-$(git -C "$PROJECT_ROOT" describe --tags --dirty 2>/dev/null || echo "0.8.0")}"
PACKAGE_ROOT="${PROJECT_ROOT}/deploy/packaging/linux"
GO="${GO:-$(command -v go 2>/dev/null || echo "go")}"

ARCH="${1:-amd64}"   # amd64 or arm64
ACTION="${2:-all}"    # all, build, deb, tar, rpm

DEB_ARCH="$ARCH"
RPM_ARCH="$ARCH"
TAR_ARCH="$ARCH"
case "$ARCH" in
  amd64) DEB_ARCH="amd64"; RPM_ARCH="x86_64"; TAR_ARCH="amd64" ;;
  arm64) DEB_ARCH="arm64"; RPM_ARCH="aarch64"; TAR_ARCH="arm64" ;;
  *) echo "Unknown arch: $ARCH (use amd64 or arm64)"; exit 1 ;;
esac

echo "=== HomeAgent Linux Packager ==="
echo "Version: $VERSION"
echo "Arch:    $ARCH"
echo ""

# ---- prepare go.mod for Linux build: point SDK replace at the in-repo SDK ----
# SDK 仓库（含工具链/示例）以嵌套 git 仓库形式维护在 third_party/homeagent-sdk，
# 打包直接用该目录，不再 clone 到 /tmp。
prepare_gomod() {
  local gomod="$PROJECT_ROOT/go.mod"
  local sdk_local="$PROJECT_ROOT/third_party/homeagent-sdk"
  local sdk_clone="/tmp/homeagent-sdk"
  local patched=0

  if grep -q 'replace gitcode.com/JianFeeeee/homeagent-sdk' "$gomod"; then
    echo ">>> Updating go.mod: replacing Windows SDK path with in-repo SDK..."
    if [ -d "$sdk_local" ]; then
      sed -i.bak "s|^replace gitcode.com/JianFeeeee/homeagent-sdk => .*|replace gitcode.com/JianFeeeee/homeagent-sdk => ${sdk_local}|" "$gomod"
      patched=1
    elif [ ! -d "$sdk_clone" ]; then
      echo ">>> Cloning SDK to $sdk_clone (in-repo SDK missing)..."
      git clone git@gitcode.com:JianFeeeee/homeagent-sdk.git "$sdk_clone" 2>/dev/null || \
      git clone https://gitcode.com/JianFeeeee/homeagent-sdk.git "$sdk_clone" 2>/dev/null || true
      if [ -d "$sdk_clone" ]; then
        sed -i.bak "s|^replace gitcode.com/JianFeeeee/homeagent-sdk => .*|replace gitcode.com/JianFeeeee/homeagent-sdk => ${sdk_clone}|" "$gomod"
        patched=1
      else
        echo "WARNING: Cannot clone SDK. Build may fail."
      fi
    else
      sed -i.bak "s|^replace gitcode.com/JianFeeeee/homeagent-sdk => .*|replace gitcode.com/JianFeeeee/homeagent-sdk => ${sdk_clone}|" "$gomod"
      patched=1
    fi
  fi
  return $patched
}

restore_gomod() {
  if [ -f "$PROJECT_ROOT/go.mod.bak" ]; then
    echo ">>> Restoring original go.mod..."
    mv "$PROJECT_ROOT/go.mod.bak" "$PROJECT_ROOT/go.mod"
  fi
}

# ---- temporarily move .syso files (Windows COFF objects) for Linux builds ----
hide_syso() {
  echo ">>> Hiding .syso files (Windows-only object files)..."
  for dir in homed waiter; do
    if [ -f "$PROJECT_ROOT/cmd/$dir/$dir.syso" ]; then
      mv "$PROJECT_ROOT/cmd/$dir/$dir.syso" "$PROJECT_ROOT/cmd/$dir/$dir.syso.bak"
    fi
  done
}

restore_syso() {
  for dir in homed waiter; do
    if [ -f "$PROJECT_ROOT/cmd/$dir/$dir.syso.bak" ]; then
      mv "$PROJECT_ROOT/cmd/$dir/$dir.syso.bak" "$PROJECT_ROOT/cmd/$dir/$dir.syso"
    fi
  done
}

# ensure both are always restored on exit
restore_all() { restore_gomod; restore_syso; }
trap restore_all EXIT

# ---- build Go binaries via existing build.sh ----
build_go() {
  echo ">>> Building Go binaries for linux/$ARCH..."

  prepare_gomod || true
  hide_syso

  bash "$PROJECT_ROOT/deploy/packaging/build.sh" "linux/$ARCH" "homed" 2>&1 || {
    echo "WARNING: homed build failed (CGO/sqlite3 issue). Server/full packages may be incomplete."
  }
  bash "$PROJECT_ROOT/deploy/packaging/build.sh" "linux/$ARCH" "waiter" 2>&1 || {
    echo "WARNING: waiter build failed."
  }
  bash "$PROJECT_ROOT/deploy/packaging/build.sh" "linux/$ARCH" "initconfig" 2>&1 || {
    echo "WARNING: initconfig build failed（包内将缺少首次配置初始化器）。"
  }

  local suffix="linux_${ARCH}"
  local homed_bin="$BUILD_DIR/homed_$suffix"
  local waiter_bin="$BUILD_DIR/waiter_$suffix"

  if [ ! -f "$homed_bin" ]; then
    echo "ERROR: homed binary not found at $homed_bin"
    exit 1
  fi
  if [ ! -f "$waiter_bin" ]; then
    echo "ERROR: waiter binary not found at $waiter_bin"
    exit 1
  fi

  echo "  homed:  $homed_bin ($(du -h "$homed_bin" | cut -f1))"
  echo "  waiter: $waiter_bin ($(du -h "$waiter_bin" | cut -f1))"
  echo ""
}

# ---- build GUI (manual directory assembly, avoids electron-packager network issues) ----
#
# electron 运行时必须按**目标架构**取，不能用 host 的
# node_modules/electron/dist——那里永远是 host 架构（本机 x64）。
# v1.0.0 / v1.0.1 的 arm64 full/client 包都踩了这个坑：目录名带
# -arm64、homed/waiter 确实是 aarch64，但里面的 electron 是 x86-64，
# 在 arm64 机器上一启动就是 Exec format error（从未被交叉验证过）。
#
# 现在改为优先从 electron 缓存里取对应架构的 zip，并在最后做
# 一道强制校验：架构不符就删掉目录并跳过 GUI，宁可不发也不发坏包。
build_gui() {
  local gui_dir="$PROJECT_ROOT/cmd/gui"
  local gui_out="$BUILD_DIR/homeagent-gui-linux-${TAR_ARCH}"

  if [ -d "$gui_out" ]; then
    echo ">>> GUI already built at $gui_out (delete to rebuild)"
    return
  fi

  echo ">>> Building GUI directory for linux/$ARCH..."

  if [ ! -d "$gui_dir/node_modules" ]; then
    echo "  npm install..."
    (cd "$gui_dir" && npm install --production)
  fi

  # electron 版本从已安装的包里读，保证运行时与 app 依赖一致
  local ever
  ever=$(python3 -c "import json;print(json.load(open('$gui_dir/node_modules/electron/package.json'))['version'])" 2>/dev/null || true)

  mkdir -p "$gui_out"

  # 优先：缓存里的目标架构 zip（~/.cache/electron/<hash>/electron-v<ver>-linux-<arch>.zip）
  local zip=""
  if [ -n "$ever" ]; then
    zip=$(find "$HOME/.cache/electron" -name "electron-v${ever}-linux-${TAR_ARCH}.zip" 2>/dev/null | head -1)
  fi
  if [ -z "$zip" ]; then
    zip=$(find "$HOME/.cache/electron" -name "electron-v*-linux-${TAR_ARCH}.zip" 2>/dev/null | head -1)
  fi

  if [ -n "$zip" ]; then
    echo "  electron runtime: $(basename "$zip")"
    unzip -q -o "$zip" -d "$gui_out"
  else
    # 回退：仅当目标架构 == host 架构时才能用 host 的 dist
    local host_arch
    case "$(uname -m)" in
      x86_64) host_arch=amd64 ;;
      aarch64|arm64) host_arch=arm64 ;;
      *) host_arch=unknown ;;
    esac
    if [ "$TAR_ARCH" != "$host_arch" ]; then
      echo "  WARNING: 缺 electron-v*-linux-${TAR_ARCH}.zip 缓存，且目标架构与 host"
      echo "           ($host_arch) 不同——不能用 host 的 electron 冒充。跳过 GUI。"
      echo "           解法：下载 electron-v${ever:-<ver>}-linux-${TAR_ARCH}.zip 到"
      echo "           ~/.cache/electron/<任意子目录>/ 后重跑。"
      rm -rf "$gui_out"
      return
    fi
    local electron_dir="$gui_dir/node_modules/electron/dist"
    if [ ! -f "$electron_dir/electron" ]; then
      echo "  WARNING: electron binary not found at $electron_dir. GUI will be skipped."
      rm -rf "$gui_out"
      return
    fi
    echo "  electron runtime: host node_modules (同架构 $host_arch)"
    cp -r "$electron_dir"/* "$gui_out/" 2>/dev/null
  fi

  mkdir -p "$gui_out/resources/app/node_modules"
  mkdir -p "$gui_out/resources/app/renderer"
  rm -f "$gui_out/resources/default_app.asar" 2>/dev/null

  # copy app source
  cp "$gui_dir/main.js" "$gui_out/resources/app/"
  cp "$gui_dir/preload.js" "$gui_out/resources/app/"
  cp "$gui_dir/package.json" "$gui_out/resources/app/"
  cp "$gui_dir/renderer/index.html" "$gui_out/resources/app/renderer/"
  cp "$gui_dir/renderer/app.js" "$gui_out/resources/app/renderer/"
  cp "$gui_dir/renderer/style.css" "$gui_out/resources/app/renderer/" 2>/dev/null || true
  cp "$gui_dir/renderer/mascot.svg" "$gui_out/resources/app/renderer/" 2>/dev/null || true

  # production node_modules for app
  if [ -d "$gui_dir/node_modules" ]; then
    for mod in icojs; do
      if [ -d "$gui_dir/node_modules/$mod" ]; then
        cp -r "$gui_dir/node_modules/$mod" "$gui_out/resources/app/node_modules/"
      fi
    done
  fi

  # create desktop entry and symlink
  cat > "$gui_out/resources/app/homeagent-gui.desktop" << DESKTOP
[Desktop Entry]
Name=HomeAgent
Comment=HomeAgent Desktop GUI
Exec=$gui_out/homeagent-gui
Terminal=false
Type=Application
Categories=Utility;
Icon=$gui_out/resources/app/icon.svg
DESKTOP

  # create launcher script
  cat > "$gui_out/homeagent-gui" << 'LAUNCHER'
#!/bin/sh
DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$DIR/electron" "$DIR/resources/app" "$@"
LAUNCHER
  chmod +x "$gui_out/homeagent-gui"
  chmod +x "$gui_out/electron"

  # 最后一道强制校验：electron 二进制的实际架构必须匹配目标架构。
  # 不做这步就会重现 v1.0.0/v1.0.1 的隐形坏包：包名、目录名、
  # homed/waiter 全对，只有 electron 是错架构，直到用户在 arm64 机器上
  # 双击才发现 Exec format error。
  local want_pat
  case "$TAR_ARCH" in
    amd64) want_pat="x86-64" ;;
    arm64) want_pat="aarch64" ;;
    *)     want_pat="" ;;
  esac
  if [ -n "$want_pat" ]; then
    local got
    got=$(file -b "$gui_out/electron" 2>/dev/null || echo "")
    if ! printf '%s' "$got" | grep -q "$want_pat"; then
      echo "  ERROR: electron 架构不符——期望 $want_pat，实际: ${got%%,*}"
      echo "         删除 GUI 目录并跳过（宁可不发，也不发装了跑不起来的包）。"
      rm -rf "$gui_out"
      return
    fi
  fi

  echo "  GUI built: $gui_out ($(du -sh "$gui_out" | cut -f1), $(file -b "$gui_out/electron" | cut -d, -f2 | tr -d ' '))"
  echo ""
}

# ---- stage files for a variant ----
stage_variant() {
  local variant="$1"
  local staging="$2"
  local suffix="linux_${ARCH}"

  echo ">>> Staging $variant..."

  mkdir -p "$staging/usr/bin"
  mkdir -p "$staging/etc/systemd/system"
  mkdir -p "$staging/var/lib/homeagent"

  local initconfig_bin="$BUILD_DIR/initconfig_$suffix"

  case "$variant" in
    full)
      cp "$BUILD_DIR/homed_$suffix" "$staging/usr/bin/homed"
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      cp "$PROJECT_ROOT/deploy/homeagent.service" "$staging/etc/systemd/system/homeagent.service"
      [ -f "$initconfig_bin" ] && cp "$initconfig_bin" "$staging/usr/bin/initconfig"
      stage_setup "$staging"
      stage_gui "$staging"
      ;;
    server)
      cp "$BUILD_DIR/homed_$suffix" "$staging/usr/bin/homed"
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      cp "$PROJECT_ROOT/deploy/homeagent.service" "$staging/etc/systemd/system/homeagent.service"
      [ -f "$initconfig_bin" ] && cp "$initconfig_bin" "$staging/usr/bin/initconfig"
      stage_setup "$staging"
      ;;
    client)
      cp "$BUILD_DIR/waiter_$suffix" "$staging/usr/bin/waiter"
      stage_gui "$staging"
      ;;
  esac

  chmod 755 "$staging/usr/bin/"* 2>/dev/null || true
  echo "  OK"
}

stage_gui() {
  local staging="$1"
  local gui_src="$BUILD_DIR/homeagent-gui-linux-${TAR_ARCH}"
  if [ -d "$gui_src" ]; then
    mkdir -p "$staging/usr/lib/homeagent-gui"
    cp -r "$gui_src"/* "$staging/usr/lib/homeagent-gui/"
    cat > "$staging/usr/bin/homeagent-gui" << 'SCRIPT'
#!/bin/sh
exec /usr/lib/homeagent-gui/homeagent-gui "$@"
SCRIPT
    chmod 755 "$staging/usr/bin/homeagent-gui"
  else
    echo "  WARNING: GUI not built, skipping GUI staging"
  fi
}

stage_setup() {
  local staging="$1"
  local setup_src="$PROJECT_ROOT/deploy/packaging/linux/setup.sh"
  if [ -f "$setup_src" ]; then
    mkdir -p "$staging/usr/lib/homeagent"
    cp "$setup_src" "$staging/usr/lib/homeagent/setup.sh"
    chmod 755 "$staging/usr/lib/homeagent/setup.sh"
  fi
}

# ---- create .deb ----
build_deb() {
  local variant="$1"
  local staging="$2"
  local deb_dir="${DIST_DIR}/deb"
  mkdir -p "$deb_dir"

  local pkg_name="homeagent-${variant}_${VERSION}_${DEB_ARCH}.deb"
  local deb_root
  deb_root="$(mktemp -d)"

  mkdir -p "$deb_root/DEBIAN"

  local control_file="$PACKAGE_ROOT/deb/control-${variant}"
  local installed_size_kb
  installed_size_kb=$(du -sk "$staging" | cut -f1)

  sed -e "s/VERSION_PLACEHOLDER/$VERSION/g" \
      -e "s/ARCH_PLACEHOLDER/$DEB_ARCH/g" \
      -e "s/INSTALLED_SIZE_PLACEHOLDER/$installed_size_kb/g" \
      "$control_file" > "$deb_root/DEBIAN/control"

  if [ -f "$PACKAGE_ROOT/deb/postinst" ]; then
    cp "$PACKAGE_ROOT/deb/postinst" "$deb_root/DEBIAN/postinst"
    chmod 755 "$deb_root/DEBIAN/postinst"
  fi

  if [ -f "$PACKAGE_ROOT/deb/prerm" ]; then
    cp "$PACKAGE_ROOT/deb/prerm" "$deb_root/DEBIAN/prerm"
    chmod 755 "$deb_root/DEBIAN/prerm"
  fi

  cp -r "$staging"/* "$deb_root/" 2>/dev/null || true

  echo ">>> Building .deb: $pkg_name"
  fakeroot dpkg-deb --build "$deb_root" "$deb_dir/$pkg_name" 2>/dev/null || \
    dpkg-deb --build "$deb_root" "$deb_dir/$pkg_name" 2>&1
  rm -rf "$deb_root"
  echo "  Created: $deb_dir/$pkg_name ($(du -h "$deb_dir/$pkg_name" | cut -f1))"
}

# ---- create combined .tar.gz (all binaries, no variant split) ----
build_tar() {
  local tar_dir="${DIST_DIR}/tar"
  mkdir -p "$tar_dir"

  local archive_name="homeagent_${VERSION}_linux_${TAR_ARCH}.tar.gz"
  local archive_dir="homeagent-${VERSION}-linux-${TAR_ARCH}"

  # build combined staging
  local staging
  staging="$(mktemp -d)"
  mkdir -p "$staging/usr/bin" "$staging/usr/lib/homeagent"

  # copy all available binaries
  for bin in homed waiter initconfig; do
    local src="$BUILD_DIR/${bin}_linux_${TAR_ARCH}"
    [ -f "$src" ] && cp "$src" "$staging/usr/bin/$bin"
  done

  # setup script
  local setup_src="$PROJECT_ROOT/deploy/packaging/linux/setup.sh"
  [ -f "$setup_src" ] && cp "$setup_src" "$staging/usr/lib/homeagent/setup.sh"

  # GUI if available
  local gui_src="$BUILD_DIR/homeagent-gui-linux-${TAR_ARCH}"
  if [ -d "$gui_src" ]; then
    mkdir -p "$staging/usr/lib/homeagent-gui"
    cp -r "$gui_src"/* "$staging/usr/lib/homeagent-gui/"
    cat > "$staging/usr/bin/homeagent-gui" << 'SCRIPT'
#!/bin/sh
exec /usr/lib/homeagent-gui/homeagent-gui "$@"
SCRIPT
    chmod 755 "$staging/usr/bin/homeagent-gui"
  fi

  chmod 755 "$staging/usr/bin/"* 2>/dev/null || true

  echo ">>> Building .tar.gz: $archive_name"
  (cd "$staging" && tar czf "$tar_dir/$archive_name" --transform "s|^\.|${archive_dir}|" .)
  echo "  Created: $tar_dir/$archive_name ($(du -h "$tar_dir/$archive_name" | cut -f1))"
  rm -rf "$staging"
}

# ---- create .rpm (via fpm if available) ----
build_rpm() {
  local variant="$1"
  local staging="$2"
  local rpm_dir="${DIST_DIR}/rpm"
  mkdir -p "$rpm_dir"

  local pkg_name="homeagent-${variant}-${VERSION}-1.${RPM_ARCH}.rpm"

  # find fpm
  local fpm_bin="$(command -v fpm 2>/dev/null || true)"
  if [ -z "$fpm_bin" ]; then
    fpm_bin="$(find /home -name "fpm" -type f -path "*/bin/*" 2>/dev/null | head -1 || true)"
  fi

  if [ -z "$fpm_bin" ]; then
    echo "  SKIP .rpm: fpm not installed. Install it with: gem install fpm"
    echo "  Alternatively, build RPM on Fedora/RHEL using:"
    echo "    rpmbuild -ba deploy/packaging/linux/homeagent.spec"
    return
  fi

  # find or extract rpmbuild (fpm needs it)
  local rpmbuild_dir="/tmp/rpmext"
  if [ ! -f "$rpmbuild_dir/usr/bin/rpmbuild" ]; then
    # try to extract from cached deb packages
    local rpm_deb
    rpm_deb="$(find /tmp -name "rpm_*.deb" -type f 2>/dev/null | head -1)"
    if [ -z "$rpm_deb" ]; then
      rpm_deb="$(find "$PROJECT_ROOT" -name "rpm_*.deb" -type f 2>/dev/null | head -1)"
    fi
    if [ -n "$rpm_deb" ]; then
      mkdir -p "$rpmbuild_dir"
      (cd "$rpmbuild_dir" && ar x "$rpm_deb" 2>/dev/null && \
        tar --no-same-permissions -xf data.tar.zst --zstd 2>/dev/null) || true
      for libdeb in /tmp/librpm*.deb; do
        [ -f "$libdeb" ] && (cd "$rpmbuild_dir" && ar x "$libdeb" 2>/dev/null && \
          tar --no-same-permissions -xf data.tar.zst --zstd 2>/dev/null) || true
      done
    fi
  fi

  if [ -f "$rpmbuild_dir/usr/bin/rpmbuild" ]; then
    export PATH="$rpmbuild_dir/usr/bin:$PATH"
    export LD_LIBRARY_PATH="$rpmbuild_dir/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    export RPM_CONFIGDIR="$rpmbuild_dir/usr/lib/rpm"
  fi

  echo ">>> Building .rpm via fpm: $pkg_name"
  "$fpm_bin" -s dir -t rpm \
    -n "homeagent-${variant}" \
    -v "$VERSION" \
    --iteration 1 \
    -a "$RPM_ARCH" \
    --description "HomeAgent ${variant^} package" \
    --url "https://github.com/trueagent/HomeAgent" \
    --license "Proprietary" \
    -C "$staging" \
    -p "$rpm_dir/$pkg_name" \
    . 2>&1
  echo "  Created: $rpm_dir/$pkg_name"
}

# ---- main ----
main() {
  local target_arch="$ARCH"

  mkdir -p "$BUILD_DIR"

  case "$ACTION" in
    all|build)
      build_go
      build_gui
      ;;
  esac

  if [ "$ACTION" = "build" ]; then
    echo ""
    echo "=== Build complete. Binaries in $BUILD_DIR ==="
    exit 0
  fi

  mkdir -p "$DIST_DIR"

  for variant in full server client; do
    echo ""
    echo "=============================================="
    echo "  Packaging: $variant"
    echo "=============================================="

    local staging
    staging=$(mktemp -d)
    stage_variant "$variant" "$staging"

    case "$ACTION" in
      all|deb) build_deb "$variant" "$staging" ;;
    esac

    case "$ACTION" in
      all|rpm) build_rpm "$variant" "$staging" ;;
    esac

    rm -rf "$staging"
  done

  case "$ACTION" in
    all|tar) build_tar ;;
  esac

  echo ""
  echo "=== Done! Packages in: $DIST_DIR ==="
  echo ""
  echo "Summary:"
  find "$DIST_DIR" -type f \( -name "*.deb" -o -name "homeagent_*.tar.gz" -o -name "*.rpm" \) 2>/dev/null | sort | while read -r f; do
    echo "  $(du -h "$f" | cut -f1)  $f"
  done
}

main

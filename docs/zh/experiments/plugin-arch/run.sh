#!/usr/bin/env bash
# 插件架构评估实验 —— 一键复跑
# 用法: ./run.sh [实验编号...]   例: ./run.sh 12 13    留空跑全部
# 依赖: go >= 1.21, gcc, Linux (eventfd/memfd/dlopen)
set -uo pipefail
cd "$(dirname "$0")"
ROOT=$(pwd)
PASS=0; FAIL=0

need() { command -v "$1" >/dev/null || { echo "缺少依赖: $1"; exit 1; }; }
need go; need gcc

# 统一的临时 module 环境（避免污染主仓 go.mod）
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

banner() { echo; echo "════════ $* ════════"; }

# x/sys 只有 exp1/2/4/8/10 需要
prep_xsys() {
  cat > "$1/go.mod" <<EOF
module exp
go 1.21
require golang.org/x/sys v0.20.0
EOF
  (cd "$1" && GOFLAGS=-mod=mod go get golang.org/x/sys@v0.20.0 >/dev/null 2>&1)
}
prep_plain() { printf 'module exp\ngo 1.21\n' > "$1/go.mod"; }

run_go() { # <目录> <说明>
  if (cd "$1" && go run . 2>&1); then PASS=$((PASS+1)); else echo "  ❌ 失败: $2"; FAIL=$((FAIL+1)); fi
}

SEL="${*:-all}"
sel() { [ "$SEL" = "all" ] && return 0; case " $SEL " in *" $1 "*) return 0;; esac; return 1; }

# ── 01: dlclose / NODELETE ────────────────────────────────
if sel 1; then
  banner "实验 1 组: dlclose 对 DF_1_NODELETE 是 no-op"
  W=$WORK/e01; mkdir -p $W; cp 01-dlclose-nodelete/*.c $W/
  gcc -shared -fPIC -o $W/probe_v1.so $W/probe_v1.c
  gcc -shared -fPIC -o $W/probe_v2.so $W/probe_v2.c
  gcc -shared -fPIC -o $W/shim.so     $W/shim.c
  cp $W/probe_v1.so $W/probe.so
  for e in exp01a exp01b; do
    mkdir -p $W/$e; cp 01-dlclose-nodelete/$e/main.go $W/$e/
    sed -i '/^\/\/go:build ignore$/d' $W/$e/main.go; prep_plain $W/$e
    (cd $W/$e && go build -o ../$e.bin . 2>&1 | head -3)
  done
  echo "--- 01a: Go 宿主经 C shim 加载/卸载纯 C so ---"
  (cd $W && ./exp01a.bin) && PASS=$((PASS+1)) || FAIL=$((FAIL+1))
  echo "--- 01b: /proc/self/maps 段数验证（纯 C 归零，Go c-shared 不归零）---"
  (cd $W && ./exp01b.bin) && PASS=$((PASS+1)) || FAIL=$((FAIL+1))
fi

# ── 01c: 版本化路径（需要两个真 Go c-shared）────────────────
if sel 1c; then
  banner "实验 1c: 版本化路径 dlopen 可加载新代码"
  W=$WORK/e01c; mkdir -p $W/{v1,v2,host}
  for V in v1 v2; do
    cat > $W/$V/main.go <<EOF
package main
import "C"
//export lib_version
func lib_version() *C.char { return C.CString("$V-CODE") }
func main() {}
EOF
    printf 'module gl%s\ngo 1.21\n' $V > $W/$V/go.mod
    (cd $W/$V && go build -buildmode=c-shared -o ../gl$V.so . 2>&1|head -3)
  done
  cp 01-dlclose-nodelete/exp01c/main.go $W/host/
  sed -i '/^\/\/go:build ignore$/d' $W/host/main.go; prep_plain $W/host
  (cd $W/host && go build -o ../h.bin .) && (cd $W && ./h.bin) && PASS=$((PASS+1)) || FAIL=$((FAIL+1))
fi

# ── 02: 可行性 1-11 ───────────────────────────────────────
declare -A XSYS=([1]=1 [2]=1 [4]=1 [8]=1 [10]=1)
for n in 1 2 3 4 5 6 7 8 9 10 11; do
  sel $n || continue
  banner "实验 $n"
  W=$WORK/f$n; mkdir -p $W
  case $n in
    1)  cp 02-feasibility/exp1_eventfd.go $W/main.go ;;
    2)  cp 02-feasibility/exp2_parent.go $W/main.go; cp 02-feasibility/exp2_child.go $W/ ;;
    3)  cp 02-feasibility/exp3_parent.go $W/main.go; cp 02-feasibility/exp3_child.go $W/ ;;
    4)  cp 02-feasibility/exp4.go $W/main.go ;;
    5)  cp 02-feasibility/exp5b.go $W/main.go; cp 02-feasibility/exp5_plugin.go $W/ ;;
    6)  cp 02-feasibility/exp6.go $W/main.go; cp 02-feasibility/exp6_crash.go $W/ ;;
    7)  cp 02-feasibility/exp7.go $W/main.go ;;
    8)  cp 02-feasibility/exp8.go $W/main.go; cp 02-feasibility/exp8_worker.go $W/ ;;
    9)  cp 02-feasibility/exp9.go $W/main.go; cp 02-feasibility/exp9_worker.go $W/ ;;
    10) cp 02-feasibility/exp10.go $W/main.go ;;
    11) cp 02-feasibility/exp11.go $W/main.go; cp 02-feasibility/exp11_plug.go $W/ ;;
  esac
  # 去掉 main.go 的 build ignore（它是入口）
  sed -i '/^\/\/go:build ignore$/d' $W/main.go
  if [ "${XSYS[$n]:-}" = "1" ]; then prep_xsys $W; else prep_plain $W; fi
  # 需要预编译的辅助二进制
  case $n in
    5)  (cd $W && go build -o plugbin exp5_plugin.go 2>&1|head -3) ;;
    6)  (cd $W && go build -o crashbin exp6_crash.go 2>&1|head -3) ;;
    11) (cd $W && go build -o plug11 exp11_plug.go 2>&1|head -3) ;;
  esac
  run_go $W "实验 $n"
done

# ── 03: lost update ───────────────────────────────────────
for e in 12 13; do
  sel $e || continue
  banner "实验 $e: 副本模型 lost update"
  W=$WORK/l$e; mkdir -p $W
  cp 03-lost-update/exp$e/main.go $W/; sed -i '/^\/\/go:build ignore$/d' $W/main.go
  prep_plain $W; run_go $W "实验 $e"
done

# ── 04: cgo 不可中断 ──────────────────────────────────────
for e in 14a 14b; do
  sel 14 || sel $e || continue
  banner "实验 $e: cgo 调用不可中断"
  W=$WORK/c$e; mkdir -p $W
  cp 04-cgo-uninterruptible/hang.c $W/
  gcc -shared -fPIC -o $W/hang.so $W/hang.c
  cp 04-cgo-uninterruptible/exp$e/main.go $W/; sed -i '/^\/\/go:build ignore$/d' $W/main.go
  prep_plain $W; run_go $W "实验 $e"
done

banner "汇总: 通过 $PASS, 失败 $FAIL"
[ $FAIL -eq 0 ]

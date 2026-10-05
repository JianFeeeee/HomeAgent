package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInitMemoryStackKeepsDistillerRunning 锁死启动接线回归：
// initMemoryStack 必须返回一个**仍在运行**的蒸馏器。
//
// 历史 bug：main() 拆分时函数体内残留一句 `defer distiller.Stop()`，
// 函数一 return 就 cancel 掉刚启动的循环，规则蒸馏 10min 心跳从不运行。
// 该缺陷不会让任何单测变红——pipeline 的 TestDistillOnce* 直接调
// distillOnce，绕过了 Start/Stop 接线；只有在这里按「启动阶段函数」的
// 真实调用方式断言，才照得出来。
func TestInitMemoryStackKeepsDistillerRunning(t *testing.T) {
	dir := t.TempDir()
	// NewGraphDB 需要父目录已存在（生产由 dataDir 初始化保证）。
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0755); err != nil {
		t.Fatal(err)
	}
	st, cleanup := initMemoryStack(dir, nil)
	if st == nil || st.distiller == nil {
		cleanup()
		t.Fatal("initMemoryStack 未返回蒸馏器")
	}
	if st.db == nil {
		cleanup()
		t.Skip("图库未初始化，无法验证蒸馏接线")
	}
	if st.distiller.Stopped() {
		cleanup()
		t.Fatal("initMemoryStack 返回后蒸馏循环已被停掉（defer Stop 残留？）")
	}

	// cleanup 是唯一的停机点：先停蒸馏器、再关图库。
	cleanup()
	if !st.distiller.Stopped() {
		t.Fatal("cleanup 之后蒸馏器应已停止")
	}
}

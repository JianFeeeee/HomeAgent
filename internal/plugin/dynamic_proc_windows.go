//go:build windows

package plugin

import (
	"fmt"
	"os"
	"path/filepath"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// tryLoadProc 的 Windows 桩：子进程通道本身是跨平台的（stdio JSON-RPC 无平台差异），
// 但共享内存数据面当前基于 POSIX mmap，Windows 需改用 CreateFileMapping。
//
// 迁移评估 §9.2 已记录：Windows DLL 路径当前能力严重退化（只下发 3 字段、无写回），
// 迁移到子进程后三套 ABI 收敛为单一 RPC 实现，Windows 反而受益——但需要测试机验证。
func tryLoadProc(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	for _, candidate := range []string{binEntry, "plugin.exe"} {
		if st, err := os.Stat(filepath.Join(dir, candidate)); err == nil && !st.IsDir() {
			return nil, fmt.Errorf("proc plugin %s: Windows 子进程通道尚未实现（Part 2 + §9.2）", name)
		}
	}
	return nil, nil
}

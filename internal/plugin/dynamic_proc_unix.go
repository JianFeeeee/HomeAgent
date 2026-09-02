//go:build linux || darwin

package plugin

import (
	"fmt"
	"os"
	"path/filepath"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// tryLoadProc 加载子进程插件（plugin.bin）——外部插件多进程化的加载入口。
//
// 设计依据：docs/zh/架构迁移评估.md §3（stdio JSON-RPC 控制面 + shm 数据面 + eventfd 通知面）
// 实施计划：docs/zh/plugin-migration-plan.md Part 2
//
// 当前状态：**分派桩位**。共享内存数据面与锁仲裁已在 internal/plugin/proc/ 落地
// 并通过 16 项测试（含 -race），进程管理与 RPC 编解码为 Part 2 内容。
//
// 返回 nil,nil 表示目录中没有 plugin.bin（交由后续探测通道）。
// 找到二进制但通道未就绪时返回明确错误——不静默回退到 cabi，
// 否则"已迁移插件跑回旧通道"极难排查。
func tryLoadProc(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	binPath := filepath.Join(dir, binEntry)
	st, err := os.Stat(binPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("proc plugin %s: 检查 %s: %w", name, binEntry, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("proc plugin %s: %s 是目录，不是可执行文件", name, binEntry)
	}
	if st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("proc plugin %s: %s 缺少可执行权限（chmod +x）", name, binEntry)
	}

	return nil, fmt.Errorf("proc plugin %s: 子进程通道尚未实现（Part 2）——"+
		"共享内存数据面已就绪（internal/plugin/proc），"+
		"如需运行请把 plugin.json 的 entry 改回 %s 走 C ABI 通道", name, soEntry)
}

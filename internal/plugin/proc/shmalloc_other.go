//go:build !linux && !darwin && !windows

package proc

import (
	"fmt"
	"os"
)

// allocShm 在尚未适配的平台上明确报错。
//
// 不静默降级成「无共享段」：那会让 stage 静默失去数据面，
// 插件看起来加载成功但读不到 StageContext——比启动失败难查得多。
//
// Windows 适配路径：CreateFileMapping + MapViewOfFile，句柄经
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST 或命名段传给子进程。
// §9.2 已记录 Windows DLL 路径当前能力严重退化（只下发 3 字段、无写回），
// 迁移到子进程后三套 ABI 收敛为单一 RPC 实现，Windows 反而受益，但需测试机验证。
func allocShm(size int) (*os.File, []byte, error) {
	return nil, nil, fmt.Errorf("proc: 当前平台尚未支持共享内存数据面（需 CreateFileMapping 适配，§9.2）")
}

func freeShm(f *os.File, data []byte) error {
	if f != nil {
		return f.Close()
	}
	return nil
}

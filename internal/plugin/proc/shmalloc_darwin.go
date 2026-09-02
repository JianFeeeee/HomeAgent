//go:build darwin

package proc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// allocShm 用临时文件 + mmap 创建共享段（macOS）。
//
// macOS 没有 memfd_create。改用 os.CreateTemp 后立即 unlink：文件名从目录树消失，
// 但 fd 与映射继续有效，进程退出即回收——与 memfd 的不残留语义一致。
// 已 unlink 的 fd 仍可经 ExtraFiles 传给子进程，子进程 mmap 同一 inode，
// 故「全部插件共享一块段」的前提在 macOS 同样成立。
func allocShm(size int) (*os.File, []byte, error) {
	f, err := os.CreateTemp("", "hastagectx-*")
	if err != nil {
		return nil, nil, fmt.Errorf("proc: 创建共享段临时文件: %w", err)
	}
	// 立即摘除目录项：后续无人能按路径打开它，也不会有残留文件
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("proc: unlink 共享段临时文件: %w", err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("proc: 共享段 ftruncate: %w", err)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, size,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("proc: 共享段 mmap: %w", err)
	}
	return f, data, nil
}

// freeShm 解除映射并关闭段。
func freeShm(f *os.File, data []byte) error {
	if data != nil {
		unix.Munmap(data)
	}
	if f != nil {
		return f.Close()
	}
	return nil
}

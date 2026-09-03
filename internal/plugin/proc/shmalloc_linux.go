//go:build linux

package proc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// allocShm 用 memfd 创建共享段（Linux）。
//
// 选 memfd 而非 /dev/shm 文件：无需文件名、不残留（最后一个 fd 关闭即回收）、
// 可经 ExtraFiles 传给子进程。实验 2 已验证父子 mmap 到不同虚拟地址时
// 相对偏移仍正确解引用——这是段内一律用偏移而非指针的前提。
func allocShm(size int) (*os.File, []byte, error) {
	fd, err := unix.MemfdCreate("hastagectx", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, nil, fmt.Errorf("proc: 创建共享段 memfd: %w", err)
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("proc: 共享段 ftruncate: %w", err)
	}
	data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("proc: 共享段 mmap: %w", err)
	}
	return os.NewFile(uintptr(fd), "hastagectx"), data, nil
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

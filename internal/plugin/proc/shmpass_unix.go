//go:build linux || darwin || freebsd

package proc

import "os"

// procEnvForShm 在 Unix 返回 nil：段经继承 fd 传递，无需环境变量。
func (h *Host) procEnvForShm() []string { return nil }

// procExtraFilesForShm 返回经 ExtraFiles 传给子进程的 fd 列表。
//
// 顺序即 fd 编号（cmd.ExtraFiles[0] → 子进程 fd 3）：
//
//	fd 3 = 统一共享内存区域（SuperBlock + StageContext + EvtRing）
//	fd 4 = 事件通知（eventfd / pipe 读端）
//
// 插件侧模板 z_proc_shm_unix.go 的常量与此严格对应。
func (h *Host) procExtraFilesForShm() []*os.File {
	return []*os.File{h.memfd, h.evtfd}
}

//go:build windows

package proc

import (
	"fmt"
	"os"
)

// procEnvForShm 返回子进程挂载共享段所需的环境变量（Windows）。
//
// Windows 没有 fd 继承语义（os/exec 的 ExtraFiles 在 Windows 不被支持），
// 故段与事件对象的**名字**经环境变量传给子进程，插件侧模板的
// z_proc_shm_windows.go 按同名 OpenFileMappingW / OpenEventW 打开。
//
// 名字带 PID 与递增序号：多个 homed 实例并存时不能撞名。
func (h *Host) procEnvForShm() []string {
	return []string{
		fmt.Sprintf("HOMEAGENT_SHM_STAGE=%s", shmNameOf(h.data)),
		fmt.Sprintf("HOMEAGENT_SHM_EVTRING=%s", shmNameOf(h.evtData)),
		fmt.Sprintf("HOMEAGENT_EVT_EVENT=%s", evtEventNameOf(h.evtNotifyFd)),
	}
}

// procExtraFilesForShm 在 Windows 返回 nil：段不经 fd 传递。
func (h *Host) procExtraFilesForShm() []*os.File { return nil }

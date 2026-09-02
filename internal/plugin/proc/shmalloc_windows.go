//go:build windows

package proc

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 侧共享段：命名 FileMapping + 命名 Event。
//
// 与 Unix 的机制差异（不是能力差异）：
// Windows 没有 fd 继承语义——os/exec 的 ExtraFiles 在 Windows 实现里不被支持。
// 等价机制是命名内核对象：父进程 CreateFileMappingW 建带名字的段，
// 子进程 OpenFileMappingW 按同名打开，拿到同一份物理页。
//
// **这是 §9.2 的正解**。C ABI 时代 Windows 是第三套独立 ABI 实现
// （dynamic_dll_windows.go），stage 只下发 3 字段且完全没有写回，
// sanitizer 这类改写型插件静默失效。三套 ABI 收敛为单一 RPC 后，
// Windows 与 Unix 共用同一份 stage 逻辑与同一份共享段布局，
// 平台差异只剩本文件的创建端 + 插件侧模板的打开端。
//
// 名字带 PID 与递增序号：多个 homed 实例并存时不能撞名，
// 同一实例内 StageContext 段与事件环段也必须分开。
var shmNameSeq atomic.Uint64

const (
	shmNamePrefix   = "Local\\HomeAgentShm"
	evtRingNamePfx  = "Local\\HomeAgentEvtRing"
	evtEventNamePfx = "Local\\HomeAgentEvtSignal"
	envStageShmName = "HOMEAGENT_SHM_STAGE"
	envEvtRingName  = "HOMEAGENT_SHM_EVTRING"
	envEvtEventName = "HOMEAGENT_EVT_EVENT"
)

// namedShm 持有一块命名共享段。
//
// 不用 *os.File 承载：Windows 的 FileMapping 句柄不是文件句柄，
// 包进 os.File 后 Close 语义不对（会尝试当文件关）。故用独立类型，
// 由 shmHandles 表按 mmap 地址反查——freeShm 只拿到 (*os.File, []byte)。
type namedShm struct {
	name    string
	mapping windows.Handle
	addr    uintptr
	size    int
}

// shmHandles 记录已分配的段，供 freeShm 按数据指针反查句柄。
//
// 为何需要这张表：allocShm 的跨平台签名返回 (*os.File, []byte)，
// Windows 没有对应的 fd，只能把句柄存在旁路。key 用切片首地址。
var (
	shmHandles   = map[uintptr]*namedShm{}
	shmHandlesMu sync.Mutex
)

// allocShm 创建命名共享段并映射。
//
// 返回的 *os.File 为 nil：Windows 不经 fd 传递段，插件按名字打开。
// 名字通过 procEnvForShm 注入子进程环境变量。
func allocShm(size int) (*os.File, []byte, error) {
	name := fmt.Sprintf("%s_%d_%d", shmNamePrefix, os.Getpid(), shmNameSeq.Add(1))
	shm, data, err := createNamedMapping(name, size)
	if err != nil {
		return nil, nil, err
	}
	shmHandlesMu.Lock()
	shmHandles[uintptr(unsafe.Pointer(&data[0]))] = shm
	shmHandlesMu.Unlock()
	return nil, data, nil
}

// createNamedMapping 建命名段并映射为 []byte。
func createNamedMapping(name string, size int) (*namedShm, []byte, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, fmt.Errorf("proc: 共享段名字非法 %q: %w", name, err)
	}

	// INVALID_HANDLE_VALUE + 命名 → 由系统页文件支撑的匿名段（不落盘）
	mapping, err := windows.CreateFileMapping(
		windows.InvalidHandle, nil, windows.PAGE_READWRITE,
		uint32(uint64(size)>>32), uint32(size), namePtr)
	if err != nil {
		return nil, nil, fmt.Errorf("proc: 创建命名共享段 %q: %w", name, err)
	}

	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(mapping)
		return nil, nil, fmt.Errorf("proc: 映射共享段 %q: %w", name, err)
	}

	return &namedShm{name: name, mapping: mapping, addr: addr, size: size},
		unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

// freeShm 解除映射并关闭段句柄。
func freeShm(f *os.File, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	key := uintptr(unsafe.Pointer(&data[0]))
	shmHandlesMu.Lock()
	shm, ok := shmHandles[key]
	delete(shmHandles, key)
	shmHandlesMu.Unlock()
	if !ok {
		return nil
	}
	var firstErr error
	if err := windows.UnmapViewOfFile(shm.addr); err != nil {
		firstErr = err
	}
	if err := windows.CloseHandle(shm.mapping); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// shmNameOf 返回某块已分配段的名字（供注入子进程环境变量）。
func shmNameOf(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	shmHandlesMu.Lock()
	defer shmHandlesMu.Unlock()
	if shm, ok := shmHandles[uintptr(unsafe.Pointer(&data[0]))]; ok {
		return shm.name
	}
	return ""
}

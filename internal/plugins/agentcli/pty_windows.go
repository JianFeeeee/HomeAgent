//go:build windows

package agentcli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreatePseudoConsole              = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole              = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole               = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateProcessW                   = kernel32.NewProc("CreateProcessW")
	procGetExitCodeProcess               = kernel32.NewProc("GetExitCodeProcess")
	procTerminateProcess                 = kernel32.NewProc("TerminateProcess")
	procCloseHandle                      = kernel32.NewProc("CloseHandle")
)

const (
	procThreadAttributePseudoConsole = 0x16 // PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE (22)
	extendedStartupinfoPresent       = 0x00080000
	createUnicodeEnvironment         = 0x00000400
	stillActive                      = 259 // STILL_ACTIVE
)

type coord struct {
	x int16
	y int16
}

type processInformation struct {
	process syscall.Handle
	thread  syscall.Handle
	pid     uint32
	tid     uint32
}

// startupInfoEx 对应 STARTUPINFOEXW：STARTUPINFOW 之后追加 attribute list 指针。
type startupInfoEx struct {
	cb              uint32
	lpReserved      *uint16
	lpDesktop       *uint16
	lpTitle         *uint16
	dwX             uint32
	dwY             uint32
	dwXSize         uint32
	dwYSize         uint32
	dwXCountChars   uint32
	dwYCountChars   uint32
	dwFillAttribute uint32
	dwFlags         uint32
	wShowWindow     uint16
	cbReserved2     uint16
	lpReserved2     *byte
	hStdInput       syscall.Handle
	hStdOutput      syscall.Handle
	hStdErr         syscall.Handle
	lpAttributeList uintptr
}

func defaultShell() string { return "cmd.exe" }

// windowsPty 基于 Windows ConPTY（Pseudo Console）的终端后端。
//
// ConPTY 通过 CreatePseudoConsole 创建伪控制台，子进程以
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE 挂到伪控制台。宿主侧使用两根
// 管道与伪控制台通信：我们写 inW（输入）、读 outR（输出）。
type windowsPty struct {
	hpc       syscall.Handle // 伪控制台句柄
	inW       *os.File       // 我们向伪控制台写输入
	outR      *os.File       // 我们读伪控制台输出
	proc      syscall.Handle // 子进程句柄
	procID    int
	cmd       *exec.Cmd
	attrList  []byte
	closeOnce sync.Once
}

// newCommandPty 创建 ConPTY 并在其上运行命令（cmd.exe /c <command>）。
func newCommandPty(command string, rows, cols uint16) (ptyTerm, *exec.Cmd, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create input pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, nil, fmt.Errorf("create output pipe: %w", err)
	}

	sz := coord{x: int16(cols), y: int16(rows)}
	var hpc syscall.Handle
	r, _, e := procCreatePseudoConsole.Call(
		uintptr(unsafe.Pointer(&sz)),
		inW.Fd(),
		outR.Fd(),
		0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	if r == 0 {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, nil, fmt.Errorf("CreatePseudoConsole: %v", e)
	}

	// 初始化 process thread attribute list 并注入伪控制台句柄
	attrList, err := buildAttrList(hpc)
	if err != nil {
		procClosePseudoConsole.Call(uintptr(hpc))
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, nil, err
	}

	cmdLine := windowsCommandLine(command)
	cli, err := syscall.UTF16PtrFromString(cmdLine)
	if err != nil {
		return nil, nil, err
	}

	var si startupInfoEx
	si.cb = uint32(unsafe.Sizeof(si))
	si.lpAttributeList = uintptr(unsafe.Pointer(&attrList[0]))

	var pi processInformation
	flags := uint32(extendedStartupinfoPresent | createUnicodeEnvironment)
	r, _, e = procCreateProcessW.Call(
		0, // 应用名
		uintptr(unsafe.Pointer(cli)), // 命令行（CreateProcessW 会就地改写，可写 buffer）
		0, 0,                         // 无安全属性
		0,                            // bInheritHandles FALSE
		uintptr(flags),               // 创建标志
		0,                            // 环境（继承）
		0,                            // 工作目录
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&attrList[0])))
		procClosePseudoConsole.Call(uintptr(hpc))
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, nil, fmt.Errorf("CreateProcessW: %v", e)
	}

	// 子进程无需 pipe 的父侧副本；我们只保留 inW/outR
	inR.Close()
	outW.Close()

	cmdObj := exec.Command("cmd.exe")
	cmdObj.Process = &os.Process{Pid: int(pi.pid)}

	pt := &windowsPty{
		hpc:      hpc,
		inW:      inW,
		outR:     outR,
		proc:     pi.process,
		procID:   int(pi.pid),
		cmd:      cmdObj,
		attrList: attrList,
	}
	return pt, cmdObj, nil
}

func buildAttrList(hpc syscall.Handle) ([]byte, error) {
	var size uintptr
	r, _, e := procInitializeProcThreadAttributeList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if r == 0 || size == 0 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList(size): %v", e)
	}
	buf := make([]byte, size)
	r, _, e = procInitializeProcThreadAttributeList.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		1,
		0,
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: %v", e)
	}
	r, _, e = procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		0,
		procThreadAttributePseudoConsole,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0,
		0,
	)
	if r == 0 {
		procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&buf[0])))
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %v", e)
	}
	return buf, nil
}

func windowsCommandLine(command string) string {
	return "cmd.exe /c " + command
}

func (p *windowsPty) Read(buf []byte) (int, error) {
	return p.outR.Read(buf)
}

func (p *windowsPty) WriteString(s string) (int, error) {
	return p.inW.WriteString(s)
}

func (p *windowsPty) Resize(rows, cols uint16) error {
	if p.hpc == 0 {
		return fmt.Errorf("pseudo console closed")
	}
	sz := coord{x: int16(cols), y: int16(rows)}
	r, _, e := procResizePseudoConsole.Call(uintptr(p.hpc), uintptr(unsafe.Pointer(&sz)))
	if r == 0 {
		return fmt.Errorf("ResizePseudoConsole: %v", e)
	}
	return nil
}

func (p *windowsPty) Running() bool {
	if p.proc == 0 {
		return false
	}
	var code uint32
	r, _, _ := procGetExitCodeProcess.Call(uintptr(p.proc), uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		// 句柄失效（进程已退出并释放句柄）视为停止
		return false
	}
	return code == stillActive
}

func (p *windowsPty) Kill() error {
	if p.proc != 0 {
		procTerminateProcess.Call(uintptr(p.proc), 1)
		procCloseHandle.Call(uintptr(p.proc))
		p.proc = 0
	}
	return nil
}

func (p *windowsPty) Close() error {
	var errs []string
	p.closeOnce.Do(func() {
		if p.inW != nil {
			if err := p.inW.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if p.outR != nil {
			if err := p.outR.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if p.hpc != 0 {
			procClosePseudoConsole.Call(uintptr(p.hpc))
			p.hpc = 0
		}
		if len(p.attrList) > 0 {
			procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&p.attrList[0])))
			p.attrList = nil
		}
		_ = p.Kill()
	})
	if len(errs) > 0 {
		return fmt.Errorf("close: %s", strings.Join(errs, "; "))
	}
	return nil
}
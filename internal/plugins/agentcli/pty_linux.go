//go:build linux

package agentcli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// PTY ioctl constants for Linux
const (
	TIOCGPTN   = 0x80045430
	TIOCSPTLCK = 0x40045431
	TIOCSWINSZ = 0x5414
)

type winsize struct {
	Row    uint16
	Col    uint16
	XPixel uint16
	YPixel uint16
}

func ioctl(fd, cmd uintptr, ptr unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, cmd, uintptr(ptr))
	if errno != 0 {
		return errno
	}
	return nil
}

func openPty() (master *os.File, slave *os.File, err error) {
	mfd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master = os.NewFile(uintptr(mfd), "/dev/ptmx")

	var unlock int32
	if err := ioctl(uintptr(mfd), TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}

	var ptyno int32
	if err := ioctl(uintptr(mfd), TIOCGPTN, unsafe.Pointer(&ptyno)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptyno)
	sfd, err := syscall.Open(slavePath, syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", slavePath, err)
	}
	slave = os.NewFile(uintptr(sfd), slavePath)

	return master, slave, nil
}

func defaultShell() string { return "bash" }

// linuxPty 基于 Linux PTY 的终端后端。
type linuxPty struct {
	master *os.File
	slave  *os.File
	cmd    *exec.Cmd
}

func (p *linuxPty) Read(buf []byte) (int, error) { return p.master.Read(buf) }

func (p *linuxPty) WriteString(s string) (int, error) { return p.master.WriteString(s) }

func (p *linuxPty) Resize(rows, cols uint16) error {
	ws := winsize{Row: rows, Col: cols}
	if err := ioctl(uintptr(p.master.Fd()), TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		return fmt.Errorf("TIOCSWINSZ: %w", err)
	}
	return nil
}

// Running 在 Linux 上保持旧语义：进程退出通过 master EOF 由 readLoop/cleanup 感知，
// 因此这里恒返回 true，行为与改造前一致。
func (p *linuxPty) Running() bool { return true }

func (p *linuxPty) Kill() error {
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

func (p *linuxPty) Close() error {
	p.slave.Close()
	return p.master.Close()
}

// newCommandPty 创建 PTY 并在其上启动子命令（sh -c）。
func newCommandPty(command string, rows, cols uint16) (ptyTerm, *exec.Cmd, error) {
	master, slave, err := openPty()
	if err != nil {
		return nil, nil, err
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, nil, fmt.Errorf("start command: %w", err)
	}

	slave.Close()
	pt := &linuxPty{master: master, cmd: cmd}
	if err := pt.Resize(rows, cols); err != nil {
		master.Close()
		cmd.Process.Kill()
		return nil, nil, fmt.Errorf("resize pty: %w", err)
	}
	return pt, cmd, nil
}
//go:build linux

package agentcli

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// PTY ioctl constants for Linux
const (
	TIOCGPTN   = 0x80045430
	TIOCSPTLCK = 0x40045431
	TIOCSWINSZ = 0x5414
)

const (
	DefaultTimeout    = 5 * time.Minute
	ReadBufSize       = 4096
	MaxOutputBuffer   = 128 * 1024
	NotifyOutputDelay = 500 * time.Millisecond
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

type TerminalSession struct {
	id        string
	cmd       *exec.Cmd
	master    *os.File
	slave     *os.File
	mu        sync.Mutex
	buf       bytes.Buffer
	createdAt time.Time
	timeout   time.Duration
	closed    bool
	stopCh    chan struct{}
	done      chan struct{}
}

func (t *TerminalSession) Write(input string) (int, error) {
	return t.master.WriteString(input)
}

func (t *TerminalSession) Read(buf []byte) (int, error) {
	return t.master.Read(buf)
}

func (t *TerminalSession) ReadOutput() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func (t *TerminalSession) ReadAndClearOutput() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.buf.String()
	t.buf.Reset()
	return s
}

func (t *TerminalSession) appendOutput(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buf.Len()+len(data) > MaxOutputBuffer {
		excess := t.buf.Len() + len(data) - MaxOutputBuffer
		if t.buf.Len() > excess {
			t.buf.Next(excess)
		} else {
			t.buf.Reset()
		}
	}
	t.buf.Write(data)
}

func (t *TerminalSession) Resize(rows, cols uint16) error {
	ws := winsize{Row: rows, Col: cols}
	if err := ioctl(uintptr(t.master.Fd()), TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		return fmt.Errorf("TIOCSWINSZ: %w", err)
	}
	return nil
}

func (t *TerminalSession) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()

	close(t.stopCh)
	t.master.Close()
	<-t.done

	t.slave.Close()
	t.cmd.Process.Kill()
}

func (t *TerminalSession) IsExpired() bool {
	return time.Since(t.createdAt) >= t.timeout
}

type Plugin struct {
	name           string
	mu             sync.Mutex
	wg             sync.WaitGroup
	stopCh         chan struct{}
	sessions       map[string]*TerminalSession
	nextID         int
	defaultTimeout time.Duration
}

func New(name string) *Plugin {
	return &Plugin{
		name:     name,
		stopCh:   make(chan struct{}),
		sessions: make(map[string]*TerminalSession),
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "default_timeout", Type: "string", DisplayName: "默认终端超时",
		Description: "终端自动关闭的默认时间，例如 5m, 10m, 30m, 1h（默认 5m）",
		Default:     "5m",
	})
	if v, _ := s.Settings().Get("default_timeout"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil {
				p.defaultTimeout = d
			}
		}
	}
	if p.defaultTimeout <= 0 {
		p.defaultTimeout = DefaultTimeout
	}

	s.RegisterTool("terminal_create", sdk.ToolDef{
		Name:        "terminal_create",
		Description: "创建一个新的交互式终端会话。返回终端 ID，后续通过此 ID 进行读写操作。适用于运行交互式程序如 vim、ssh、top、nano 等。终端默认 5 分钟后自动关闭，可通过 timeout 参数调整。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的命令（默认 bash）。如需运行特定程序直接传入即可，例如：vim /tmp/test.txt",
				},
				"timeout": map[string]interface{}{
					"type":        "string",
					"description": "终端自动关闭时间，例如 5m, 10m, 30m, 1h（默认 5m）",
				},
				"rows": map[string]interface{}{
					"type":        "integer",
					"description": "终端行数（默认 24）",
				},
				"cols": map[string]interface{}{
					"type":        "integer",
					"description": "终端列数（默认 80）",
				},
			},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleCreate(s, args)
	})

	s.RegisterTool("terminal_write", sdk.ToolDef{
		Name:        "terminal_write",
		Description: "向指定终端发送输入。支持普通文本和特殊键（通过 key 参数传入）。特殊键包括：enter, tab, escape, ctrl_a~ctrl_z, alt_a~alt_z, f1~f12, up, down, left, right, home, end, backspace, delete, page_up, page_down。普通文本传入 input 参数即可。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID，来自 terminal_create 的返回值",
				},
				"input": map[string]interface{}{
					"type":        "string",
					"description": "要发送的文本内容（普通文字直接输入）",
				},
				"key": map[string]interface{}{
					"type":        "string",
					"description": "特殊按键：enter, tab, escape, ctrl_a~ctrl_z, alt_a~alt_z, f1~f12, up, down, left, right, home, end, backspace, delete, page_up, page_down",
				},
			},
			"required": []string{"id"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleWrite(s, args)
	})

	s.RegisterTool("terminal_read", sdk.ToolDef{
		Name:        "terminal_read",
		Description: "读取指定终端的当前屏幕内容。返回自上次读取以来的新输出。如需持续监控请多次调用。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID",
				},
				"clear": map[string]interface{}{
					"type":        "boolean",
					"description": "读取后是否清除缓冲区（默认 true）",
				},
			},
			"required": []string{"id"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleRead(args)
	})

	s.RegisterTool("terminal_resize", sdk.ToolDef{
		Name:        "terminal_resize",
		Description: "调整指定终端的尺寸（行数和列数）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID",
				},
				"rows": map[string]interface{}{
					"type":        "integer",
					"description": "行数",
				},
				"cols": map[string]interface{}{
					"type":        "integer",
					"description": "列数",
				},
			},
			"required": []string{"id"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleResize(args)
	})

	s.RegisterTool("terminal_close", sdk.ToolDef{
		Name:        "terminal_close",
		Description: "关闭指定终端会话。释放资源。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "终端 ID",
				},
			},
			"required": []string{"id"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleClose(args)
	})

	s.RegisterTool("terminal_list", sdk.ToolDef{
		Name:        "terminal_list",
		Description: "列出所有活跃的终端会话及其状态。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.handleList()
	})

	p.wg.Add(1)
	go p.cleanupLoop(s)

	return nil
}

func (p *Plugin) Stop() error {
	close(p.stopCh)
	p.wg.Wait()

	p.mu.Lock()
	for _, t := range p.sessions {
		t.Close()
	}
	p.sessions = nil
	p.mu.Unlock()

	return nil
}

func (p *Plugin) nextIDLocked() string {
	p.nextID++
	return fmt.Sprintf("term_%d", p.nextID)
}

func (p *Plugin) handleCreate(s *sdk.PluginSDK, args map[string]interface{}) (interface{}, error) {
	command, _ := args["command"].(string)
	if command == "" {
		command = "bash"
	}

	timeoutStr, _ := args["timeout"].(string)
	timeout := p.defaultTimeout
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		}
	}
	// SSH 命令自动使用更长的超时（5 分钟）
	if timeoutStr == "" && (strings.HasPrefix(command, "ssh ") || strings.HasPrefix(command, "ssh -")) {
		timeout = 5 * time.Minute
	}

	rows := uint16(24)
	cols := uint16(80)
	if r, ok := args["rows"].(float64); ok && r > 0 {
		rows = uint16(r)
	}
	if c, ok := args["cols"].(float64); ok && c > 0 {
		cols = uint16(c)
	}

	master, slave, err := openPty()
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("创建 PTY 失败: %v", err)}, nil
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
		Setctty: true,
		Ctty: 0,
	}

	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return map[string]interface{}{"error": fmt.Sprintf("启动命令失败: %v", err)}, nil
	}

	slave.Close()

	session := &TerminalSession{
		cmd:       cmd,
		master:    master,
		createdAt: time.Now(),
		timeout:   timeout,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}

	p.mu.Lock()
	id := p.nextIDLocked()
	session.id = id
	p.sessions[id] = session

	ws := winsize{Row: rows, Col: cols}
	ioctl(uintptr(master.Fd()), TIOCSWINSZ, unsafe.Pointer(&ws))
	p.mu.Unlock()

	p.wg.Add(1)
	go p.readLoop(session, s)

	log.Printf("[agentcli] created terminal %s: command=%q timeout=%v rows=%d cols=%d", id, command, timeout, rows, cols)

	return map[string]interface{}{
		"id":      id,
		"status":  "created",
		"command": command,
		"timeout": timeout.String(),
		"rows":    rows,
		"cols":    cols,
	}, nil
}

func (p *Plugin) handleWrite(s *sdk.PluginSDK, args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	input, _ := args["input"].(string)
	key, _ := args["key"].(string)

	var data []byte

	if key != "" {
		b, err := mapKey(key)
		if err != nil {
			return map[string]interface{}{"error": err.Error()}, nil
		}
		data = b
	} else if input != "" {
		data = []byte(input)
	} else {
		return map[string]interface{}{"error": "需要提供 input 或 key 参数"}, nil
	}

	n, err := session.Write(string(data))
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("写入失败: %v", err)}, nil
	}

	return map[string]interface{}{
		"status":   "ok",
		"bytes":    n,
		"terminal": id,
	}, nil
}

func (p *Plugin) handleRead(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	clear := true
	if v, ok := args["clear"].(bool); ok {
		clear = v
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	var output string
	if clear {
		output = session.ReadAndClearOutput()
	} else {
		output = session.ReadOutput()
	}

	if output == "" {
		output = "[终端无新输出]"
	}

	return map[string]interface{}{
		"status":   "ok",
		"terminal": id,
		"output":   output,
		"running":  session.cmd.ProcessState == nil || !session.cmd.ProcessState.Exited(),
		"uptime":   time.Since(session.createdAt).String(),
	}, nil
}

func (p *Plugin) handleResize(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	rows, okRows := args["rows"].(float64)
	cols, okCols := args["cols"].(float64)
	if !okRows || !okCols {
		return map[string]interface{}{"error": "rows 和 cols 为必填"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	p.mu.Unlock()
	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	if err := session.Resize(uint16(rows), uint16(cols)); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("调整尺寸失败: %v", err)}, nil
	}

	return map[string]interface{}{
		"status":   "ok",
		"terminal": id,
		"rows":     rows,
		"cols":     cols,
	}, nil
}

func (p *Plugin) handleClose(args map[string]interface{}) (interface{}, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return map[string]interface{}{"error": "id is required"}, nil
	}

	p.mu.Lock()
	session, ok := p.sessions[id]
	if ok {
		delete(p.sessions, id)
	}
	p.mu.Unlock()

	if !ok {
		return map[string]interface{}{"error": fmt.Sprintf("终端 %s 不存在或已关闭", id)}, nil
	}

	session.Close()
	log.Printf("[agentcli] closed terminal %s", id)

	return map[string]interface{}{
		"status":   "closed",
		"terminal": id,
	}, nil
}

func (p *Plugin) handleList() (interface{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	type termInfo struct {
		ID       string `json:"id"`
		Command  string `json:"command"`
		Uptime   string `json:"uptime"`
		ExpiresIn string `json:"expires_in"`
		Running  bool   `json:"running"`
	}

	var terms []termInfo
	for _, t := range p.sessions {
		running := t.cmd.ProcessState == nil || !t.cmd.ProcessState.Exited()
		remaining := t.timeout - time.Since(t.createdAt)
		if remaining < 0 {
			remaining = 0
		}
		terms = append(terms, termInfo{
			ID:        t.id,
			Uptime:    time.Since(t.createdAt).Round(time.Second).String(),
			ExpiresIn: remaining.Round(time.Second).String(),
			Running:   running,
		})
	}

	if terms == nil {
		terms = []termInfo{}
	}

	return map[string]interface{}{
		"status":   "ok",
		"count":    len(terms),
		"terminals": terms,
	}, nil
}

func (p *Plugin) readLoop(t *TerminalSession, s *sdk.PluginSDK) {
	defer p.wg.Done()
	defer close(t.done)

	buf := make([]byte, ReadBufSize)
	lastNotify := time.Now()
	pollInterval := 200 * time.Millisecond

	readCh := make(chan readResult, 4)
	go p.reader(t, buf, readCh)

	for {
		if t.IsExpired() {
			log.Printf("[agentcli] terminal %s expired after %v", t.id, t.timeout)
			s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 已超时关闭（%s）]", t.id, t.timeout))
			p.mu.Lock()
			delete(p.sessions, t.id)
			p.mu.Unlock()
			return
		}

		if t.cmd.ProcessState != nil && t.cmd.ProcessState.Exited() {
			s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 中的进程已退出]", t.id))
			p.mu.Lock()
			delete(p.sessions, t.id)
			p.mu.Unlock()
			return
		}

		select {
		case <-t.stopCh:
			return
		case r := <-readCh:
			if r.err != nil {
				return
			}
			if r.n > 0 {
				data := make([]byte, r.n)
				copy(data, buf[:r.n])
				t.appendOutput(data)
				if time.Since(lastNotify) > NotifyOutputDelay {
					preview := string(data)
					if len(preview) > 100 {
						preview = preview[:100]
					}
					preview = sanitizePreview(preview)
					s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 有新输出]\n%s", t.id, preview))
					lastNotify = time.Now()
				}
			}
		case <-time.After(pollInterval):
		}
	}
}

type readResult struct {
	n   int
	err error
}

func (p *Plugin) reader(t *TerminalSession, buf []byte, ch chan<- readResult) {
	for {
		n, err := t.master.Read(buf)
		select {
		case ch <- readResult{n, err}:
		case <-t.stopCh:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *Plugin) cleanupLoop(s *sdk.PluginSDK) {
	defer p.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.mu.Lock()
			for id, t := range p.sessions {
				if t.IsExpired() {
					log.Printf("[agentcli] cleanup: terminal %s expired", id)
					delete(p.sessions, id)
					go func(term *TerminalSession) {
						term.Close()
					}(t)
				}
				if t.cmd.ProcessState != nil && t.cmd.ProcessState.Exited() {
					log.Printf("[agentcli] cleanup: terminal %s process exited", id)
					delete(p.sessions, id)
					go func(term *TerminalSession) {
						term.Close()
					}(t)
				}
			}
			p.mu.Unlock()
		}
	}
}

func mapKey(key string) ([]byte, error) {
	key = strings.ToLower(key)
	switch key {
	case "enter":
		return []byte{0x0D}, nil
	case "tab":
		return []byte{0x09}, nil
	case "escape", "esc":
		return []byte{0x1B}, nil
	case "backspace":
		return []byte{0x7F}, nil
	case "delete":
		return []byte{0x1B, 0x5B, 0x33, 0x7E}, nil
	case "home":
		return []byte{0x1B, 0x5B, 0x48}, nil
	case "end":
		return []byte{0x1B, 0x5B, 0x46}, nil
	case "page_up":
		return []byte{0x1B, 0x5B, 0x35, 0x7E}, nil
	case "page_down":
		return []byte{0x1B, 0x5B, 0x36, 0x7E}, nil
	case "up":
		return []byte{0x1B, 0x5B, 0x41}, nil
	case "down":
		return []byte{0x1B, 0x5B, 0x42}, nil
	case "right":
		return []byte{0x1B, 0x5B, 0x43}, nil
	case "left":
		return []byte{0x1B, 0x5B, 0x44}, nil
	}

	if strings.HasPrefix(key, "ctrl_") && len(key) == 6 {
		c := key[5]
		if c >= 'a' && c <= 'z' {
			return []byte{byte(c - 'a' + 1)}, nil
		}
	}

	if strings.HasPrefix(key, "alt_") && len(key) == 5 {
		c := key[4]
		if c >= 'a' && c <= 'z' {
			return []byte{0x1B, c}, nil
		}
	}

	if strings.HasPrefix(key, "f") && len(key) <= 4 {
		var n int
		if _, err := fmt.Sscanf(key, "f%d", &n); err == nil && n >= 1 && n <= 12 {
			return functionKeyCode(n), nil
		}
	}

	return nil, fmt.Errorf("不支持的特殊按键: %s", key)
}

func functionKeyCode(n int) []byte {
	switch n {
	case 1:
		return []byte{0x1B, 0x5B, 0x50}
	case 2:
		return []byte{0x1B, 0x5B, 0x51}
	case 3:
		return []byte{0x1B, 0x5B, 0x52}
	case 4:
		return []byte{0x1B, 0x5B, 0x53}
	case 5:
		return []byte{0x1B, 0x5B, 0x31, 0x35, 0x7E}
	case 6:
		return []byte{0x1B, 0x5B, 0x31, 0x37, 0x7E}
	case 7:
		return []byte{0x1B, 0x5B, 0x31, 0x38, 0x7E}
	case 8:
		return []byte{0x1B, 0x5B, 0x31, 0x39, 0x7E}
	case 9:
		return []byte{0x1B, 0x5B, 0x32, 0x30, 0x7E}
	case 10:
		return []byte{0x1B, 0x5B, 0x32, 0x31, 0x7E}
	case 11:
		return []byte{0x1B, 0x5B, 0x32, 0x32, 0x7E}
	case 12:
		return []byte{0x1B, 0x5B, 0x32, 0x34, 0x7E}
	default:
		return []byte{}
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if opErr, ok := err.(*os.PathError); ok {
		err = opErr.Err
	}
	if syscallErr, ok := err.(syscall.Errno); ok {
		return syscallErr == syscall.EAGAIN || syscallErr == syscall.EWOULDBLOCK || syscallErr == syscall.ETIMEDOUT
	}
	return strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "would block")
}

func init() {
	plugin.RegisterFactory("agentcli", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
	plugin.RegisterPluginMeta("agentcli", "终端交互", "Agent CLI")
}

func sanitizePreview(s string) string {
	var buf bytes.Buffer
	for _, r := range s {
		if r == '\n' {
			buf.WriteString("\\n")
		} else if r == '\r' {
			buf.WriteString("\\r")
		} else if r == '\t' {
			buf.WriteString("\\t")
		} else if r >= 32 && r <= 126 {
			buf.WriteRune(r)
		} else if r == 0x1B {
			buf.WriteString("^[")
		} else if r < 32 {
			fmt.Fprintf(&buf, "^%c", r+'A'-1)
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

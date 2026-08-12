//go:build linux || windows

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

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

const (
	DefaultTimeout      = 5 * time.Minute
	ReadBufSize         = 4096
	MaxOutputBuffer     = 128 * 1024
	DefaultNotifyBytes  = 2048 // 积累 2KB 未读输出再通知
	DefaultNotifyInterval = 2 * time.Second // 同一终端两次通知的最小间隔（兜底）
)

// ptyTerm 抽象平台终端后端（Linux PTY / Windows ConPTY）。
type ptyTerm interface {
	Read(buf []byte) (int, error)
	WriteString(s string) (int, error)
	Resize(rows, cols uint16) error
	Running() bool
	Kill() error
	Close() error
}

// terminalRunning 判断终端后端进程是否仍在运行：优先走平台实现，
// 否则回落到 exec.Cmd 的 ProcessState（仅 Linux 传统路径）。
func terminalRunning(t *TerminalSession) bool {
	if p, ok := t.session.(interface {
		Running() bool
	}); ok && p.Running() {
		return true
	}
	return t.cmd != nil && (t.cmd.ProcessState == nil || !t.cmd.ProcessState.Exited())
}

type TerminalSession struct {
	id        string
	cmd       *exec.Cmd
	session   ptyTerm
	mu        sync.Mutex
	buf       bytes.Buffer
	createdAt time.Time
	timeout   time.Duration
	closed    bool
	stopCh    chan struct{}
	done      chan struct{}

	// 通知节流字段
	unreadBytes   int           // 最近一次通知后积累的未读字节数
	lastNotify    time.Time     // 最近一次通知时间
}

func (t *TerminalSession) Write(input string) (int, error) {
	return t.session.WriteString(input)
}

func (t *TerminalSession) Read(buf []byte) (int, error) {
	return t.session.Read(buf)
}

func (t *TerminalSession) Resize(rows, cols uint16) error {
	return t.session.Resize(rows, cols)
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
	t.session.Close()
	<-t.done

	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
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
	notifyBytes    int
	notifyInterval time.Duration
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
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "notify_bytes", Type: "int", DisplayName: "通知阈值字节数",
		Description: "累积多少字节未读输出后发送通知（默认 2048）",
		Default:     "2048",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "notify_interval", Type: "string", DisplayName: "通知最小间隔",
		Description: "同一终端两次通知的最小时间间隔，如 2s, 5s（默认 2s）",
		Default:     "2s",
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
	if v, _ := s.Settings().Get("notify_bytes"); v != nil {
		if i, ok := v.(float64); ok && i > 0 {
			p.notifyBytes = int(i)
		}
	}
	if p.notifyBytes <= 0 {
		p.notifyBytes = DefaultNotifyBytes
	}
	if v, _ := s.Settings().Get("notify_interval"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil {
				p.notifyInterval = d
			}
		}
	}
	if p.notifyInterval <= 0 {
		p.notifyInterval = DefaultNotifyInterval
	}

	s.RegisterTool("terminal_create", sdk.ToolDef{
		Name:        "terminal_create",
		Description: "创建一个新的交互式终端会话。返回终端 ID，后续通过此 ID 进行读写操作。适用于运行交互式程序如 vim、ssh、top、nano 等。终端默认 5 分钟后自动关闭，可通过 timeout 参数调整。",
		NoMemory:    true,
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
		NoMemory:    true,
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
		NoMemory:    true,
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
		NoMemory:    true,
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
		NoMemory:    true,
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
		NoMemory:    true,
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
		command = defaultShell()
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

	term, cmd, err := newCommandPty(command, rows, cols)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("创建终端失败: %v", err)}, nil
	}

	session := &TerminalSession{
		cmd:       cmd,
		session:   term,
		createdAt: time.Now(),
		timeout:   timeout,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}

	p.mu.Lock()
	id := p.nextIDLocked()
	session.id = id
	p.sessions[id] = session
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
		"running":  terminalRunning(session),
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
		running := terminalRunning(t)
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
	pollInterval := 200 * time.Millisecond

	readCh := make(chan readResult, 4)
	go p.reader(t, buf, readCh)

	// 立即发送首次"终端已启动"通知，让 agent 感知存在
	s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 已启动]", t.id))
	t.lastNotify = time.Now()

	for {
		if t.IsExpired() {
			log.Printf("[agentcli] terminal %s expired after %v", t.id, t.timeout)
			s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 已超时关闭（%s）]", t.id, t.timeout))
			p.mu.Lock()
			delete(p.sessions, t.id)
			p.mu.Unlock()
			return
		}

		if !terminalRunning(t) {
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
				// 读取错误/EOF → 立即通知（进程可能已结束）
				s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 读取结束: %v]", t.id, r.err))
				return
			}
			if r.n > 0 {
				data := make([]byte, r.n)
				copy(data, buf[:r.n])
				t.appendOutput(data)

				// 语义通知：累积未读字节数
				t.mu.Lock()
				t.unreadBytes += r.n
				needNotify := t.unreadBytes >= p.notifyBytes ||
					time.Since(t.lastNotify) >= p.notifyInterval
				t.mu.Unlock()

				if needNotify {
					t.mu.Lock()
					preview := t.buf.String()
					if len(preview) > 200 {
						preview = preview[len(preview)-200:] // 取最新 200 字符
					}
					preview = sanitizePreview(preview)
					t.unreadBytes = 0
					t.lastNotify = time.Now()
					t.mu.Unlock()

					s.InjectText("agentcli", "agentcli", fmt.Sprintf("[终端 %s 有新输出]\n%s", t.id, preview))
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
		n, err := t.session.Read(buf)
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
				if !terminalRunning(t) {
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

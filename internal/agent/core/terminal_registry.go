package core

import (
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// TerminalRegistry 是**内核侧**的终端会话与命令历史权威视图（「内核开，两个插件接」）。
//
// 此前 WebUI 与 CLI 插件各自订阅 EventToolCall / EventTerminalOutput 攒一份
// 状态：同一件事两份推导，还各自踩过同一个坑（工具 result 是 Go map 文本，
// 断言成 map[string]interface{} 永远失败 → /terminals 空空如也）。
//
// 现在权威状态收归内核一份：内核订阅自己的事件总线，把 terminal_* / cmd_run
// 的工具调用与 agentcli 的 terminal_output 事件归并成唯一真相；
// WebUI 和 CLI 都从 s.Status().GetKernelStatus() 读取，不再各自推导。
type TerminalRegistry struct {
	mu    sync.Mutex
	terms map[string]*TermState
	cmds  []CmdExec
}

// TermState 与 WebUI 的 termState / CLI 的 cliTermState 同字段（/terminals 口径）。
type TermState struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`
	Output    string `json:"output"`
	CreatedAt string `json:"created_at"`
	Uptime    string `json:"uptime"`
	created   time.Time
}

// CmdExec 与 WebUI 的 CmdExec 同字段（/cmd/history 口径）。
type CmdExec struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status"`
	Time     string `json:"time"`
}

const (
	maxCmdHistory = 100
	maxTerminals  = 50
	maxTermOutput = 64 * 1024
)

func NewTerminalRegistry() *TerminalRegistry {
	return &TerminalRegistry{
		terms: make(map[string]*TermState),
	}
}

// OnToolCall 归并内核自己发布的 EventToolCall（agent 路径；result 是 Go map
// 文本，id/command 从 args 或 map 文本里回填）。
func (r *TerminalRegistry) OnToolCall(payload map[string]interface{}) {
	tool, _ := payload["tool"].(string)
	args, _ := payload["args"].(map[string]interface{})
	status, _ := payload["status"].(string)
	switch tool {
	case "cmd_run":
		r.mu.Lock()
		r.cmds = append(r.cmds, CmdExec{
			Command: getStr2(args, "command"),
			Status:  status,
			Time:    time.Now().Format(time.RFC3339),
		})
		if len(r.cmds) > maxCmdHistory {
			r.cmds = r.cmds[len(r.cmds)-maxCmdHistory:]
		}
		r.mu.Unlock()
	case "terminal_create":
		id := getStr2(args, "id")
		if id == "" {
			id = terminalIDFromResultPayload(payload)
		}
		if id == "" {
			return
		}
		cmd := getStr2(args, "command")
		if cmd == "" {
			cmd = mapFieldFromResultPayload(payload, "command")
		}
		r.mu.Lock()
		if old, ok := r.terms[id]; ok {
			old.Command = cmd
			old.Running = true
			old.created = time.Now()
		} else {
			r.terms[id] = &TermState{
				ID:        id,
				Command:   cmd,
				Running:   true,
				CreatedAt: time.Now().Format(time.RFC3339),
				created:   time.Now(),
			}
		}
		if len(r.terms) > maxTerminals {
			for k := range r.terms {
				delete(r.terms, k)
				break
			}
		}
		r.mu.Unlock()
	case "terminal_close":
		id := getStr2(args, "id")
		if id != "" {
			r.mu.Lock()
			if t, ok := r.terms[id]; ok {
				t.Running = false
			}
			r.mu.Unlock()
		}
	}
}

// OnTerminalOutput 归并 agentcli 的 terminal_output 事件（含生命周期事件：
// 创建时带 command，关闭/退出/超时带 running=false）。
func (r *TerminalRegistry) OnTerminalOutput(payload map[string]interface{}) {
	id, _ := payload["terminal_id"].(string)
	if id == "" {
		return
	}
	output, _ := payload["output"].(string)
	running, _ := payload["running"].(bool)
	command, _ := payload["command"].(string)

	r.mu.Lock()
	ts, ok := r.terms[id]
	if !ok {
		ts = &TermState{ID: id, created: time.Now()}
		if command != "" {
			ts.Command = command
		}
		ts.CreatedAt = time.Now().Format(time.RFC3339)
		r.terms[id] = ts
	}
	if command != "" {
		ts.Command = command
	}
	ts.Running = running
	if output != "" {
		if len(ts.Output)+len(output) > maxTermOutput {
			excess := len(ts.Output) + len(output) - maxTermOutput
			if len(ts.Output) > excess {
				ts.Output = ts.Output[excess:]
			} else {
				ts.Output = ""
			}
		}
		ts.Output += output
	}
	if len(r.terms) > maxTerminals {
		for k := range r.terms {
			delete(r.terms, k)
			break
		}
	}
	r.mu.Unlock()
}

// ListTerminals / CmdHistory 实现 sdk.TerminalAPI（内核对插件开放的终端接口）。
func (r *TerminalRegistry) ListTerminals() []sdk.TerminalStatus {
	terms, _ := r.Snapshot()
	return terms
}

func (r *TerminalRegistry) CmdHistory() []sdk.CmdExecStatus {
	_, cmds := r.Snapshot()
	return cmds
}

// Snapshot 返回加过 Uptime 的终端列表与命令历史快照（拷贝，调用方可随意改）。
func (r *TerminalRegistry) Snapshot() ([]sdk.TerminalStatus, []sdk.CmdExecStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	terms := make([]sdk.TerminalStatus, 0, len(r.terms))
	for _, t := range r.terms {
		terms = append(terms, sdk.TerminalStatus{
			ID:        t.ID,
			Command:   t.Command,
			Running:   t.Running,
			Output:    t.Output,
			CreatedAt: t.CreatedAt,
			Uptime:    time.Since(t.created).Round(time.Second).String(),
		})
	}
	cmds := make([]sdk.CmdExecStatus, len(r.cmds))
	for i, c := range r.cmds {
		cmds[i] = sdk.CmdExecStatus(c)
	}
	return terms, cmds
}

func getStr2(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// terminalIDFromResultPayload 从 EventToolCall payload 的 result 里抠 terminal id。
// result 是 Go map 文本（map[cols:80 ... id:term_2 ...]），不是结构化对象。
func terminalIDFromResultPayload(payload map[string]interface{}) string {
	res, _ := payload["result"].(string)
	return terminalIDFromMapText(res)
}

func mapFieldFromResultPayload(payload map[string]interface{}, key string) string {
	res, _ := payload["result"].(string)
	return mapFieldFromMapText(res, key)
}

// terminalIDFromMapText / mapFieldFromMapText 解析 Go map 文本的字段。
//
// 为什么不能信 payload["result"] 是 map[string]interface{}：工具结果在
// executeToolCall → ToolResultItem.Output 就已被 fmt 序列化成文本
// （map[cols:80 command:sleep 120 id:term_2 ...]），事件负载里拿到的
// 永远是字符串。用正则按空格切字段即可，id/command 不含空格。
func terminalIDFromMapText(s string) string {
	return mapFieldFromMapText(s, "id")
}

func mapFieldFromMapText(s, key string) string {
	s = strings.TrimSpace(s)
	// 剥掉 Go 的 map[...] 外壳，否则外层中括号把深度抬到 1，
	// 内部所有空格都不再被当成字段分隔。
	if strings.HasPrefix(s, "map[") && strings.HasSuffix(s, "]") {
		s = s[len("map[") : len(s)-1]
	}
	for _, f := range splitMapTextFields(s) {
		k, v, ok := parseMapField(f)
		if ok && k == key {
			return v
		}
	}
	return ""
}

func splitMapTextFields(s string) []string {
	var fields []string
	depth := 0
	cur := ""
	for _, c := range s {
		switch c {
		case '[', '{', '(':
			depth++
		case ']', '}', ')':
			if depth > 0 {
				depth--
			}
		case ' ':
			if depth == 0 && cur != "" {
				fields = append(fields, cur)
				cur = ""
				continue
			}
		}
		cur += string(c)
	}
	if cur != "" {
		fields = append(fields, cur)
	}
	return fields
}

func parseMapField(f string) (k, v string, ok bool) {
	for i := 0; i < len(f); i++ {
		if f[i] == ':' {
			return f[:i], f[i+1:], true
		}
	}
	return "", "", false
}

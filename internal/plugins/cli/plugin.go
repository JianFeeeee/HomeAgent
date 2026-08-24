package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// DefaultSocket 由 main.go 在 Load() 前设置，覆盖默认 socket 路径。
var DefaultSocket string

const (
	cliSource  = "cli"
	cliChannel = "cli"
)

func init() {
	plugin.RegisterPluginMeta("cli", "CLI", "CLI")
	plugin.RegisterFactory("cli", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		sock := DefaultSocket
		if sock == "" {
			dataDir, ok := config["data_dir"].(string)
			if !ok {
				return nil, fmt.Errorf("cli plugin: config missing 'data_dir' or not a string")
			}
			sock = filepath.Join(dataDir, "cli.sock")
		}
		return New(name, sock), nil
	})
}

type Plugin struct {
	name   string
	socket string
	ln     net.Listener
	mu     sync.Mutex
	wg     sync.WaitGroup
}

func New(name, socketPath string) *Plugin {
	return &Plugin{
		name:   name,
		socket: socketPath,
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	s.RegisterOutputChannel("cli", 1, "CLI 终端", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		if payload != "" {
			fmt.Println(payload)
			s.Publish(&sdk.Event{
				Type: sdk.EventAgentOutput,
				Payload: map[string]interface{}{
					"content": payload,
					"channel": "cli",
					"kind":    "channel_output",
				},
			})
		}
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "api_key", Type: "password", DisplayName: "CLI API 密钥",
		Description: "CLI 客户端连接时需提供的认证密钥（留空则使用 WebUI 密钥）",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "socket_path", Type: "string", DisplayName: "Socket 管道路径",
		Description: "CLI Unix 域套接字监听路径（留空则使用默认路径）",
	})
	if v, _ := s.Settings().Get("socket_path"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			p.socket = s
		}
	}

	dir := filepath.Dir(p.socket)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	os.Remove(p.socket)

	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		return fmt.Errorf("listen unix socket %s: %w", p.socket, err)
	}
	p.ln = ln

	os.Chmod(p.socket, 0666)

	p.wg.Add(1)
	go p.acceptLoop(s)

	log.Printf("[cli] unix socket listening on %s", p.socket)
	return nil
}

func (p *Plugin) acceptLoop(s *sdk.PluginSDK) {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			break
		}
		p.wg.Add(1)
		go p.handleConn(conn, s)
	}
}

func (p *Plugin) handleConn(conn net.Conn, s *sdk.PluginSDK) {
	defer conn.Close()
	defer p.wg.Done()

	scanner := bufio.NewScanner(conn)

	apiKey := p.cliAPIKey(s)
	if apiKey != "" {
		if !scanner.Scan() {
			return
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "/auth ") || strings.TrimSpace(line[6:]) != apiKey {
			writeLine(conn, map[string]interface{}{
				"type":  "error",
				"error": "unauthorized",
			})
			return
		}
		writeLine(conn, map[string]interface{}{
			"type":    "response",
			"content": "authenticated",
		})
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if line[0] == '/' {
			if p.handleBuiltin(conn, line, s) {
				continue
			}
		}

		p.handleChat(&connWriter{conn: conn}, line, s)
	}
}

// connWriter 为单条连接提供互斥保护的 JSON 行写入。
// 对话过程中事件订阅回调运行在事件总线的发布 goroutine 上，
// 与主循环写最终响应并发，因此写入必须串行化。
type connWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *connWriter) writeLine(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	w.mu.Lock()
	w.conn.Write(data)
	w.mu.Unlock()
}

// handleChat 处理一条对话消息：订阅内核的推理/工具调用事件并实时
// 转发给客户端（流式过程输出），InjectTextSync 返回后写出最终响应。
// 仅插件层改动：通过 SDK 订阅事件，不触碰内核。
func (p *Plugin) handleChat(w *connWriter, line string, s *sdk.PluginSDK) {
	unsubReasoning := s.Subscribe(sdk.EventReasoning, func(evt *sdk.Event) {
		if ch, _ := evt.Payload["channel"].(string); ch != cliChannel {
			return
		}
		content, _ := evt.Payload["content"].(string)
		if content == "" {
			return
		}
		w.writeLine(map[string]interface{}{"type": "reasoning", "content": content})
	})
	unsubToolCall := s.Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
		if ch, _ := evt.Payload["channel"].(string); ch != cliChannel {
			return
		}
		tool, _ := evt.Payload["tool"].(string)
		status, _ := evt.Payload["status"].(string)
		result, _ := evt.Payload["result"].(string)
		w.writeLine(map[string]interface{}{
			"type":   "tool_call",
			"tool":   tool,
			"status": status,
			"result": truncateOneLine(result, 160),
		})
	})
	defer unsubReasoning()
	defer unsubToolCall()

	resp := s.InjectTextSync(cliSource, cliChannel, line)
	if resp != nil {
		content, _ := resp.Payload["content"].(string)
		w.writeLine(map[string]interface{}{
			"type":    "response",
			"content": content,
		})
	} else {
		w.writeLine(map[string]interface{}{
			"type":  "error",
			"error": "agent is not available",
		})
	}
}

func (p *Plugin) cliAPIKey(s *sdk.PluginSDK) string {
	if s != nil {
		if v, _ := s.Settings().Get("api_key"); v != nil {
			if k, ok := v.(string); ok && k != "" {
				return k
			}
		}
	}
	return p.webuiAPIKey(s)
}

func (p *Plugin) webuiAPIKey(s *sdk.PluginSDK) string {
	if s == nil {
		return ""
	}
	v, _ := s.Settings().GetPlugin("webui", "api_key")
	if k, ok := v.(string); ok {
		return k
	}
	return ""
}

func (p *Plugin) handleBuiltin(conn net.Conn, line string, s *sdk.PluginSDK) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}

	switch parts[0] {
	case "/help":
		p.cmdHelp(conn)
	case "/status":
		p.cmdStatus(conn, s)
	case "/kernel":
		p.cmdKernel(conn, s)
	case "/settings":
		p.cmdSettings(conn, parts, s)
	case "/plugin":
		p.cmdPlugin(conn, parts, s)
	case "/memory":
		p.cmdMemory(conn, parts, s)
	case "/knowledge":
		p.cmdKnowledge(conn, s)
	case "/agents":
		p.cmdAgents(conn, s)
	default:
		return false
	}
	return true
}

func (p *Plugin) cmdHelp(conn net.Conn) {
	writeLine(conn, map[string]interface{}{
		"type": "response",
		"content": `内置命令（直接对话内核，不依赖网络）:
  /help                        显示此帮助
  /status                      系统运行状态
  /kernel                      内核状态（插件、工具、LLM、记忆）
  /settings                    列出所有配置
  /settings set <key> <val>    修改配置项
  /settings core.llm           按前缀筛选
  /plugin list                 列出所有插件
  /plugin install <url>        安装插件（需回环网络）
  /plugin remove <name>        卸载插件
  /plugin disable <name>       禁用插件
  /plugin enable <name>        启用插件
  /plugin info <name>          查看插件详情
  /memory query <关键词>        查询图记忆
  /knowledge                   列出知识库
  /agents                      列出 Agent

其他文本直接发送给 Agent 处理。`,
	})
}

// ======== /status ========

func (p *Plugin) cmdStatus(conn net.Conn, s *sdk.PluginSDK) {
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := st.GetKernelStatus()

	llmStatus := "不可用"
	if ks.LLM.Available {
		llmStatus = fmt.Sprintf("%s (%d sources)", ks.LLM.Provider, ks.LLM.Sources)
	}
	memInfo := "未初始化"
	if ks.Memory.Available {
		memInfo = fmt.Sprintf("%d entities, %d relations", ks.Memory.EntityCount, ks.Memory.RelationCount)
	}
	runtime := fmt.Sprintf("goroutines=%d mem=%dMB", ks.Runtime.Goroutines, ks.Runtime.MemoryMB)
	uptime := ks.Uptime

	writeLine(conn, map[string]interface{}{
		"type": "response",
		"content": fmt.Sprintf(`HomeAgent 内核状态
  状态:   running
  运行:   %s
  LLM:    %s
  记忆:   %s
  运行时: %s
  插件:   %d loaded
  工具:   %d registered`, uptime, llmStatus, memInfo, runtime,
			len(ks.Plugins), len(ks.Tools)),
	})
}

// ======== /kernel ========

func (p *Plugin) cmdKernel(conn net.Conn, s *sdk.PluginSDK) {
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	data, _ := json.MarshalIndent(st.GetKernelStatus(), "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== /settings ========

func (p *Plugin) cmdSettings(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	sett := s.Settings()
	if sett == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "config registry not available"})
		return
	}

	if len(parts) >= 2 && parts[1] == "set" {
		if len(parts) < 4 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /settings set <key> <value>"})
			return
		}
		key := parts[2]
		val := strings.Join(parts[3:], " ")
		if err := sett.SetCore(key, val); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("已设置: %s = %s", key, val)})
		return
	}

	prefix := ""
	if len(parts) >= 2 {
		prefix = parts[1]
	}
	keys, _ := sett.ListCore(prefix)
	sort.Strings(keys)
	if len(keys) == 0 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "无匹配配置项"})
		return
	}
	var lines []string
	for _, k := range keys {
		v, _ := sett.GetCore(k)
		lines = append(lines, fmt.Sprintf("  %s = %v", k, v))
	}
	writeLine(conn, map[string]interface{}{
		"type":    "response",
		"content": fmt.Sprintf("配置 (%d 项):\n%s", len(keys), strings.Join(lines, "\n")),
	})
}

// ======== /plugin ========

func (p *Plugin) cmdPlugin(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	if len(parts) < 2 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin list|install <url>|remove <name>|info <name>|disable <name>|enable <name>"})
		return
	}

	pmgr := s.PluginMgr()

	switch parts[1] {
	case "list":
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		names := pmgr.ListLoadedPlugins()
		disabled := pmgr.ListDisabledPlugins()
		disabledNames := make(map[string]bool)
		for _, d := range disabled {
			disabledNames[d.Name] = true
		}
		all := make(map[string]string)
		for _, n := range names {
			all[n] = "\033[32m已加载\033[0m"
		}
		for _, d := range disabled {
			if _, ok := all[d.Name]; !ok {
				all[d.Name] = "\033[31m已禁用\033[0m"
			} else {
				all[d.Name] = "\033[32m已加载\033[0m (禁用将在重启后生效)"
			}
		}
		if len(all) == 0 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "无插件"})
			return
		}
		var lines []string
		for name, status := range all {
			lines = append(lines, fmt.Sprintf("  %s %s", name, status))
		}
		sort.Strings(lines)
		writeLine(conn, map[string]interface{}{
			"type":    "response",
			"content": fmt.Sprintf("插件 (%d):\n%s", len(all), strings.Join(lines, "\n")),
		})

	case "install":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin install <url>"})
			return
		}
		writeLine(conn, map[string]interface{}{
			"type":    "response",
			"content": "安装插件需要网络，本环境可能受限。请通过 WebUI 或使用 agent 对话安装。",
		})

	case "remove":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin remove <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		name := parts[2]
		dir := pmgr.PluginDir()
		if dir == "" {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin dir not configured"})
			return
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		// 同步清理禁用表
		_ = pmgr.EnablePlugin(name)
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已删除，执行 /plugin reload 生效", name)})

	case "info":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin info <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		name := parts[2]
		metas := pmgr.PluginMetas()
		meta, hasMeta := metas[name]
		loaded := false
		for _, n := range pmgr.ListLoadedPlugins() {
			if n == name {
				loaded = true
				break
			}
		}
		if loaded {
			display := name
			if hasMeta && meta.NameZh != "" {
				display = meta.NameZh
			}
			writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("名称: %s (%s)\n状态: 已加载", display, name)})
			return
		}
		if pmgr.IsPluginDisabled(name) {
			writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %q 已禁用", name)})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %q 未安装", name)})

	case "disable":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin disable <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		if err := pmgr.DisablePlugin(parts[2], "cli"); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已禁用", parts[2])})

	case "enable":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin enable <name>"})
			return
		}
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		if err := pmgr.EnablePlugin(parts[2]); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已启用", parts[2])})

	case "reload":
		if pmgr == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin manager not available"})
			return
		}
		if _, err := pmgr.ReloadPlugins(); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": "插件已重载"})

	default:
		writeLine(conn, map[string]interface{}{"type": "response", "content": "未知: /plugin " + parts[1] + "。支持: list, install, remove, info, disable, enable, reload"})
	}
}

// ======== /memory ========

func (p *Plugin) cmdMemory(conn net.Conn, parts []string, s *sdk.PluginSDK) {
	if len(parts) < 3 || parts[1] != "query" {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /memory query <关键词>"})
		return
	}
	q := strings.Join(parts[2:], " ")

	mem := s.Memory()
	if mem == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "memory not available"})
		return
	}
	entities, relations, err := mem.Recall([]string{q}, 2)
	if err != nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
		return
	}
	result := map[string]interface{}{
		"entities":  entities,
		"relations": relations,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== /knowledge ========

func (p *Plugin) cmdKnowledge(conn net.Conn, s *sdk.PluginSDK) {
	ks := s.Knowledge()
	if ks == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "knowledge not available"})
		return
	}
	items, err := ks.List()
	if err != nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
		return
	}
	if len(items) == 0 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "知识库为空"})
		return
	}
	data, _ := json.MarshalIndent(items, "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== /agents ========

func (p *Plugin) cmdAgents(conn net.Conn, s *sdk.PluginSDK) {
	st := s.Status()
	if st == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := st.GetKernelStatus()
	data, _ := json.MarshalIndent(map[string]string{"agent_id": ks.AgentID}, "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== helpers ========

// truncateOneLine 将多行文本压成单行并按 rune 截断，用于事件结果预览。
func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func writeLine(conn net.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	conn.Write(data)
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
	}
	p.wg.Wait()
	os.Remove(p.socket)
	return nil
}

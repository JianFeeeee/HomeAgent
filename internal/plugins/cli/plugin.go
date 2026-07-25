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

	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// DefaultSocket 由 main.go 在 Load() 前设置，覆盖默认 socket 路径。
var DefaultSocket string

// 以下通过 Configure() 注入内核依赖
var (
	pluginReg  *plugin.Registry
	cfgReg     *internalConfig.ConfigRegistry
	statusProv agentCore.StatusProvider
	pluginDir  string
)

// Configure 由 main.go 在 Load() 前调用，注入内核依赖供结构化命令使用。
func Configure(pr *plugin.Registry, cr *internalConfig.ConfigRegistry, sp agentCore.StatusProvider, pDir string) {
	pluginReg = pr
	cfgReg = cr
	statusProv = sp
	pluginDir = pDir
}

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

	s.RegisterOutputChannel("cli", 1, "CLI 终端", func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		if payload != "" {
			fmt.Println(payload)
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

		resp := s.InjectTextSync("cli", "cli", line)
		if resp != nil {
			content, _ := resp.Payload["content"].(string)
			writeLine(conn, map[string]interface{}{
				"type":    "response",
				"content": content,
			})
		} else {
			writeLine(conn, map[string]interface{}{
				"type":  "error",
				"error": "agent is not available",
			})
		}
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
	return p.webuiAPIKey()
}

func (p *Plugin) webuiAPIKey() string {
	if cfgReg == nil {
		return ""
	}
	ps := cfgReg.PluginConfig("webui")
	if v, _ := ps.Get("api_key"); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
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
		p.cmdStatus(conn)
	case "/kernel":
		p.cmdKernel(conn)
	case "/settings":
		p.cmdSettings(conn, parts)
	case "/plugin":
		p.cmdPlugin(conn, parts)
	case "/memory":
		p.cmdMemory(conn, parts, s)
	case "/knowledge":
		p.cmdKnowledge(conn, s)
	case "/agents":
		p.cmdAgents(conn)
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
  /plugin list                 列出已安装插件
  /plugin install <url>        安装插件（需回环网络）
  /plugin remove <name>        卸载插件
  /plugin info <name>          查看插件详情
  /memory query <关键词>        查询图记忆
  /knowledge                   列出知识库
  /agents                      列出 Agent

其他文本直接发送给 Agent 处理。`,
	})
}

// ======== /status ========

func (p *Plugin) cmdStatus(conn net.Conn) {
	if statusProv == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := statusProv.GetKernelStatus()

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

func (p *Plugin) cmdKernel(conn net.Conn) {
	if statusProv == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	data, _ := json.MarshalIndent(statusProv.GetKernelStatus(), "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== /settings ========

func (p *Plugin) cmdSettings(conn net.Conn, parts []string) {
	if cfgReg == nil {
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
		if err := cfgReg.Set(key, val); err != nil {
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
	keys := cfgReg.List(prefix)
	sort.Strings(keys)
	if len(keys) == 0 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "无匹配配置项"})
		return
	}
	var lines []string
	for _, k := range keys {
		v, _ := cfgReg.Get(k)
		lines = append(lines, fmt.Sprintf("  %s = %v", k, v))
	}
	writeLine(conn, map[string]interface{}{
		"type":    "response",
		"content": fmt.Sprintf("配置 (%d 项):\n%s", len(keys), strings.Join(lines, "\n")),
	})
}

// ======== /plugin ========

func (p *Plugin) cmdPlugin(conn net.Conn, parts []string) {
	if len(parts) < 2 {
		writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin list|install <url>|remove <name>|info <name>"})
		return
	}

	switch parts[1] {
	case "list":
		if pluginReg == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin registry not available"})
			return
		}
		names := pluginReg.List()
		if len(names) == 0 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "无已加载插件"})
			return
		}
		writeLine(conn, map[string]interface{}{
			"type":    "response",
			"content": fmt.Sprintf("已加载插件 (%d):\n  %s", len(names), strings.Join(names, "\n  ")),
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
		name := parts[2]
		if pluginDir == "" {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin dir not configured"})
			return
		}
		dir := filepath.Join(pluginDir, name)
		if err := os.RemoveAll(dir); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %s 已删除，执行 /plugin reload 生效", name)})

	case "info":
		if len(parts) < 3 {
			writeLine(conn, map[string]interface{}{"type": "response", "content": "用法: /plugin info <name>"})
			return
		}
		if pluginReg == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin registry not available"})
			return
		}
		plg := pluginReg.Get(parts[2])
		if plg == nil {
			writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("插件 %q 未加载", parts[2])})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": fmt.Sprintf("名称: %s\n状态: 已加载", plg.Name())})

	case "reload":
		if pluginReg == nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": "plugin registry not available"})
			return
		}
		if _, err := pluginReg.Reload(pluginDir); err != nil {
			writeLine(conn, map[string]interface{}{"type": "error", "error": err.Error()})
			return
		}
		writeLine(conn, map[string]interface{}{"type": "response", "content": "插件已重载"})

	default:
		writeLine(conn, map[string]interface{}{"type": "response", "content": "未知: /plugin " + parts[1] + "。支持: list, install, remove, info, reload"})
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

func (p *Plugin) cmdAgents(conn net.Conn) {
	if statusProv == nil {
		writeLine(conn, map[string]interface{}{"type": "error", "error": "status provider not available"})
		return
	}
	ks := statusProv.GetKernelStatus()
	data, _ := json.MarshalIndent(map[string]string{"agent_id": ks.AgentID}, "", "  ")
	writeLine(conn, map[string]interface{}{"type": "response", "content": string(data)})
}

// ======== helpers ========

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

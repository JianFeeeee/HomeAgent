package mcp

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// MCP 服务器配置（来自 config_mcp 表或 skill.json）
type serverConfig struct {
	Name    string   `json:"name"`
	Command string   `json:"command,omitempty"`  // stdio 模式
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	URL     string   `json:"url,omitempty"`      // SSE 模式
}

func init() {
	plugin.RegisterPluginMeta("mcp", "MCP 服务器", "MCP")
	plugin.RegisterFactory("mcp", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	servers []*Server
	configs []serverConfig
	sdk     *sdk.PluginSDK
	mu      sync.Mutex
	wg      sync.WaitGroup
}

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	cfgs, err := p.loadConfig(s)
	if err != nil {
		return fmt.Errorf("load mcp config: %w", err)
	}
	p.configs = cfgs
	if len(cfgs) == 0 {
		log.Printf("[mcp] no servers configured, idle")
		return nil
	}

	for _, cfg := range cfgs {
		if err := p.connectAndRegister(cfg); err != nil {
			log.Printf("[mcp] connect %s: %v", cfg.Name, err)
		}
	}

	tDef := sdk.ToolDef{
		Name:        "mcp_restart_server",
		Description: "重启 MCP 服务器连接。当 MCP 工具返回 pipe/transport closed 错误时，用此工具重启指定的 MCP 服务器。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "MCP 服务器名称（如 email）",
				},
			},
			"required": []string{"name"},
		},
	}
	if err := s.RegisterTool("mcp_restart_server", tDef, p.restartServerHandler); err != nil {
		log.Printf("[mcp] register restart tool: %v", err)
	}

	return nil
}

func (p *Plugin) loadConfig(s *sdk.PluginSDK) ([]serverConfig, error) {
	// 优先从独立服务器配置键读取（servers.<name>.<field>）
	keys, _ := s.Settings().List("servers.")
	if len(keys) > 0 {
		serverNames := make(map[string]bool)
		for _, k := range keys {
			parts := strings.SplitN(k, ".", 3)
			if len(parts) >= 2 {
				serverNames[parts[1]] = true
			}
		}
		var cfgs []serverConfig
		for name := range serverNames {
			cfg := serverConfig{Name: name}
			if v, _ := s.Settings().Get("servers." + name + ".command"); v != nil {
				if s, ok := v.(string); ok {
					cfg.Command = s
				}
			}
			if v, _ := s.Settings().Get("servers." + name + ".url"); v != nil {
				if s, ok := v.(string); ok {
					cfg.URL = s
				}
			}
			if v, _ := s.Settings().Get("servers." + name + ".args"); v != nil {
				if s, ok := v.(string); ok && s != "" {
					json.Unmarshal([]byte(s), &cfg.Args)
				}
			}
			if v, _ := s.Settings().Get("servers." + name + ".env"); v != nil {
				if s, ok := v.(string); ok && s != "" {
					json.Unmarshal([]byte(s), &cfg.Env)
				}
			}
			if cfg.Command != "" || cfg.URL != "" {
				cfgs = append(cfgs, cfg)
			}
		}
		if len(cfgs) > 0 {
			return cfgs, nil
		}
	}

	// 回退：从旧版 JSON blob 读取
	raw, err := s.Settings().Get("servers")
	if err == nil {
		switch v := raw.(type) {
		case string:
			var cfgs []serverConfig
			if err := json.Unmarshal([]byte(v), &cfgs); err == nil && len(cfgs) > 0 {
				return cfgs, nil
			}
		case []interface{}:
			data, _ := json.Marshal(v)
			var cfgs []serverConfig
			if json.Unmarshal(data, &cfgs) == nil && len(cfgs) > 0 {
				return cfgs, nil
			}
		}
	}

	return nil, nil
}

func (p *Plugin) connectServer(cfg serverConfig) (*Server, []MCPTool, error) {
	var transport Transport

	if cfg.URL != "" {
		transport = NewSSETransport(cfg.URL)
	} else if cfg.Command != "" {
		var err error
		transport, err = NewStdioTransport(cfg.Command, cfg.Args, cfg.Env)
		if err != nil {
			return nil, nil, fmt.Errorf("stdio transport: %w", err)
		}
	} else {
		return nil, nil, fmt.Errorf("neither command nor url specified")
	}

	server := NewServer(cfg.Name, transport)

	tools, err := server.ListTools()
	if err != nil {
		transport.Close()
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}

	return server, tools, nil
}

func (p *Plugin) makeHandler(server *Server, toolName string) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		return server.CallTool(toolName, args)
	}
}

func (p *Plugin) connectAndRegister(cfg serverConfig) error {
	server, tools, err := p.connectServer(cfg)
	if err != nil {
		return err
	}

	for _, tool := range tools {
		toolName := fmt.Sprintf("%s_%s", cfg.Name, tool.Name)
		tDef := sdk.ToolDef{
			Name:        toolName,
			Description: fmt.Sprintf("[MCP/%s] %s", cfg.Name, tool.Description),
			Parameters:  tool.InputSchema,
		}
		tHandler := p.makeHandler(server, tool.Name)
		if err := p.sdk.RegisterTool(toolName, tDef, tHandler); err != nil {
			log.Printf("[mcp] register tool %s: %v", toolName, err)
			continue
		}
		log.Printf("[mcp] registered tool: %s (%s)", toolName, cfg.Name)
	}

	p.mu.Lock()
	p.servers = append(p.servers, server)
	p.mu.Unlock()
	log.Printf("[mcp] connected server: %s (%d tools)", cfg.Name, len(tools))
	return nil
}

func (p *Plugin) restartServerHandler(args map[string]interface{}) (interface{}, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "参数 name 不能为空", nil
	}

	p.mu.Lock()
	var idx int = -1
	for i, s := range p.servers {
		if s.Name() == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		p.mu.Unlock()
		return fmt.Sprintf("MCP 服务器 [%s] 不存在", name), nil
	}
	oldServer := p.servers[idx]
	p.mu.Unlock()

	var cfg *serverConfig
	for i := range p.configs {
		if p.configs[i].Name == name {
			cfg = &p.configs[i]
			break
		}
	}
	if cfg == nil {
		return fmt.Sprintf("MCP 服务器 [%s] 的配置未找到", name), nil
	}

	transport, err := newTransport(*cfg)
	if err != nil {
		return fmt.Sprintf("创建 MCP 服务器 [%s] 传输层失败: %v", name, err), nil
	}

	oldServer.SetTransport(transport)

	_, err = oldServer.ListTools()
	if err != nil {
		return fmt.Sprintf("MCP 服务器 [%s] 重启后通信仍异常: %v", name, err), nil
	}

	return fmt.Sprintf("MCP 服务器 [%s] 已成功重启", name), nil
}

func newTransport(cfg serverConfig) (Transport, error) {
	if cfg.URL != "" {
		return NewSSETransport(cfg.URL), nil
	}
	if cfg.Command != "" {
		return NewStdioTransport(cfg.Command, cfg.Args, cfg.Env)
	}
	return nil, fmt.Errorf("neither command nor url specified")
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.servers {
		s.Close()
	}
	p.servers = nil
	return nil
}

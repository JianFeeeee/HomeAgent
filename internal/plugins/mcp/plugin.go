package mcp

import (
	"encoding/json"
	"fmt"
	"log"
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
	plugin.RegisterFactory("mcp", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	servers []*Server
	mu      sync.Mutex
	wg      sync.WaitGroup
}

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "servers",
		Default:     "",
		Type:        "text",
		DisplayName: "MCP 服务器配置",
		Description: "MCP 服务器列表，JSON 数组格式，包含 name、command/url、args、env 等字段",
		Category:    "mcp",
	})

	cfgs, err := p.loadConfig(s)
	if err != nil {
		return fmt.Errorf("load mcp config: %w", err)
	}
	if len(cfgs) == 0 {
		log.Printf("[mcp] no servers configured, idle")
		return nil
	}

	for _, cfg := range cfgs {
		server, tools, err := p.connectServer(cfg)
		if err != nil {
			log.Printf("[mcp] connect %s: %v", cfg.Name, err)
			continue
		}

		for _, tool := range tools {
			toolName := fmt.Sprintf("%s_%s", cfg.Name, tool.Name)
			tDef := sdk.ToolDef{
				Name:        toolName,
				Description: fmt.Sprintf("[MCP/%s] %s", cfg.Name, tool.Description),
				Parameters:  tool.InputSchema,
			}
			tHandler := p.makeHandler(server, tool.Name)
			if err := s.RegisterTool(toolName, tDef, tHandler); err != nil {
				log.Printf("[mcp] register tool %s: %v", toolName, err)
				continue
			}
			log.Printf("[mcp] registered tool: %s (%s)", toolName, cfg.Name)
		}

		p.mu.Lock()
		p.servers = append(p.servers, server)
		p.mu.Unlock()
		log.Printf("[mcp] connected server: %s (%d tools)", cfg.Name, len(tools))
	}

	return nil
}

func (p *Plugin) loadConfig(s *sdk.PluginSDK) ([]serverConfig, error) {
	// 优先从 skill.json（config map）读取
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

	// 备用：从 JSON 文件读取
	// 没有配置时不报错，只返回空
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

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.servers {
		s.Close()
	}
	p.servers = nil
	return nil
}

package api

import (
	"log"
	"net/http"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	luaVM "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

// Plugin 将 HTTP API + WebUI 包装为 IO Device
// 作为 HomeAgent 自带的默认 IO 通道插件
type Plugin struct {
	name    string
	handler *Handler
	server  *http.Server
	addr    string
	mux     *http.ServeMux
}

func NewWebUIPlugin(name, addr string, sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager,
	lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager, tm *text.Memory, ks *knowledge.Store, tr *tracker.Tracker) *Plugin {

	h := NewHandler(sup, mem, sk, lua, cfg, iom, tm, ks, tr)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	return &Plugin{
		name:    name,
		handler: h,
		addr:    addr,
		mux:     mux,
	}
}

func (p *Plugin) Name() string                           { return p.name }
func (p *Plugin) Type() agentIO.DeviceType               { return agentIO.DeviceIO }
func (p *Plugin) Description() string                    { return "HTTP API & Web Dashboard" }
func (p *Plugin) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText | agentIO.CapStructured }
func (p *Plugin) Tools() []agentIO.ToolDef                { return nil }
func (p *Plugin) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return nil, nil
}

func (p *Plugin) Start() error {
	p.server = &http.Server{Addr: p.addr, Handler: p.mux}
	go func() {
		log.Printf("[webui] HTTP server listening on %s", p.addr)
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[webui] server error: %v", err)
		}
	}()
	return nil
}

func (p *Plugin) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

package webui

import (
	"log"
	"net/http"

	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	luaVM "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

// 包级依赖注入 — 由 main.go 在 Load() 前调用 Configure() 设置。
var (
	webuiAddr           string
	webuiSup            *supervisor.Daemon
	webuiMem            *memory.GraphDB
	webuiSK             *skill.Manager
	webuiLua            *luaVM.VM
	webuiCfg            *types.Config
	webuiIOM            *agentIO.IOManager
	webuiTM             *text.Memory
	webuiKS             *knowledge.Store
	webuiTR             *tracker.Tracker
	webuiCR             *internalConfig.ConfigRegistry
	webuiPR             *plugin.Registry
	webuiEvBus          *events.Bus
	webuiStatusProvider agentCore.StatusProvider
)

// Configure 注入 WebUI 插件需要的内核依赖。必须在 Load() 之前调用。
func Configure(addr string,
	sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager,
	lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager,
	tm *text.Memory, ks *knowledge.Store, tr *tracker.Tracker,
	cr *internalConfig.ConfigRegistry, pr *plugin.Registry, evBus *events.Bus,
	sp agentCore.StatusProvider,
) {
	webuiAddr = addr
	webuiSup, webuiMem, webuiSK, webuiLua = sup, mem, sk, lua
	webuiCfg, webuiIOM, webuiTM, webuiKS = cfg, iom, tm, ks
	webuiTR, webuiCR, webuiPR, webuiEvBus = tr, cr, pr, evBus
	webuiStatusProvider = sp
}

func init() {
	plugin.RegisterFactory("webui", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		if webuiSup == nil {
			return nil, nil // 未 Configure 则跳过（不给日志警告）
		}
		addr := webuiAddr
		if a, ok := config["addr"].(string); ok {
			addr = a
		}
		return New(name, addr,
			webuiSup, webuiMem, webuiSK, webuiLua,
			webuiCfg, webuiIOM, webuiTM, webuiKS,
			webuiTR, webuiCR, webuiPR, webuiEvBus, webuiStatusProvider,
		), nil
	})
}

type Plugin struct {
	name    string
	addr    string
	handler *Handler
	server  *http.Server
	mux     *http.ServeMux

	sup    *supervisor.Daemon
	mem    *memory.GraphDB
	sk     *skill.Manager
	lua    *luaVM.VM
	cfg    *types.Config
	iom    *agentIO.IOManager
	tm     *text.Memory
	ks     *knowledge.Store
	tr     *tracker.Tracker
	cr     *internalConfig.ConfigRegistry
	pr     *plugin.Registry
	evBus  *events.Bus
	statusProvider agentCore.StatusProvider
}

func New(name, addr string,
	sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager,
	lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager,
	tm *text.Memory, ks *knowledge.Store, tr *tracker.Tracker,
	cr *internalConfig.ConfigRegistry, pr *plugin.Registry, evBus *events.Bus,
	sp agentCore.StatusProvider,
) *Plugin {
	return &Plugin{
		name: name,
		addr: addr,
		mux:  http.NewServeMux(),
		sup:  sup, mem: mem, sk: sk, lua: lua, cfg: cfg,
		iom: iom, tm: tm, ks: ks, tr: tr, cr: cr, pr: pr, evBus: evBus,
		statusProvider: sp,
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	h := NewHandler(p.sup, p.mem, p.sk, p.lua, p.cfg, p.iom, p.tm, p.ks, p.tr, p.cr, p.pr, p.evBus, p.statusProvider)
	p.handler = h
	h.RegisterRoutes(p.mux)

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

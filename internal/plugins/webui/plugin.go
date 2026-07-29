package webui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
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
	webuiProviderMgr    *agentAPI.ProviderManager
	webuiBaseAPIKey     string
)

// Configure 注入 WebUI 插件需要的内核依赖。必须在 Load() 之前调用。
func Configure(addr string,
	sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager,
	lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager,
	tm *text.Memory, ks *knowledge.Store, tr *tracker.Tracker,
	cr *internalConfig.ConfigRegistry, pr *plugin.Registry, evBus *events.Bus,
	sp agentCore.StatusProvider, pm *agentAPI.ProviderManager, baseKey string,
) {
	webuiAddr = addr
	webuiSup, webuiMem, webuiSK, webuiLua = sup, mem, sk, lua
	webuiCfg, webuiIOM, webuiTM, webuiKS = cfg, iom, tm, ks
	webuiTR, webuiCR, webuiPR, webuiEvBus = tr, cr, pr, evBus
	webuiStatusProvider = sp
	webuiProviderMgr = pm
	webuiBaseAPIKey = baseKey
}

func init() {
	plugin.RegisterPluginMeta("webui", "Web 控制台", "WebUI")
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
			webuiProviderMgr, webuiBaseAPIKey,
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
	providerMgr    *agentAPI.ProviderManager
	baseAPIKey     string
}

func New(name, addr string,
	sup *supervisor.Daemon, mem *memory.GraphDB, sk *skill.Manager,
	lua *luaVM.VM, cfg *types.Config, iom *agentIO.IOManager,
	tm *text.Memory, ks *knowledge.Store, tr *tracker.Tracker,
	cr *internalConfig.ConfigRegistry, pr *plugin.Registry, evBus *events.Bus,
	sp agentCore.StatusProvider, pm *agentAPI.ProviderManager, baseKey string,
) *Plugin {
	return &Plugin{
		name: name,
		addr: addr,
		mux:  http.NewServeMux(),
		sup:  sup, mem: mem, sk: sk, lua: lua, cfg: cfg,
		iom: iom, tm: tm, ks: ks, tr: tr, cr: cr, pr: pr, evBus: evBus,
		statusProvider: sp, providerMgr: pm, baseAPIKey: baseKey,
	}
}

func randomSecret(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func (p *Plugin) ensureAuthBootstrap(s *sdk.PluginSDK) {
	sett := s.Settings()
	if sett == nil {
		return
	}
	if v, _ := sett.Get("username"); v == nil || fmt.Sprint(v) == "" {
		_ = sett.Set("username", "admin")
	}
	if v, _ := sett.Get("password"); v == nil || fmt.Sprint(v) == "" {
		pw := randomSecret(12)
		_ = sett.Set("password", pw)
		log.Printf("[webui] bootstrap password generated for user admin: %s", pw)
	}
	if v, _ := sett.Get("api_key"); v == nil || fmt.Sprint(v) == "" {
		key := randomSecret(16)
		_ = sett.Set("api_key", key)
		log.Printf("[webui] bootstrap api_key generated: %s", key)
	}
	if v, _ := sett.Get("session_ttl_hours"); v == nil || fmt.Sprint(v) == "" {
		_ = sett.Set("session_ttl_hours", "24")
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	s.RegisterOutputChannel("webui", 1, "Web 控制台", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		if payload != "" {
			p.evBus.Publish(&events.Event{
				Type: events.EventAgentOutput,
				Payload: map[string]interface{}{
					"content": payload,
					"channel": "webui",
				},
			})
		}
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "api_key", Default: "", Type: "password", DisplayName: "API 密钥", Description: "访问 API 时需要的密钥", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "username", Default: "admin", Type: "string", DisplayName: "登录用户名", Description: "Web 控制台登录用户名", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "password", Default: "", Type: "password", DisplayName: "Web 控制台登录密码", Description: "Web 控制台登录密码", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "session_ttl_hours", Default: "24", Type: "int", DisplayName: "会话时长(小时)", Description: "登录 cookie 有效时长", Category: "webui"})
	p.ensureAuthBootstrap(s)

	s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
		p.evBus.Publish(&events.Event{Type: events.EventStage, Payload: map[string]interface{}{"phase": "pre_action", "message": "thinking"}})
		return nil
	})
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		tool := ""
		if len(ctx.ToolCalls) > 0 {
			tool = ctx.ToolCalls[0].Name
		}
		p.evBus.Publish(&events.Event{Type: events.EventStage, Payload: map[string]interface{}{"phase": "before_toolcall", "tool": tool, "message": "tool:" + tool}})
		return nil
	})
	s.RegisterStage(sdk.StageBeforeOutput, func(ctx *sdk.StageContext) error {
		p.evBus.Publish(&events.Event{Type: events.EventStage, Payload: map[string]interface{}{"phase": "before_output", "message": "output"}})
		return nil
	})

	h := NewHandler(p.sup, p.mem, p.sk, p.lua, p.cfg, p.iom, p.tm, p.ks, p.tr, p.cr, p.pr, p.evBus, p.statusProvider, p.providerMgr, p.baseAPIKey)
	h.SetPluginMgr(s.PluginMgr())
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

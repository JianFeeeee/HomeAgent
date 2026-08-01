package webui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("webui", "Web 控制台", "WebUI")
	plugin.RegisterFactory("webui", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	handler *Handler
	server  *http.Server
	mux     *http.ServeMux
}

func New(name string) *Plugin {
	return &Plugin{
		name: name,
		mux:  http.NewServeMux(),
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

	addr := ":8080"
	if v, _ := s.Settings().Get("addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			addr = s2
		}
	}

	s.RegisterOutputChannel("webui", 1, "Web 控制台", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		if payload != "" {
			s.Publish(&sdk.Event{
				Type: sdk.EventAgentOutput,
				Payload: map[string]interface{}{
					"content": payload,
					"channel": "webui",
				},
			})
		}
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "addr", Default: ":8080", Type: "string", DisplayName: "监听地址", Description: "Web 控制台监听地址", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "api_key", Default: "", Type: "password", DisplayName: "API 密钥", Description: "访问 API 时需要的密钥", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "username", Default: "admin", Type: "string", DisplayName: "登录用户名", Description: "Web 控制台登录用户名", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "password", Default: "", Type: "password", DisplayName: "Web 控制台登录密码", Description: "Web 控制台登录密码", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "session_ttl_hours", Default: "24", Type: "int", DisplayName: "会话时长(小时)", Description: "登录 cookie 有效时长", Category: "webui"})
	p.ensureAuthBootstrap(s)

	s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
		s.Publish(&sdk.Event{Type: sdk.EventStage, Payload: map[string]interface{}{"phase": "pre_action", "message": "thinking"}})
		return nil
	})
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		tool := ""
		if len(ctx.ToolCalls) > 0 {
			tool = ctx.ToolCalls[0].Name
		}
		s.Publish(&sdk.Event{Type: sdk.EventStage, Payload: map[string]interface{}{"phase": "before_toolcall", "tool": tool, "message": "tool:" + tool}})
		return nil
	})
	s.RegisterStage(sdk.StageBeforeOutput, func(ctx *sdk.StageContext) error {
		s.Publish(&sdk.Event{Type: sdk.EventStage, Payload: map[string]interface{}{"phase": "before_output", "message": "output"}})
		return nil
	})

	p.handler = NewHandler(s)
	p.handler.RegisterRoutes(p.mux)

	p.server = &http.Server{Addr: addr, Handler: p.mux}
	go func() {
		log.Printf("[webui] HTTP server listening on %s", addr)
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

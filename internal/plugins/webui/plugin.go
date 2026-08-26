package webui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

// webFilesDir 是 agent 向 webui 发送文件时的中转目录（<data>/webui_files）。
// 由插件 Start 时从 daemon.data_dir 推导注入。
var webFilesDir string

// stageWebFile 把 agent 要发送的本地文件拷贝到 webui_files 中转目录，
// 返回可下载 URL 路径与字节数。image/file 的 payload 支持本地路径或 http(s) URL
// （URL 直接透传给前端，不落盘）。文件名用随机 UUID 防路径猜测，扩展名保留自源文件。
func stageWebFile(payload string, isImage bool) (url string, size int64, err error) {
	if strings.HasPrefix(payload, "http://") || strings.HasPrefix(payload, "https://") {
		return payload, 0, nil // 远程 URL 直接透传
	}
	if webFilesDir == "" {
		return "", 0, fmt.Errorf("webui files dir not initialized")
	}
	src := payload
	if _, err := os.Stat(src); err != nil {
		return "", 0, fmt.Errorf("文件不存在: %s", src)
	}
	if err := os.MkdirAll(webFilesDir, 0755); err != nil {
		return "", 0, fmt.Errorf("create webui_files: %w", err)
	}
	buf := make([]byte, 8)
	rand.Read(buf)
	ext := strings.ToLower(filepath.Ext(src))
	if extBad(ext) {
		ext = ".bin"
	}
	name := hex.EncodeToString(buf) + ext
	dst := filepath.Join(webFilesDir, name)
	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", 0, fmt.Errorf("create dest: %w", err)
	}
	defer out.Close()
	n, err := io.Copy(out, in)
	if err != nil {
		os.Remove(dst)
		return "", 0, fmt.Errorf("copy: %w", err)
	}
	return "/files/" + name, n, nil
}

// extBad 过滤危险/无意义扩展名（防双扩展名绕过 Content-Type）。
func extBad(ext string) bool {
	switch ext {
	case "", ".html", ".htm", ".svg", ".js", ".exe", ".sh", ".bat", ".cmd", ".ps1":
		return true
	}
	return false
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

	// 中转目录：<data>/webui_files，agent 发送 image/file 时拷贝至此
	if dd, err := s.Settings().GetCore("daemon.data_dir"); err == nil {
		if s2, ok := dd.(string); ok && s2 != "" {
			webFilesDir = filepath.Join(s2, "webui_files")
		}
	}

	addr := ":8080"
	if v, _ := s.Settings().Get("addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			addr = s2
		}
	}

	// 能力位 7 = CapText|CapFile|CapImage；旧值 1 仅文本，agent 无法向 webui 发文件/图片
	s.RegisterOutputChannel("webui", 7, "Web 控制台（支持文字/图片/文件，图片内联展示、文件可下载）", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		rawType, _ := args["type"].(string)
		if payload == "" {
			return nil, fmt.Errorf("payload 不能为空")
		}
		// 能力位：CapText|CapFile|CapImage = 1|2|4 = 7（旧值 1 仅文本）。
		// image/file 时 payload 为本地路径（或 http URL），拷贝到 webui_files
		// 并经 /files/ 带鉴权下发；前端按 kind 渲染图片预览/文件下载卡片。
		if rawType == "image" || rawType == "file" {
			url, size, err := stageWebFile(payload, rawType == "image")
			if err != nil {
				return nil, err
			}
			s.Publish(&sdk.Event{
				Type: sdk.EventAgentOutput,
				Payload: map[string]interface{}{
					"content":    payload,
					"channel":    "webui",
					"kind":       "channel_output",
					"output_type": rawType,
					"url":        url,
					"size":       size,
				},
			})
			return map[string]interface{}{"status": "ok", "url": url, "size": size}, nil
		}
		s.Publish(&sdk.Event{
			Type: sdk.EventAgentOutput,
			Payload: map[string]interface{}{
				"content": payload,
				"channel": "webui",
				"kind":    "channel_output",
			},
		})
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "addr", Default: ":8080", Type: "string", DisplayName: "监听地址", Description: "Web 控制台监听地址", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "api_key", Default: "", Type: "password", DisplayName: "API 密钥", Description: "访问 API 时需要的密钥", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "username", Default: "admin", Type: "string", DisplayName: "登录用户名", Description: "Web 控制台登录用户名", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "password", Default: "", Type: "password", DisplayName: "Web 控制台登录密码", Description: "Web 控制台登录密码", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "session_ttl_hours", Default: "24", Type: "int", DisplayName: "会话时长(小时)", Description: "登录 cookie 有效时长", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "device_gateway_enabled", Default: "false", Type: "bool", DisplayName: "设备网关反代", Description: "启用后 /api/v1/device/* 反代到 remotedevice 插件（默认关闭，避免硬耦合）", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "device_gateway_addr", Default: "127.0.0.1:9890", Type: "string", DisplayName: "设备网关地址", Description: "remotedevice 插件的内部监听地址", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "device_gateway_token", Default: "", Type: "password", DisplayName: "设备网关令牌", Description: "访问 remotedevice 的 token（与 remotedevice 的 ws_token 一致）", Category: "webui"})
	p.ensureAuthBootstrap(s)
	// 注入设备网关反代配置（默认禁用；仅当用户开启时才挂载路由）
	if v, _ := s.Settings().Get("device_gateway_enabled"); v != nil {
		if s2, ok := v.(string); ok && s2 == "true" {
			deviceGatewayEnabled = true
		}
	}
	if v, _ := s.Settings().Get("device_gateway_addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			deviceGatewayAddr = s2
		}
	}
	if v, _ := s.Settings().Get("device_gateway_token"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			deviceGatewayToken = s2
		}
	}

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

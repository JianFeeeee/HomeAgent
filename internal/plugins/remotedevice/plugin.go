package remotedevice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("remotedevice", "远程设备网关", "Remote Device")
	plugin.RegisterFactory("remotedevice", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

const defaultAddr = "127.0.0.1:9890"

// Plugin 是 remotedevice 设备接入网关插件：
// 持有 HTTP 服务（WS 设备通道 + REST 管理面）与 devicectl Device（agent 工具）。
type Plugin struct {
	name     string
	registry *Registry
	mux      *http.ServeMux
	server   *http.Server
	addr     string
	token    string
	sdk      *sdk.PluginSDK
	dev      *devicectlDevice
}

func New(name string) *Plugin {
	return &Plugin{
		name:     name,
		registry: NewRegistry(),
		mux:      http.NewServeMux(),
	}
}

func (p *Plugin) Name() string { return p.name }

func genToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("tok-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func newReqID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s

	// ---- 设置 ----------------
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "listen_addr", Default: defaultAddr, Type: "string", DisplayName: "监听地址", Description: "设备网关 HTTP/WS 监听地址（默认 127.0.0.1:9890，仅本机）", Category: "remotedevice"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "ws_token", Default: "", Type: "password", DisplayName: "接入 Token", Description: "设备绑定/接入时使用的令牌；留空启动时自动生成", Category: "remotedevice"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "authorized_devices", Default: "", Type: "text", DisplayName: "已授权设备", Description: "逗号分隔的已授权设备 ID 列表（由系统维护）", Category: "remotedevice"})

	p.addr = defaultAddr
	if v, _ := s.Settings().Get("listen_addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			p.addr = s2
		}
	}
	p.token = ""
	if v, _ := s.Settings().Get("ws_token"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			p.token = s2
		}
	}
	if p.token == "" {
		p.token = genToken()
		if err := s.Settings().Set("ws_token", p.token); err != nil {
			log.Printf("[remotedevice] persist ws_token: %v", err)
		}
	}
	p.registry.SetAcceptToken(func(provided string) bool {
		return provided != "" && provided == p.token
	})

	// 恢复已授权设备集合
	if v, _ := s.Settings().Get("authorized_devices"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			var ids []string
			for _, id := range strings.Split(s2, ",") {
				if id = strings.TrimSpace(id); id != "" {
					ids = append(ids, id)
				}
			}
			p.registry.RestoreAuthorized(ids)
		}
	}

	// ---- devicectl Device（agent 工具） ----------------
	p.dev = &devicectlDevice{reg: p.registry, persist: p.persistAuthorized}
	// screensee 视觉描述回调：截屏回传后用视觉模型描述屏幕内容
	p.dev.SetSeeHandler(p.describeScreen)
	if err := s.RegisterChannel("devicectl", p.dev); err != nil {
		log.Printf("[remotedevice] register devicectl channel: %v", err)
	}

	// ---- REST 管理面 + WS 设备通道 ----------------
	p.registerRoutes()
	p.server = &http.Server{Addr: p.addr, Handler: p.mux}
	go func() {
		log.Printf("[remotedevice] device gateway listening on %s", p.addr)
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[remotedevice] server error: %v", err)
		}
	}()
	return nil
}

// persistAuthorized 在授权变更后写回设置（持久化重启不丢）。
func (p *Plugin) persistAuthorized() {
	if p.sdk == nil {
		return
	}
	ids := p.registry.AuthorizedIDs()
	_ = p.sdk.Settings().Set("authorized_devices", strings.Join(ids, ","))
}

func (p *Plugin) registerRoutes() {
	// 设备通道（WS）
	p.mux.HandleFunc("/api/v1/device/ws", p.registry.ServeWS)
	// REST 管理面（全部需 token）
	p.mux.HandleFunc("/api/v1/device", p.requireToken(p.handleDeviceList))
	p.mux.HandleFunc("/api/v1/device/online", p.requireToken(p.handleDeviceOnline))
	p.mux.HandleFunc("/api/v1/device/", p.requireToken(p.handleDeviceByID))
	p.mux.HandleFunc("/api/v1/device/push", p.requireToken(p.handleDevicePush))
	p.mux.HandleFunc("/api/v1/device/auth", p.requireToken(p.handleDeviceAuth))
}

// requireToken 校验 REST 请求的接入令牌（X-API-Key header 或 ?token=）。
func (p *Plugin) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-API-Key")
		if k == "" {
			k = r.URL.Query().Get("token")
		}
		if k == "" || k != p.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Plugin) handleDeviceList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"devices": p.registry.List()})
}

func (p *Plugin) handleDeviceOnline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"devices": p.registry.OnlineList()})
}

func (p *Plugin) handleDeviceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/device/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		m, ok := p.registry.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "device not found"})
			return
		}
		writeJSON(w, http.StatusOK, m)
		return
	}
	http.NotFound(w, r)
}

func (p *Plugin) handleDevicePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req struct {
		DeviceID string                 `json:"device_id"`
		Payload  map[string]interface{} `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	if req.DeviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "device_id required"})
		return
	}
	if err := p.registry.PushJSON(req.DeviceID, req.Payload); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (p *Plugin) handleDeviceAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req struct {
		DeviceID  string `json:"device_id"`
		Authorize bool   `json:"authorize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	if req.DeviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "device_id required"})
		return
	}
	if _, ok := p.registry.Get(req.DeviceID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "device not found"})
		return
	}
	p.registry.SetAuthorized(req.DeviceID, req.Authorize)
	p.persistAuthorized()
	writeJSON(w, http.StatusOK, map[string]interface{}{"device_id": req.DeviceID, "authorized": req.Authorize})
}

// describeScreen 用视觉模型描述设备屏幕截图（screensee 回调）。
// provider 为空时使用默认 LLM 源；模型不支持视觉时返回友好错误。
func (p *Plugin) describeScreen(dataURL string, provider string) string {
	if p.sdk == nil || p.sdk.LLM() == nil {
		return "LLM 不可用，无法描述屏幕内容"
	}
	llm := p.sdk.LLM()
	req := &sdk.LLMCompletionRequest{
		MaxTokens: 2048,
		Messages: []sdk.LLMMessage{{
			Role: "user",
			Blocks: []sdk.LLMContentBlock{
				{Type: "text", Text: "这是用户设备的屏幕截图。请详细描述屏幕上显示的内容：正在运行的窗口/应用、可见的文字内容、界面状态等。如果是代码编辑器或终端，尽量转述关键文字信息。"},
				{Type: "image_url", ImageURL: dataURL},
			},
		}},
	}
	// 指定源：临时切换（低频操作，用完恢复原源）
	if provider != "" {
		prev := llm.CurrentSource()
		if err := llm.SetSource(provider); err != nil {
			log.Printf("[remotedevice] screensee set source %s: %v", provider, err)
		} else if prev != "" {
			defer func() { _ = llm.SetSource(prev) }()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := llm.Chat(ctx, req)
	if err != nil {
		return fmt.Sprintf("屏幕截图视觉描述失败: %v（当前模型可能不支持图像输入）", err)
	}
	return resp.Content
}

func (p *Plugin) Stop() error {
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return p.server.Shutdown(ctx)
	}
	return nil
}

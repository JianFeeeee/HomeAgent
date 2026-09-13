package remotedevice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
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
	// 注意：不注册 authorized_devices 设置项 —— 鉴权在设备端执行（客户端存储），
	// 服务端不保存授权状态，避免 agent 经 config_set 工具自行授权。

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

	// ---- devicectl Device（agent 工具） ----------------
	p.dev = &devicectlDevice{reg: p.registry}
	// screensee 视觉描述回调：截屏回传后用视觉模型描述屏幕内容
	p.dev.SetSeeHandler(p.describeScreen)
	if err := s.RegisterChannel("devicectl", p.dev); err != nil {
		log.Printf("[remotedevice] register devicectl channel: %v", err)
	}

	// ---- 媒体落盘目录：<data>/device_media ----------------
	// 设备回传的录像/照片等二进制聚合后写入此目录，cmd_result 返回 file 路径，
	// 避免 base64 内联撑爆 LLM 上下文。目录由 logManager/运维定期清理。
	if dataDir, err := s.Settings().GetCore("daemon.data_dir"); err == nil {
		if dd, ok := dataDir.(string); ok && dd != "" {
			p.registry.SetMediaDir(filepath.Join(dd, "device_media"))
		}
	}

	// ---- 设备主动上报事件 → agent 注入 ----------------
	// 摄像头发现异常/传感器报警等场景：设备经 WS op=event 上报，
	// 插件将其格式化为文本经 SDK InjectText 异步注入 agent（source=device/{id}，
	// 回复路由回 device/{id} 通道），同时发 EventBus 供 WebUI 展示。
	// 节流：同设备同类型事件 10s 内去重，防传感器风暴。
	lastEventAt := map[string]time.Time{}
	var eventMu sync.Mutex
	p.registry.SetEventHandler(func(deviceID string, msg map[string]interface{}) {
		evtType, _ := msg["type"].(string)
		if evtType == "" {
			evtType = "unknown"
		}
		key := deviceID + "|" + evtType
		eventMu.Lock()
		if last, ok := lastEventAt[key]; ok && time.Since(last) < 10*time.Second {
			eventMu.Unlock()
			log.Printf("[remotedevice] event throttled: %s from %s", evtType, deviceID)
			return
		}
		lastEventAt[key] = time.Now()
		eventMu.Unlock()

		// 组装人类可读的事件文本（agent 可直接理解）
		detail, _ := msg["detail"].(string)
		if detail == "" {
			if d, ok := msg["payload"].(map[string]interface{}); ok {
				b, _ := json.Marshal(d)
				detail = string(b)
			}
		}
		text := fmt.Sprintf("【设备事件上报】设备 %s 触发事件 %s", deviceID, evtType)
		if detail != "" {
			text += "：" + detail
		}
		text += "。请关注此事件并按需处理（如通知用户、调用相关工具核实）。"

		log.Printf("[remotedevice] event from %s: %s", deviceID, evtType)
		if p.sdk != nil {
			// 设备通道 device/<id> 是动态的：设备首次上报时**懒登记** inputch
			// （Register 幂等），父 agent 才能把它划给驻留子。
			devCh := "device/" + deviceID
			_ = p.sdk.RegisterInputChannel(devCh, sdk.ChannelDef{})
			// 异步注入：不阻塞 WS 读循环；回复路由回 device/{id} 输出通道
			p.sdk.InjectInput(devCh, devCh, "text", map[string]interface{}{"content": text})
		}
	})

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

func (p *Plugin) registerRoutes() {
	// 设备通道（WS）
	p.mux.HandleFunc("/api/v1/device/ws", p.registry.ServeWS)
	// REST 管理面（全部需 token）
	// 注意：/api/v1/device/auth 已移除 —— 授权由设备端控制，服务端不提供授权接口。
	p.mux.HandleFunc("/api/v1/device", p.requireToken(p.handleDeviceList))
	p.mux.HandleFunc("/api/v1/device/online", p.requireToken(p.handleDeviceOnline))
	p.mux.HandleFunc("/api/v1/device/", p.requireToken(p.handleDeviceByID))
	p.mux.HandleFunc("/api/v1/device/push", p.requireToken(p.handleDevicePush))
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

package remotedevice

import (
	"bufio"
	"crypto/sha1" // RFC6455 规定 Sec-WebSocket-Accept 必须用 SHA-1，勿改
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DeviceMeta 描述一台接入了网关的设备。
// Authorized 设备自报（由客户端存储和声明），服务端仅报告不决策。
// 鉴权在设备端执行：服务端推送命令后，设备自行决定是否执行。
type DeviceMeta struct {
	DeviceID   string                 `json:"device_id"`
	Name       string                 `json:"name"`
	Kind       string                 `json:"kind"`
	Caps       []string               `json:"caps"`
	Info       map[string]interface{} `json:"info,omitempty"`
	Authorized bool                   `json:"authorized"`
	Online     bool                   `json:"online"`
	LastSeen   int64                  `json:"last_seen"`
	RemoteAddr string                 `json:"remote_addr"`
}

// wconn 表示一条活跃的 WS 连接（由网关持有）。
type wconn struct {
	deviceID string
	w        *bufio.Writer
	// wmu 序列化对该连接 bufio.Writer 的所有写。
	//
	// 两个角色会并发写同一连接：handleWS 主循环（读设备帧后的 hello_ack/
	// bind_ack/pong 回写）与 PushJSON/PushData（agent→设备的下发路径，可能
	// 来自任意 goroutine）。bufio.Writer 不是线程安全的，不加锁会在
	// WriteByte/Flush 上产生 data race（生产实测触发）。
	wmu sync.Mutex
}

// lockWrite 对 wconn 加写锁并返回 writer；调用方必须 defer unlockWrite。
// 单独写成方法而不是直接暴露字段，避免调用方绕过锁。
func (c *wconn) lockWrite() *bufio.Writer {
	c.wmu.Lock()
	return c.w
}

func (c *wconn) unlockWrite() {
	c.wmu.Unlock()
}

// Registry 是设备接入网关的注册表：管理在线连接、设备元数据。线程安全。
// 鉴权在设备端执行，服务端不存储授权状态。
type Registry struct {
	mu       sync.RWMutex
	devices  map[string]*DeviceMeta // deviceID -> meta（在线/历史）
	conns    map[string]*wconn      // deviceID -> 活跃连接（支持 push）
	onlineCh chan string

	// onOnline/onOffline：设备上下线的同步回调（见 SetPresenceHandler）。
	onOnline   func(DeviceMeta)
	onOffline  func(string)
	onStatus   func(msg map[string]interface{})
	onEvent    func(deviceID string, msg map[string]interface{})
	acceptFn   func(token string) bool
	cmdPending map[string]chan map[string]interface{} // reqID -> 结果 channel
	results    map[string]resultEntry                 // reqID -> 已留档结果
	mediaDir   string                                 // 设备回传媒体落盘目录；空则退化为 base64 内联
}

// resultEntry 保存一次 cmdrun 的结果（供 device_ctl_cmdresult 查询）。
type resultEntry struct {
	Result map[string]interface{}
	Time   time.Time
}

// ===== 能力矩阵：caps 声明 → 工具可用性 =====
// 设备 hello 时声明自身能力（caps），服务端据此校验工具调用：
// 摄像头只声明 camera 就不能被调 screensee/computeruse，避免无效下发。
// 兼容历史值：cmd/cmdrun 视为 shell 命令能力；未声明任何已知能力的设备
// （如旧版 GUI/waiter）视为全能力，保持向后兼容。
var capabilityTools = map[string][]string{
	// 屏幕显示/查看
	"screen":    {"screensue", "screensee"},
	"screensue": {"screensue"},
	"screensee": {"screensee"},
	// 鼠标键盘操控
	"computeruse": {"computeruse"},
	// 剪切板
	"clipboard":    {"clipboardsee", "clipboardsue"},
	"clipboardsee": {"clipboardsee"},
	"clipboardsue": {"clipboardsue"},
	// 摄像头（抓拍/录像）
	"camera":    {"camerasue"},
	"camerasue": {"camerasue"},
	// 音频播放
	"speaker":    {"speakeruse"},
	"speakeruse": {"speakeruse"},
}

// compatFullCaps 视为「全能力」的历史 caps 值：声明了这些的设备不参与能力裁剪。
var compatFullCaps = map[string]bool{
	"cmd": true, "cmdrun": true, "cmdresult": true,
}

// SupportsTool 判断设备是否支持某 agent 工具（基于其声明的 caps）。
// 规则：
//   - 设备未声明任何已知能力且无兼容全能力标记 → 视为全能力（旧设备兼容）
//   - 声明了任一兼容全能力标记（cmd/cmdrun 等）→ 全能力
//   - 否则严格按 capabilityTools 映射匹配
func (r *Registry) SupportsTool(deviceID, tool string) bool {
	r.mu.RLock()
	m, ok := r.devices[deviceID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return deviceSupportsTool(m.Caps, tool)
}

func deviceSupportsTool(caps []string, tool string) bool {
	hasKnown := false
	for _, c := range caps {
		if compatFullCaps[c] {
			return true // 历史全能力设备
		}
		if _, known := capabilityTools[c]; known {
			hasKnown = true
			for _, t := range capabilityTools[c] {
				if t == tool {
					return true
				}
			}
		}
	}
	return !hasKnown // 未声明任何已知能力 → 全能力兼容
}

// SetMediaDir 设置设备回传媒体的落盘目录。
// 非空时 cmd_data_end 聚合完成后写入该目录，cmd_result 返回 file 路径
// （大体积 base64 内联会撑爆 LLM 上下文与工具结果管道）；空则保持旧的内联行为。
func (r *Registry) SetMediaDir(dir string) {
	r.mu.Lock()
	r.mediaDir = dir
	r.mu.Unlock()
}

// mediaExt 按 mime/kind 推断扩展名。
func mediaExt(mime, kind string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.Contains(m, "mp4"):
		return ".mp4"
	case strings.Contains(m, "webm"):
		return ".webm"
	case strings.Contains(m, "jpeg"), strings.Contains(m, "jpg"):
		return ".jpg"
	case strings.Contains(m, "png"):
		return ".png"
	case strings.Contains(m, "wav"):
		return ".wav"
	case strings.Contains(m, "mpeg"), strings.Contains(m, "mp3"):
		return ".mp3"
	}
	k := strings.ToLower(kind)
	if strings.Contains(k, "video") {
		return ".mp4"
	}
	if strings.Contains(k, "image") || strings.Contains(k, "camera_photo") {
		return ".jpg"
	}
	return ".bin"
}

// NewRegistry 返回初始化后的设备注册表。
func NewRegistry() *Registry {
	return &Registry{
		devices:    make(map[string]*DeviceMeta),
		conns:      make(map[string]*wconn),
		onlineCh:   make(chan string, 16),
		cmdPending: make(map[string]chan map[string]interface{}),
		results:    make(map[string]resultEntry),
	}
}

// SetAcceptToken 设置绑定 token 校验函数（插件注入，来自 Settings）。
func (r *Registry) SetAcceptToken(fn func(token string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acceptFn = fn
}

// SetStatusHandler 注册设备上报状态的回调。
func (r *Registry) SetStatusHandler(h func(msg map[string]interface{})) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onStatus = h
}

// SetEventHandler 注册设备主动上报事件的回调（设备→agent 单向推送）。
// 典型场景：摄像头识别到未知人员驻留、传感器报警等，设备无需 agent 轮询即可上报。
// 回调参数：deviceID + 事件消息（含 type/payload 等）。
func (r *Registry) SetEventHandler(h func(deviceID string, msg map[string]interface{})) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onEvent = h
}

func (r *Registry) acceptBind(token string) bool {
	r.mu.RLock()
	fn := r.acceptFn
	r.mu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(token)
}

// ============ 设备查询 ============

// Online 返回设备是否在线。
func (r *Registry) Online(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.devices[id]
	return ok && m.Online
}

// Authorized 已移除：授权状态由设备端自报（DeviceMeta.Authorized），服务端不存储。

// List 返回全部设备（在线或历史）。
func (r *Registry) List() []DeviceMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DeviceMeta, 0, len(r.devices))
	for _, m := range r.devices {
		c := *m
		out = append(out, c)
	}
	return out
}

// OnlineList 返回在线的设备列表。
func (r *Registry) OnlineList() []DeviceMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []DeviceMeta
	for _, m := range r.devices {
		if m.Online {
			c := *m
			out = append(out, c)
		}
	}
	return out
}

// Get 返回单个设备。
func (r *Registry) Get(id string) (DeviceMeta, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.devices[id]
	if !ok {
		return DeviceMeta{}, false
	}
	return *m, true
}

// ============ 授权 ============
// 已移除服务端授权存储：设备在 hello/status 中自报 authorized，
// 服务端仅透传展示；实际鉴权由设备端执行（收到 cmd 后自行决定是否执行）。

// ============ 在线状态维护 ============

func (r *Registry) register(meta DeviceMeta) {
	r.mu.Lock()
	meta.Online = true
	meta.LastSeen = time.Now().Unix()
	// 保留设备自报的授权状态（客户端鉴权，服务端不覆盖）
	r.devices[meta.DeviceID] = &meta
	onOnline := r.onOnline
	r.mu.Unlock()
	// 先回调（可能注册 device-<id> 输出通道），再发变更通知。
	if onOnline != nil {
		onOnline(meta)
	}
	r.notifyChange(meta.DeviceID)
}

func (r *Registry) markOffline(id string) {
	r.mu.Lock()
	if m, ok := r.devices[id]; ok {
		m.Online = false
	}
	delete(r.conns, id)
	onOffline := r.onOffline
	r.mu.Unlock()
	if onOffline != nil {
		onOffline(id)
	}
	r.notifyChange(id)
}

// SetPresenceHandler 注册设备上线/下线回调。
//
// 为什么不用 ChangeChan：那是 `select { case ch <- id: default: }`，缓冲满了会**丢事件**
// （设备上下线是要跟"注册/注销输出通道"绑定的，丢一次就会留下一个死通道或漏注册）。
// 这里同步调用，且在**释放锁之后**调 —— 回调内部会回查 registry（Get/List），
// 持锁调用会自己锁死自己。
func (r *Registry) SetPresenceHandler(onOnline func(DeviceMeta), onOffline func(string)) {
	r.mu.Lock()
	r.onOnline = onOnline
	r.onOffline = onOffline
	r.mu.Unlock()
}

func (r *Registry) notifyChange(id string) {
	select {
	case r.onlineCh <- id:
	default:
	}
}

// ChangeChan 返回设备上下线变更通知。
func (r *Registry) ChangeChan() <-chan string { return r.onlineCh }

// SaveResult 保存一次命令执行结果（供 cmdresult 查询）。
func (r *Registry) SaveResult(reqID string, res map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = make(map[string]resultEntry)
	}
	r.results[reqID] = resultEntry{Result: res, Time: time.Now()}
}

// GetResult 返回某次命令执行的结果。
func (r *Registry) GetResult(reqID string) (map[string]interface{}, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.results[reqID]
	if !ok {
		return nil, false
	}
	return e.Result, true
}

// ============ Push（agent -> 设备） ============

// PushJSON 向在线设备推送一条 JSON 消息。
func (r *Registry) PushJSON(deviceID string, payload map[string]interface{}) error {
	r.mu.RLock()
	c, ok := r.conns[deviceID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("device %s not online", deviceID)
	}
	w := c.lockWrite()
	defer c.unlockWrite()
	return writeText(w, mustJSON(payload))
}

// PushCmd 向设备发送命令执行请求。
func (r *Registry) PushCmd(deviceID, reqID, command, cmdType string) error {
	if cmdType == "" {
		cmdType = "shell"
	}
	return r.PushJSON(deviceID, map[string]interface{}{
		"op":       "cmd",
		"req_id":   reqID,
		"command":  command,
		"cmd_type": cmdType,
	})
}

// PushData 向设备分块下发二进制数据（网关→设备，如 TTS 音频）。
// 协议（与 GUI 设备桥协商）：
//
//	文本帧 cmd_speech_start {op, req_id, kind, mime, total} → N 个二进制帧(0x2, ≤8KB) → 文本帧 cmd_speech_end {op, req_id}
//
// kind 为语义标记（如 speech），mime 为数据 MIME 类型。设备聚合后按自身能力处理（播放等）。
func (r *Registry) PushData(deviceID, reqID, kind, mime string, data []byte) error {
	r.mu.RLock()
	c, ok := r.conns[deviceID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("device %s not online", deviceID)
	}
	// 整条下发（start + N 个 chunk + end）持锁：设备侧按协议串行聚合，
	// 若中途被 handleWS 的 hello/pong 插帧会破坏协议顺序。
	w := c.lockWrite()
	defer c.unlockWrite()
	if err := writeText(w, mustJSON(map[string]interface{}{
		"op":     "cmd_speech_start",
		"req_id": reqID,
		"kind":   kind,
		"mime":   mime,
		"total":  len(data),
	})); err != nil {
		return fmt.Errorf("push data start: %w", err)
	}
	const chunkSize = 8192
	for off := 0; off < len(data); off += chunkSize {
		end := off + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := writeBinary(w, data[off:end]); err != nil {
			return fmt.Errorf("push data chunk: %w", err)
		}
	}
	if err := writeText(w, mustJSON(map[string]interface{}{
		"op":     "cmd_speech_end",
		"req_id": reqID,
	})); err != nil {
		return fmt.Errorf("push data end: %w", err)
	}
	return nil
}

// AwaitResult 等待某请求的结果（带超时）。快速回执会先留在 results，
// 因而 PushCmd 后才开始等待也不会丢失。
func (r *Registry) AwaitResult(reqID string, timeout time.Duration) (map[string]interface{}, error) {
	ch := make(chan map[string]interface{}, 1)
	r.mu.Lock()
	if e, ok := r.results[reqID]; ok {
		r.mu.Unlock()
		return e.Result, nil
	}
	r.cmdPending[reqID] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cmdPending, reqID)
		r.mu.Unlock()
	}()
	select {
	case res := <-ch:
		return res, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for device result")
	}
}

// deliverResult 先留档再通知等待者，消除设备极速回执早于 AwaitResult 的竞态。
func (r *Registry) deliverResult(reqID string, res map[string]interface{}) {
	r.mu.Lock()
	if r.results == nil {
		r.results = make(map[string]resultEntry)
	}
	r.results[reqID] = resultEntry{Result: res, Time: time.Now()}
	ch, ok := r.cmdPending[reqID]
	r.mu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// ============ WS 网关（标准库 Hijacker，RFC6455 子集） ============

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsAccept 计算 RFC6455 §4.2.2 握手响应值：base64(SHA1(key + GUID))。
// 注意必须用 SHA-1——这是协议规定而非安全选择；此前误用 SHA-256 导致
// 所有标准 WS 客户端（浏览器/Electron/各语言标准库）校验 Accept 失败
// 后立即断开，设备永远无法完成 hello 注册（devicedetect 恒为空）。
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerListHas(h, sub string) bool {
	for _, part := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(part), sub) {
			return true
		}
	}
	return false
}

func httpUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	if !headerListHas(r.Header.Get("Upgrade"), "websocket") ||
		!headerListHas(r.Header.Get("Connection"), "upgrade") {
		return nil, nil, fmt.Errorf("not a websocket upgrade request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijack not supported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	accept := wsAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols" + "\x0d\x0a" +
		"Upgrade: websocket\x0d\x0aConnection: Upgrade\x0d\x0a" +
		"Sec-WebSocket-Accept: " + accept + "\x0d\x0a\x0d\x0a"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}

// readFrame 读取一个 WS 帧。返回 (payload, isClose, err)。
// opcode: 0x1 文本 / 0x2 二进制（设备→网关大体积数据分块，如录像回传）。
func readFrame(r *bufio.Reader) ([]byte, bool, byte, error) {
	b0, err := r.ReadByte()
	if err != nil {
		return nil, true, 0, err
	}
	opcode := b0 & 0x0f
	b1, err := r.ReadByte()
	if err != nil {
		return nil, true, opcode, err
	}
	masked := b1&0x80 != 0
	length := uint64(b1 & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, true, opcode, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, true, opcode, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// 二进制帧允许更大（录像分块聚合，单帧仍限 8MB 防滥用）
	maxFrame := uint64(1 << 20)
	if opcode == 0x2 {
		maxFrame = 8 << 20
	}
	if length > maxFrame {
		return nil, true, opcode, fmt.Errorf("frame too large")
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return nil, true, opcode, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, true, opcode, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	switch opcode {
	case 0x1, 0x2:
		return payload, false, opcode, nil
	case 0x8:
		return nil, true, opcode, nil
	case 0xa:
		return nil, false, opcode, nil
	case 0x9:
		return nil, false, opcode, errPing
	default:
		return nil, false, opcode, fmt.Errorf("unsupported opcode %x", opcode)
	}
}

var errPing = fmt.Errorf("ping")

func writeText(w *bufio.Writer, payload []byte) error {
	return writeFrame(w, 0x1, payload)
}

// writeBinary 发送 WS 二进制帧（0x2）：网关→设备大体积数据（如 TTS 音频）分块下发。
func writeBinary(w *bufio.Writer, payload []byte) error {
	return writeFrame(w, 0x2, payload)
}

func writeFrame(w *bufio.Writer, opcode byte, payload []byte) error {
	if err := writeFrameHeader(w, opcode, len(payload)); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

func writePong(w *bufio.Writer) error {
	// 必须走 writeFrame（它 Flush）。
	//
	// 回归的 bug：这里原先是裸的 writeFrameHeader，**不 Flush**。设备空闲时
	// 没有任何别的写会顺带把 bufio 缓冲刷出去，于是 pong 永远留在服务端缓冲里，
	// 客户端等 2 倍 ping 间隔（默认 30s×2 = 60s）读超时断开、重连——
	// 实测表现就是「设备通道每 60 秒掉线一次」，连带着 outputch 反复注销/注册。
	return writeFrame(w, 0xa, nil)
}

func writeFrameHeader(w *bufio.Writer, opcode byte, length int) error {
	if err := w.WriteByte(0x80 | opcode); err != nil {
		return err
	}
	if length < 126 {
		if err := w.WriteByte(byte(length)); err != nil {
			return err
		}
	} else if length <= 0xffff {
		if err := w.WriteByte(126); err != nil {
			return err
		}
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(length))
		if _, err := w.Write(ext[:]); err != nil {
			return err
		}
	} else {
		if err := w.WriteByte(127); err != nil {
			return err
		}
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(length))
		if _, err := w.Write(ext[:]); err != nil {
			return err
		}
	}
	return nil
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// ServeWS 是 WS 端点的 HTTP handler：认证 token（query 或 Sec-WebSocket-Protocol），
// 升级后进入 handleWS。未带 token 也允许 hello（登记设备），bind 时校验。
func (r *Registry) ServeWS(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := req.URL.Query().Get("token")
	if token == "" {
		token = strings.TrimSpace(req.Header.Get("X-API-Key"))
	}
	if token == "" {
		for _, p := range req.Header.Values("Sec-WebSocket-Protocol") {
			if strings.HasPrefix(p, "homeagent.") {
				token = strings.TrimPrefix(p, "homeagent.")
				break
			}
		}
	}
	handshakeAuthorized := token != "" && r.acceptBind(token)
	if token != "" && !handshakeAuthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, rw, err := httpUpgrade(w, req)
	if err != nil {
		http.Error(w, "upgrade failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[remotedevice] ws connected from %s", conn.RemoteAddr())
	go r.handleWS(conn, rw, handshakeAuthorized)
}

// wsWriteLocked 在指定设备连接的写锁保护下执行写回调。
//
// handleWS 主循环与 Push* 是两条并发写同一 bufio.Writer 的路径，
// 必须共用同一把锁。handleWS 里拿到的是 rw.Writer（与 conns 存储的是
// 同一个对象），回写前必须经此函数取锁，否则跟 Push* 依然会撞。
//
// 注意设备已离线（conns 中已删除）时直接报错——设备断开后仍尝试
// 回写没有意义，还可能在已关闭的 bufio 上写入。
func (r *Registry) wsWriteLocked(deviceID string, fn func(w *bufio.Writer) error) error {
	r.mu.RLock()
	c, ok := r.conns[deviceID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("device %s not online", deviceID)
	}
	w := c.lockWrite()
	defer c.unlockWrite()
	return fn(w)
}

func (r *Registry) handleWS(conn net.Conn, rw *bufio.ReadWriter, handshakeAuthorized bool) {
	defer conn.Close()
	var curID string
	var pendingMeta *DeviceMeta
	var bound bool
	defer func() {
		if curID != "" {
			if bound {
				r.markOffline(curID)
			} else {
				r.mu.Lock()
				delete(r.conns, curID)
				r.mu.Unlock()
			}
		}
	}()

	// 二进制分块聚合状态（设备→网关，如录像回传）：
	// cmd_data_start 开启 → 0x2 帧追加 → cmd_data_end 聚合存入 cmdresult
	var dataAccum *dataAccumulator

	for {
		payload, isClose, opcode, err := readFrame(rw.Reader)
		if err != nil {
			if err == errPing {
				// ★ 回 pong 绝不能因为「还没 bind」而失败。
				//
				// 原实现无条件走 wsWriteLocked(curID, writePong)，而 conns 表
				// **只在 bind 成功后才写入**（bind 之前刻意不把连接暴露给查询/
				// 命令路径）。于是握手完成、bind 尚未到达时来的 ping 找不到写
				// 入口 → 返回错误 → 读循环 return → **连接被关掉**。
				//
				// 生产后果（日志实证）：客户端每 30s 一次 ping，只要有一次落在
				// 未 bind 窗口就断连，表现为同一设备 20 秒内多次 ws connected、
				// online/offline 反复交替，输出通道跟着反复注销/注册。
				//
				// 正确做法：pong 直接写本连接的 writer。此时该连接**尚未**进入
				// conns（即没有 Push* 会碰它的 writer），不存在并发写风险；
				// 已 bind 时才需要取写锁（Push* 可能正在写同一 buffer）。
				var werr error
				if bound {
					werr = r.wsWriteLocked(curID, writePong)
				} else {
					werr = writePong(rw.Writer)
				}
				if werr != nil {
					return
				}
				continue
			}
			return
		}
		if isClose {
			return
		}
		if opcode == 0x2 {
			// 二进制帧：处于聚合状态时追加数据块，否则忽略
			if dataAccum != nil {
				dataAccum.chunks = append(dataAccum.chunks, payload)
				dataAccum.got += len(payload)
				// 防滥用：超出声明 total 的 2 倍或硬上限 64MB 时放弃聚合
				limit := int64(64 << 20)
				if dataAccum.total > 0 {
					declaredLimit := int64(dataAccum.total)*2 + 1024
					if declaredLimit < limit {
						limit = declaredLimit
					}
				}
				if int64(dataAccum.got) > limit {
					log.Printf("[remotedevice] data accumulation exceeded limit for req %s, dropped", dataAccum.reqID)
					dataAccum = nil
				}
			}
			continue
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		op, _ := msg["op"].(string)
		if !bound && op != "hello" && op != "bind" {
			continue
		}
		switch op {
		case "hello":
			meta := metaFromMsg(msg)
			if meta.DeviceID == "" {
				continue
			}
			meta.RemoteAddr = conn.RemoteAddr().String()
			pendingMeta = &meta
			curID = meta.DeviceID
			// Bind 前不把连接暴露给查询或命令下发路径；此时只有当前读循环会写。
			if err := writeText(rw.Writer, mustJSON(map[string]interface{}{
				"op":     "hello_ack",
				"device": meta.DeviceID,
				"online": false,
			})); err != nil {
				return
			}
		case "bind":
			token, _ := msg["token"].(string)
			id, _ := msg["device_id"].(string)
			bindAuthorized := handshakeAuthorized || r.acceptBind(token)
			if pendingMeta == nil || id == "" || id != pendingMeta.DeviceID || !bindAuthorized {
				_ = writeText(rw.Writer, mustJSON(map[string]interface{}{
					"op": "bind_ack", "ok": false, "error": "bind rejected",
				}))
				return
			}
			r.mu.Lock()
			r.conns[id] = &wconn{deviceID: id, w: rw.Writer}
			r.mu.Unlock()
			bound = true
			r.register(*pendingMeta)
			if err := r.wsWriteLocked(curID, func(w *bufio.Writer) error {
				return writeText(w, mustJSON(map[string]interface{}{"op": "bind_ack", "ok": true}))
			}); err != nil {
				return
			}
		case "status":
			id, _ := msg["device_id"].(string)
			r.mu.Lock()
			if m, ok := r.devices[id]; ok {
				m.LastSeen = time.Now().Unix()
			}
			r.mu.Unlock()
			r.mu.RLock()
			h := r.onStatus
			r.mu.RUnlock()
			if h != nil {
				h(msg)
			}
		case "event":
			// 设备主动上报事件（单向推送，无需回执）：摄像头发现异常、传感器报警等。
			// 转交插件层（经 SDK InjectText 异步注入 agent），无回调时仅记日志。
			id, _ := msg["device_id"].(string)
			if id == "" {
				id = curID
			}
			if id == "" {
				continue
			}
			r.mu.RLock()
			h := r.onEvent
			r.mu.RUnlock()
			if h != nil {
				h(id, msg)
			} else {
				log.Printf("[remotedevice] event from %s (no handler): %v", id, msg)
			}
		case "cmd_result":
			reqID, _ := msg["req_id"].(string)
			if reqID != "" {
				r.deliverResult(reqID, msg)
			}
		case "cmd_data_start":
			reqID, _ := msg["req_id"].(string)
			if reqID == "" {
				continue
			}
			total, _ := msg["total"].(float64)
			kind, _ := msg["kind"].(string)
			mime, _ := msg["mime"].(string)
			dataAccum = &dataAccumulator{
				reqID: reqID,
				kind:  kind,
				mime:  mime,
				total: int(total),
			}
		case "cmd_data_end":
			reqID, _ := msg["req_id"].(string)
			status, _ := msg["status"].(string)
			if dataAccum == nil || dataAccum.reqID != reqID {
				continue
			}
			acc := dataAccum
			dataAccum = nil
			if status != "ok" {
				r.SaveResult(reqID, map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "status": "error",
					"error": "device reported transfer failure",
				})
				r.deliverResult(reqID, map[string]interface{}{
					"op": "cmd_result", "req_id": reqID, "status": "error",
					"error": "device reported transfer failure",
				})
				continue
			}
			data := make([]byte, 0, acc.got)
			for _, c := range acc.chunks {
				data = append(data, c...)
			}
			res := map[string]interface{}{
				"op":       "cmd_result",
				"req_id":   reqID,
				"status":   "ok",
				"kind":     acc.kind,
				"mime":     acc.mime,
				"size":     len(data),
				"expected": acc.total,
			}
			// 媒体落盘模式：写入 <mediaDir>/<reqID>.<ext>，cmd_result 返回 file 路径。
			// 大体积 base64 内联会撑爆 LLM 上下文（一段 10s 录像即数 MB），
			// agent 应拿路径后用 files/describe_image/ocr 等工具消费。
			r.mu.RLock()
			mediaDir := r.mediaDir
			r.mu.RUnlock()
			if mediaDir != "" {
				if err := os.MkdirAll(mediaDir, 0755); err == nil {
					fp := filepath.Join(mediaDir, reqID+mediaExt(acc.mime, acc.kind))
					if werr := os.WriteFile(fp, data, 0644); werr == nil {
						res["file"] = fp
					} else {
						log.Printf("[remotedevice] media write %s: %v", fp, werr)
					}
				} else {
					log.Printf("[remotedevice] media dir %s: %v", mediaDir, err)
				}
			}
			// 未配置落盘目录时保持旧行为：base64 内联返回（小体积数据仍可用）
			if _, hasFile := res["file"]; !hasFile {
				res["data_base64"] = base64.StdEncoding.EncodeToString(data)
			}
			r.SaveResult(reqID, res)
			r.deliverResult(reqID, res)
		}
	}
}

// dataAccumulator 聚合设备→网关的二进制分块传输（如录像回传）。
type dataAccumulator struct {
	reqID  string
	kind   string
	mime   string
	total  int
	chunks [][]byte
	got    int
}

func metaFromMsg(msg map[string]interface{}) DeviceMeta {
	var meta DeviceMeta
	if d, ok := msg["device"].(map[string]interface{}); ok {
		if v, ok := d["device_id"].(string); ok {
			meta.DeviceID = v
		}
		if v, ok := d["name"].(string); ok {
			meta.Name = v
		}
		if v, ok := d["kind"].(string); ok {
			meta.Kind = v
		}
		if caps, ok := d["caps"].([]interface{}); ok {
			for _, c := range caps {
				if s, ok := c.(string); ok {
					meta.Caps = append(meta.Caps, s)
				}
			}
		}
		if v, ok := d["authorized"].(bool); ok {
			meta.Authorized = v
		}
		if info, ok := d["info"].(map[string]interface{}); ok {
			if len(info) > 0 {
				meta.Info = info
			}
		}
	}
	return meta
}

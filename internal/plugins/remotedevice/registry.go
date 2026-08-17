package remotedevice

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DeviceMeta 描述一台接入了网关的设备。
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
}

// Registry 是设备接入网关的注册表：管理在线连接、设备元数据与已授权集合。线程安全。
type Registry struct {
	mu         sync.RWMutex
	devices    map[string]*DeviceMeta // deviceID -> meta（在线/历史）
	authorized map[string]bool        // deviceID -> 是否已授权（持久化恢复）
	conns      map[string]*wconn      // deviceID -> 活跃连接（支持 push）
	onlineCh   chan string
	onStatus   func(msg map[string]interface{})
	acceptFn   func(token string) bool
	cmdPending map[string]chan map[string]interface{} // reqID -> 结果 channel
	results    map[string]resultEntry                 // reqID -> 已留档结果
}

// resultEntry 保存一次 cmdrun 的结果（供 device_ctl_cmdresult 查询）。
type resultEntry struct {
	Result map[string]interface{}
	Time   time.Time
}

func NewRegistry() *Registry {
	return &Registry{
		devices:    make(map[string]*DeviceMeta),
		authorized: make(map[string]bool),
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

// Authorized 返回设备是否已授权。
func (r *Registry) Authorized(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.authorized[id]
}

// List 返回全部设备（在线或历史），合并授权态。
func (r *Registry) List() []DeviceMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DeviceMeta, 0, len(r.devices))
	for _, m := range r.devices {
		c := *m
		c.Authorized = r.authorized[c.DeviceID]
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
			c.Authorized = r.authorized[c.DeviceID]
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
	c := *m
	c.Authorized = r.authorized[id]
	return c, true
}

// ============ 授权 ============

// SetAuthorized 标记某设备已授权/取消授权（持久化由插件负责）。
func (r *Registry) SetAuthorized(id string, auth bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorized[id] = auth
	if m, ok := r.devices[id]; ok {
		m.Authorized = auth
	}
}

// RestoreAuthorized 插件启动时从配置恢复已授权设备集合。
func (r *Registry) RestoreAuthorized(ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.authorized[id] = true
	}
}

// AuthorizedIDs 返回全部已授权设备 ID（供插件持久化）。
func (r *Registry) AuthorizedIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for id, ok := range r.authorized {
		if ok {
			out = append(out, id)
		}
	}
	return out
}

// ============ 在线状态维护 ============

func (r *Registry) register(meta DeviceMeta) {
	r.mu.Lock()
	meta.Online = true
	meta.LastSeen = time.Now().Unix()
	meta.Authorized = r.authorized[meta.DeviceID]
	r.devices[meta.DeviceID] = &meta
	r.mu.Unlock()
	r.notifyChange(meta.DeviceID)
}

func (r *Registry) markOffline(id string) {
	r.mu.Lock()
	if m, ok := r.devices[id]; ok {
		m.Online = false
	}
	delete(r.conns, id)
	r.mu.Unlock()
	r.notifyChange(id)
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
	return writeText(c.w, mustJSON(payload))
}

// PushCmd 向设备发送命令执行请求。
func (r *Registry) PushCmd(deviceID, reqID, command string) error {
	return r.PushJSON(deviceID, map[string]interface{}{
		"op":      "cmd",
		"req_id":  reqID,
		"command": command,
	})
}

// AwaitResult 等待某请求的结果（带超时）。
func (r *Registry) AwaitResult(reqID string, timeout time.Duration) (map[string]interface{}, error) {
	ch := make(chan map[string]interface{}, 1)
	r.mu.Lock()
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

// deliverResult 设备回执结果时由 handleWS 调用。
func (r *Registry) deliverResult(reqID string, res map[string]interface{}) {
	r.mu.RLock()
	ch, ok := r.cmdPending[reqID]
	r.mu.RUnlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// ============ WS 网关（标准库 Hijacker，RFC6455 子集） ============

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	h := sha256.Sum256([]byte(key + wsGUID))
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

func readFrame(r *bufio.Reader) ([]byte, bool, error) {
	b0, err := r.ReadByte()
	if err != nil {
		return nil, true, err
	}
	opcode := b0 & 0x0f
	b1, err := r.ReadByte()
	if err != nil {
		return nil, true, err
	}
	masked := b1&0x80 != 0
	length := uint64(b1 & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, true, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, true, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 1<<20 {
		return nil, true, fmt.Errorf("frame too large")
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return nil, true, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, true, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	switch opcode {
	case 0x1:
		return payload, false, nil
	case 0x8:
		return nil, true, nil
	case 0xa:
		return nil, false, nil
	case 0x9:
		return nil, false, errPing
	default:
		return nil, false, fmt.Errorf("unsupported opcode %x", opcode)
	}
}

var errPing = fmt.Errorf("ping")

func writeText(w *bufio.Writer, payload []byte) error {
	if err := writeFrameHeader(w, 0x1, len(payload)); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

func writePong(w *bufio.Writer) error {
	return writeFrameHeader(w, 0xa, 0)
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
		for _, p := range req.Header.Values("Sec-WebSocket-Protocol") {
			if strings.HasPrefix(p, "homeagent.") {
				token = strings.TrimPrefix(p, "homeagent.")
				break
			}
		}
	}
	if token != "" && !r.acceptBind(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, rw, err := httpUpgrade(w, req)
	if err != nil {
		http.Error(w, "upgrade failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[remotedevice] ws connected from %s", conn.RemoteAddr())
	go r.handleWS(conn, rw)
}

func (r *Registry) handleWS(conn net.Conn, rw *bufio.ReadWriter) {
	defer conn.Close()
	var curID string
	defer func() {
		if curID != "" {
			r.markOffline(curID)
		}
	}()

	for {
		payload, isClose, err := readFrame(rw.Reader)
		if err != nil {
			if err == errPing {
				if werr := writePong(rw.Writer); werr != nil {
					return
				}
				continue
			}
			return
		}
		if isClose {
			return
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		op, _ := msg["op"].(string)
		switch op {
		case "hello":
			meta := metaFromMsg(msg)
			if meta.DeviceID == "" {
				continue
			}
			meta.RemoteAddr = conn.RemoteAddr().String()
			curID = meta.DeviceID
			r.register(meta)
			r.mu.Lock()
			r.conns[meta.DeviceID] = &wconn{deviceID: meta.DeviceID, w: rw.Writer}
			r.mu.Unlock()
			if err := writeText(rw.Writer, mustJSON(map[string]interface{}{
				"op":     "hello_ack",
				"device": meta.DeviceID,
				"online": true,
			})); err != nil {
				return
			}
		case "bind":
			token, _ := msg["token"].(string)
			if r.acceptBind(token) {
				id, _ := msg["device_id"].(string)
				if id != "" {
					// 默认不授权：bind 仅验证 token + 登记设备；授权完全由用户手动
					// （GUI 设备页 / REST /api/v1/device/auth）控制，绝不自动授权。
				}
				if err := writeText(rw.Writer, mustJSON(map[string]interface{}{"op": "bind_ack", "ok": true})); err != nil {
					return
				}
			} else {
				if err := writeText(rw.Writer, mustJSON(map[string]interface{}{"op": "bind_ack", "ok": false, "error": "bad token"})); err != nil {
					return
				}
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
		case "cmd_result":
			reqID, _ := msg["req_id"].(string)
			if reqID != "" {
				r.deliverResult(reqID, msg)
			}
		}
	}
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
		if info, ok := d["info"].(map[string]interface{}); ok {
			if len(info) > 0 {
				meta.Info = info
			}
		}
	}
	return meta
}

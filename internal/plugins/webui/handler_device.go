package webui

import (
	"bufio"
	"io"
	"net"
	"strings"
	"time"

	"net/http"
)

// 设备网关反代：/api/v1/device/* → remotedevice（HTTP + WS 升级透传）。

// ======== Remote Device Gateway (proxied to remotedevice, opt-in) ========

// deviceGatewayEnabled / deviceGatewayAddr 由 webui 插件启动时从设置读取并注入。
// 默认禁用：用户显式配置 device_gateway_enabled=true 后，/api/v1/device/* 才会反代到
// remotedevice 插件（self-contained），避免与 remotedevice 耦合。
var (
	deviceGatewayEnabled bool
	deviceGatewayAddr    string
	deviceGatewayToken   string
)

// handleDeviceGatewayProxy 将 /api/v1/device/* 反代到 remotedevice 内部 HTTP 服务。
// 鉴权：本端走 requireAPI（webui API key），转发时带 remotedevice 的 token（X-API-Key）。
// WS 升级请求（Upgrade: websocket）走 hijack 双向字节透传（标准库 http.Client 不支持 101 升级）。
func (h *Handler) handleDeviceGatewayProxy(w http.ResponseWriter, r *http.Request) {
	if !deviceGatewayEnabled {
		http.NotFound(w, r)
		return
	}
	// 客户端鉴权模式：服务端不再提供授权接口（授权由设备端本地控制）。
	// 拒绝旧的 /device/auth 调用，避免误导。
	if strings.HasSuffix(r.URL.Path, "/device/auth") {
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "device authorization moved to client-side; the server no longer stores authorization state",
		})
		return
	}
	addr := deviceGatewayAddr
	if addr == "" {
		addr = "127.0.0.1:9890"
	}
	path := r.URL.Path // 保留 /api/v1/device/... 全路径
	url := "http://" + addr + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	// WebSocket 升级：hijack 双向透传（支持 WS over 远程 homed）
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.proxyWebSocket(w, r, addr, path)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header = r.Header.Clone()
	if deviceGatewayToken != "" {
		req.Header.Set("X-API-Key", deviceGatewayToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "device gateway unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyWebSocket 用 TCP 直连 + hijack 将客户端 WS 连接双向透传到设备网关。
func (h *Handler) proxyWebSocket(w http.ResponseWriter, r *http.Request, addr, path string) {
	// 设备网关默认仅监听 127.0.0.1（remotedevice），反代目标即内网 homed 本机或指定 addr。
	// 用 net.Dial 直连网关并手动发起 WS 升级握手（net/http 客户端不支持 ws:// 升级）。
	host, port := addr, "9890"
	if h2, p2, ok := splitHostPort(addr); ok {
		host, port = h2, p2
	}
	target := net.JoinHostPort(host, port)
	upConn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		http.Error(w, "ws upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// 手动构造 WS 升级请求（保留客户端头 + 注入网关 token）
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		key = "homeagent-proxy-random-key"
	}
	reqPath := path
	if r.URL.RawQuery != "" {
		reqPath += "?" + r.URL.RawQuery
	}
	var b strings.Builder
	b.WriteString("GET " + reqPath + " HTTP/1.1\r\n")
	b.WriteString("Host: " + addr + "\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	if deviceGatewayToken != "" {
		b.WriteString("X-API-Key: " + deviceGatewayToken + "\r\n")
	}
	for k, vv := range r.Header {
		kl := strings.ToLower(k)
		if kl == "upgrade" || kl == "connection" || kl == "sec-websocket-key" || kl == "sec-websocket-version" || kl == "host" || kl == "x-api-key" || kl == "authorization" {
			continue
		}
		for _, v := range vv {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	if _, err := upConn.Write([]byte(b.String())); err != nil {
		http.Error(w, "ws upstream write: "+err.Error(), http.StatusBadGateway)
		return
	}

	// 读上游 101 响应
	br := bufio.NewReader(upConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		http.Error(w, "ws upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		http.Error(w, "ws upstream status: "+resp.Status, http.StatusBadGateway)
		return
	}

	// 客户端 hijack：把 101 响应头写给客户端并接管双向连接
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// 向上游 101 响应头转发给客户端
	if err := resp.Write(brw); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	// 双向透传（WS 帧字节不动）：
	// 客户端 -> 上游
	errCh := make(chan struct{}, 2)
	go func() {
		io.Copy(upConn, brw)
		if tc, ok := upConn.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		errCh <- struct{}{}
	}()
	// 上游 -> 客户端
	go func() {
		wb := bufio.NewWriter(clientConn)
		io.Copy(wb, br)
		wb.Flush()
		errCh <- struct{}{}
	}()
	<-errCh
}

// splitHostPort 拆分 addr 为 host/port；无端口时返回 ok=false。
func splitHostPort(addr string) (string, string, bool) {
	if strings.Contains(addr, ":") {
		h, p, err := net.SplitHostPort(addr)
		if err == nil {
			return h, p, true
		}
	}
	return addr, "", false
}

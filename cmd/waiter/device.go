package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type deviceBridge struct {
	addr     string
	token    string
	deviceID string
	conn     net.Conn
	mu       sync.Mutex
	stop     chan struct{}
}

func newDeviceBridge(addr, token string) *deviceBridge {
	return &deviceBridge{addr: addr, token: token, stop: make(chan struct{})}
}

func (b *deviceBridge) Start() error {
	if b.addr == "" || b.token == "" {
		return fmt.Errorf("device bridge: addr/token required")
	}
	host, port := b.addr, "9890"
	if h, p, err := net.SplitHostPort(b.addr); err == nil {
		host, port = h, p
	} else if idx := strings.LastIndex(b.addr, ":"); idx >= 0 {
		host = strings.TrimPrefix(b.addr[:idx], "http://")
		port = b.addr[idx+1:]
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	b.deviceID = "waiter-" + sanitizeID(hostname)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		return err
	}
	b.conn = conn

	key := wsKey()
	path := "/api/v1/device/ws?token=" + urlEscape(b.token)
	var sb strings.Builder
	sb.WriteString("GET " + path + " HTTP/1.1" + CRLF)
	sb.WriteString("Host: " + host + ":" + port + CRLF)
	sb.WriteString("Upgrade: websocket" + CRLF)
	sb.WriteString("Connection: Upgrade" + CRLF)
	sb.WriteString("Sec-WebSocket-Key: " + key + CRLF)
	sb.WriteString("Sec-WebSocket-Version: 13" + CRLF + CRLF)
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		conn.Close()
		return err
	}
	br := bufio.NewReader(conn)
	headerBuf := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return err
		}
		headerBuf += line
		if strings.Contains(headerBuf, CRLF+CRLF) {
			break
		}
	}
	if !strings.Contains(headerBuf, " 101 ") {
		conn.Close()
		return fmt.Errorf("ws upgrade failed: %s", firstLine(headerBuf))
	}

	b.sendJSON(helloMsg(b.deviceID))
	b.sendJSON(map[string]interface{}{"op": "bind", "device_id": b.deviceID, "token": b.token})

	go b.readLoop(br)
	return nil
}

// CRLF 用常量避免转义地狱
const CRLF = "\r\n"

func helloMsg(deviceID string) map[string]interface{} {
	hostname, _ := os.Hostname()
	return map[string]interface{}{
		"op": "hello",
		"device": map[string]interface{}{
			"device_id": deviceID,
			"name":      "HomeAgent CLI (waiter)",
			"kind":      "computer",
			"caps":      []string{"status", "cmdrun", "deviceinfo"},
			"info": map[string]interface{}{
				"hostname": hostname,
				"platform": runtime.GOOS,
				"arch":     runtime.GOARCH,
				"cpus":     runtime.NumCPU(),
				"mem_mb":   memTotalMB(),
			},
		},
	}
}

func memTotalMB() int64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				var kb int64
				fmt.Sscanf(f[1], "%d", &kb)
				return kb / 1024
			}
		}
	}
	return 0
}

func (b *deviceBridge) sendJSON(v interface{}) {
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return
	}
	writeWSFrame(b.conn, 0x1, payload)
}

func writeWSFrame(conn net.Conn, opcode byte, payload []byte) {
	length := len(payload)
	hdr := []byte{0x80 | opcode}
	switch {
	case length < 126:
		hdr = append(hdr, 0x80|byte(length))
	case length <= 0xffff:
		hdr = append(hdr, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(hdr[len(hdr)-2:], uint16(length))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[len(hdr)-8:], uint64(length))
	}
	var mask [4]byte
	rand.Read(mask[:])
	masked := make([]byte, length)
	for i := 0; i < length; i++ {
		masked[i] = payload[i] ^ mask[i&3]
	}
	conn.Write(append(append(hdr, mask[:]...), masked...))
}

func (b *deviceBridge) readLoop(br *bufio.Reader) {
	for {
		select {
		case <-b.stop:
			return
		default:
		}
		payload, err := readWSFrame(br)
		if err != nil {
			return
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg["op"] != "cmd" {
			continue
		}
		reqID, _ := msg["req_id"].(string)
		command, _ := msg["command"].(string)
		if reqID == "" {
			continue
		}
		go b.runCommand(reqID, command)
	}
}

func readWSFrame(br *bufio.Reader) ([]byte, error) {
	b0, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	opcode := b0 & 0x0f
	b1, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	length := uint64(b1 & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 1<<20 {
		return nil, fmt.Errorf("frame too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	switch opcode {
	case 0x1:
		return payload, nil
	case 0x8:
		return nil, io.EOF
	default:
		return nil, nil
	}
}

var waiterAllowCmd = regexp.MustCompile("^(ls|pwd|whoami|uname|date|echo|uptime|hostname|cat|df|free|ps|ip|dir|node|python3?|npm|git|curl|wget|systeminfo|tasklist)\\b")

func (b *deviceBridge) runCommand(reqID, command string) {
	res := b.execCommand(command)
	res["op"] = "cmd_result"
	res["req_id"] = reqID
	res["device_id"] = b.deviceID
	b.sendJSON(res)
}

func (b *deviceBridge) execCommand(cmdLine string) map[string]interface{} {
	if strings.TrimSpace(cmdLine) == "" {
		return map[string]interface{}{"status": "error", "output": "empty command", "exit_code": -1}
	}
	if !waiterAllowCmd.MatchString(strings.TrimSpace(cmdLine)) {
		return map[string]interface{}{"status": "error", "output": "command not in whitelist", "exit_code": -1}
	}
	parts := strings.Fields(strings.TrimSpace(cmdLine))
	if len(parts) == 0 {
		return map[string]interface{}{"status": "error", "output": "empty", "exit_code": -1}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return map[string]interface{}{"status": "error", "output": "timeout", "exit_code": -2}
		}
		return map[string]interface{}{"status": "error", "output": truncate8k(string(out)) + " err: " + err.Error(), "exit_code": -1}
	}
	return map[string]interface{}{"status": "ok", "output": truncate8k(string(out)), "exit_code": 0}
}

const devMaxOut = 8192

func truncate8k(s string) string {
	if len(s) <= devMaxOut {
		return s
	}
	return s[:devMaxOut]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func sanitizeID(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func urlEscape(s string) string {
	var sb strings.Builder
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0xf])
		}
	}
	return sb.String()
}

func wsKey() string {
	var b [16]byte
	rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

func (b *deviceBridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
}

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// acpPlugin 实现 Agent Client Protocol (ACP) 0.0.x 子集：
//   - 服务端：POST /api/session （JSON-RPC：session/new / session/update），
//     请求注入本 Agent，另提供 GET /api/session?id=xxx SSE 事件流。
//   - 客户端：向远程 ACP 服务端发 session/new 并读取 SSE session/reply。
type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	srvMu    sync.Mutex
	server   *http.Server
	serverID string

	mu       sync.RWMutex
	sessions map[string]*sessionState
}

type sessionState struct {
	ID       string
	Replying []map[string]interface{}
	History  []string // 轮次历史 [user, agent, user, agent...]，延续上下文用
	LastUsed time.Time
}

// maxSessionTurns 单会话保留的最大轮次对数。
const maxSessionTurns = 10

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.sessions = make(map[string]*sessionState)
	tp := p.name + "_"

	// 注册自身为输出通道：agent 回复 emit 到本通道时有落点。
	// （回复主要走同步注入返回，此通道用于 agent 主动 output_send__acp）
	s.RegisterOutputChannel(p.name, 1, "ACP Agent 互联通道（外部 agent 会话的回复由此返回）", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		log.Printf("[%s] channel output: %s", p.name, truncateStr(payload, 120))
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "listen", Default: "127.0.0.1:12001",
		Type: "string", DisplayName: "监听地址",
		Description: "ACP 服务端监听地址，设为空可禁用 HTTP 服务",
		Category:    p.name,
	})

	s.RegisterTool(tp+"acp_query", sdk.ToolDef{
		Name: tp + "acp_query", Description: "向远程 ACP Agent（如 opencode http://127.0.0.1:13000、pi bridge http://127.0.0.1:12011 或回环到自身 12001）发起一个会话请求并等待回复，返回其最终回答文本，兼容 SSE 型与同步 JSON 型 ACP 服务端",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"server_url": map[string]interface{}{"type": "string", "description": "目标 ACP 服务端地址（如 http://127.0.0.1:13000）"},
				"prompt":     map[string]interface{}{"type": "string", "description": "发送给目标 Agent 的任务描述"},
				"timeout":    map[string]interface{}{"type": "integer", "description": "等待回复超时（秒），默认 120"},
			},
			"required": []string{"server_url", "prompt"},
		},
		Cleaner: func(output string) string {
			var r struct {
				Reply string `json:"reply"`
			}
			if json.Unmarshal([]byte(output), &r) == nil && r.Reply != "" {
				return r.Reply
			}
			return output
		},
	}, p.handleAcpQuery)

	s.RegisterTool(tp+"acp_configure", sdk.ToolDef{
		Name: tp + "acp_configure", Description: "修改 ACP 插件的监听配置并生效（重启 HTTP 服务）",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"listen": map[string]interface{}{"type": "string", "description": "监听地址（如 0.0.0.0:12001，设为空禁用）"},
			},
		},
	}, p.handleConfigure)

	s.RegisterTool(tp+"acp_status", sdk.ToolDef{
		Name: tp + "acp_status", Description: "查看 ACP 插件运行状态与当前活跃会话数",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleStatus)

	addr, _ := s.Settings().Get("listen")
	if addrStr, ok := addr.(string); ok && addrStr != "" {
		if err := p.startServer(addrStr); err != nil {
			log.Printf("[%s] start ACP server: %v", p.name, err)
		}
	}

	log.Printf("[%s] started", p.name)
	return nil
}

func (p *Plugin) Stop() error {
	p.stopServer()
	return nil
}

func (p *Plugin) stopServer() {
	p.srvMu.Lock()
	defer p.srvMu.Unlock()
	if p.server != nil {
		p.server.Close()
		p.server = nil
		p.serverID = ""
	}
}

// ---- Inbound HTTP Server ----

func (p *Plugin) startServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", p.handleSession)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %v", addr, err)
	}

	srv := &http.Server{Handler: mux}
	addrStr := listener.Addr().String()

	p.srvMu.Lock()
	if p.server != nil {
		p.server.Close()
	}
	p.server = srv
	p.serverID = addrStr
	p.srvMu.Unlock()

	go func() {
		log.Printf("[%s] ACP server on %s", p.name, addrStr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[%s] serve: %v", p.name, err)
		}
	}()
	return nil
}

func (p *Plugin) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		p.handleSessionPost(w, r)
	case "GET":
		p.handleSessionSSE(w, r)
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

// handleSessionPost 处理 JSON-RPC：session/new 与 session/update
func (p *Plugin) handleSessionPost(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		JSONRPC string      `json:"jsonrpc"`
		ID      interface{} `json:"id"`
		Method  string      `json:"method"`
		Params  struct {
			Request *struct {
				Text string `json:"text"`
			} `json:"request,omitempty"`
			SessionID string `json:"session_id,omitempty"`
			Final     bool   `json:"final,omitempty"`
		} `json:"params,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json-rpc", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "session/new":
		text := ""
		if req.Params.Request != nil {
			text = strings.TrimSpace(req.Params.Request.Text)
		}
		if text == "" {
			http.Error(w, "request.text required", http.StatusBadRequest)
			return
		}

		// 会话：调用方可指定 session_id 延续多轮；不指定则新建。
		sid := strings.TrimSpace(req.Params.SessionID)
		p.mu.Lock()
		if sid != "" {
			if _, exists := p.sessions[sid]; !exists {
				p.sessions[sid] = &sessionState{ID: sid, LastUsed: time.Now()}
			}
		} else {
			sid = fmt.Sprintf("session_%d", time.Now().UnixNano())
			p.sessions[sid] = &sessionState{ID: sid, LastUsed: time.Now()}
		}
		st := p.sessions[sid]
		p.mu.Unlock()

		// 延续上下文
		injectText := text
		p.mu.Lock()
		if len(st.History) > 0 {
			ctxText := strings.Join(st.History, "\n")
			injectText = "[对话上下文]\n" + ctxText + "\n[本轮输入]\n" + text
		}
		p.mu.Unlock()

		// 同步注入等待回复：不抢占打断，完整闭环返回文本。
		reply := ""
		if p.sdk != nil {
			reply = p.sdk.InjectInputSync(p.name, p.name,
				fmt.Sprintf("[来自ACP Agent的请求 session %s]\n%s\n[注意] 请直接以文本回复本请求，不要调用 output_send__%s——你的最终文本回复会被系统自动返回给请求方。", sid, injectText, p.name))
		}

		// 写回历史 + 填充 Replying 供 SSE 消费
		p.mu.Lock()
		st.History = append(st.History, "用户: "+text, "助手: "+reply)
		if len(st.History) > maxSessionTurns*2 {
			st.History = st.History[len(st.History)-maxSessionTurns*2:]
		}
		st.LastUsed = time.Now()
		if reply != "" {
			st.Replying = append(st.Replying, map[string]interface{}{
				"type": "reply", "text": reply,
			})
		}
		p.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]interface{}{
				"session": map[string]interface{}{"id": sid},
				"reply":   reply,
			},
		})

	case "session/update":
		sid := req.Params.SessionID
		p.mu.Lock()
		st := p.sessions[sid]
		p.mu.Unlock()
		if st == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if req.Params.Final {
			// 客户端结束会话：标记并保留历史（后续可再 session/new 续）
			p.mu.Lock()
			st.LastUsed = time.Now()
			p.mu.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]interface{}{"final": true},
		})

	case "session/cancel":
		p.mu.Lock()
		delete(p.sessions, req.Params.SessionID)
		p.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]interface{}{"canceled": true},
		})

	default:
		http.Error(w, fmt.Sprintf("unknown method %q", req.Method), http.StatusBadRequest)
	}
}

// handleSessionSSE 提供 SSE 事件流订阅
func (p *Plugin) handleSessionSSE(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("id")
	if sid == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}

	p.mu.RLock()
	st := p.sessions[sid]
	p.mu.RUnlock()
	if st == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		p.mu.RLock()
		replies := append([]map[string]interface{}{}, st.Replying...)
		p.mu.RUnlock()
		for _, rep := range replies {
			data, _ := json.Marshal(rep)
			fmt.Fprintf(w, "event: session/reply\ndata: %s\n\n", data)
			fl.Flush()
		}
		p.mu.Lock()
		st.Replying = nil
		p.mu.Unlock()

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// ---- Outbound：ACP 客户端 ----

// parseRPCBody 兼容 JSON 与 SSE 两种响应体
func parseRPCBody(ct string, body []byte) (*json.RawMessage, error) {
	if strings.Contains(ct, "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(body))
		var last string
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data != "" && data != "[DONE]" {
					last = data
				}
			}
		}
		if last == "" {
			return nil, fmt.Errorf("SSE body 中无 data 帧: %s", truncateStr(string(body), 200))
		}
		body = []byte(last)
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v: %s", err, truncateStr(string(body), 300))
	}
	return &raw, nil
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (p *Plugin) handleAcpQuery(args map[string]interface{}) (interface{}, error) {
	serverURL, _ := args["server_url"].(string)
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if serverURL == "" {
		return map[string]interface{}{"error": "server_url 不能为空"}, nil
	}
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "http://" + serverURL
	}
	prompt, _ := args["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return map[string]interface{}{"error": "prompt 不能为空"}, nil
	}
	timeoutSec := 120
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeoutSec = int(v)
	}

	endpoint := serverURL + "/api/session"
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}

	newBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "acp-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"method": "session/new",
		"params": map[string]interface{}{
			"request": map[string]interface{}{"text": prompt},
		},
	})

	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(newBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("请求失败(超时%d秒): %v", timeoutSec, err)}, nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 && resp.StatusCode != 202 {
		return map[string]interface{}{"error": fmt.Sprintf("状态码 %d", resp.StatusCode), "raw_body": truncateStr(string(body), 300)}, nil
	}

	raw, err := parseRPCBody(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}
	var rpcResp struct {
		Result *struct {
			Session *struct {
				ID string `json:"id"`
			} `json:"session,omitempty"`
			SessionID string `json:"sessionId,omitempty"`
			Reply     string `json:"reply,omitempty"`
		} `json:"result,omitempty"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(*raw, &rpcResp); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("JSON-RPC 解析失败: %v", err), "raw_body": truncateStr(string(*raw), 300)}, nil
	}
	if rpcResp.Error != nil {
		return map[string]interface{}{"error": fmt.Sprintf("ACP 错误 [%d]: %s", rpcResp.Error.Code, rpcResp.Error.Message)}, nil
	}
	if rpcResp.Result == nil {
		return map[string]interface{}{"error": "响应中没有 result", "raw_body": truncateStr(string(*raw), 300)}, nil
	}

	// 兼容两种协议：
	//   A) 标准/SSE 型（opencode、本插件服务端）：result.session.id，回复经 SSE 事件流
	//   B) 同步 JSON 型（pi bridge）：result.sessionId + result.reply
	if rpcResp.Result.Reply != "" {
		return map[string]interface{}{
			"session_id": rpcResp.Result.SessionID,
			"status":     "completed",
			"reply":      rpcResp.Result.Reply,
		}, nil
	}
	if rpcResp.Result.Session == nil || rpcResp.Result.Session.ID == "" {
		return map[string]interface{}{"error": "响应中没有 session.id", "raw_body": truncateStr(string(*raw), 300)}, nil
	}
	sid := rpcResp.Result.Session.ID

	replyText := p.readSSEReply(endpoint, sid, client, timeoutSec)

	return map[string]interface{}{
		"session_id": sid,
		"status":     "completed",
		"reply":      replyText,
	}, nil
}

// readSSEReply 通过 SSE 读取 session/reply 事件并拼接回复文本
func (p *Plugin) readSSEReply(endpoint, sid string, client *http.Client, timeoutSec int) string {
	sseURL := fmt.Sprintf("%s?id=%s", endpoint, sid)
	req, _ := http.NewRequest("GET", sseURL, nil)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("(SSE 读取失败: %v)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("(SSE 状态码 %d: %s)", resp.StatusCode, truncateStr(string(bb), 200))
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for sc.Scan() {
		if time.Now().After(deadline) {
			break
		}
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, "event: ") && strings.TrimSpace(strings.TrimPrefix(line, "event: ")) == "session/error" {
			break
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var evt struct {
				SessionID string `json:"session_id,omitempty"`
				Type      string `json:"type,omitempty"`
				Text      string `json:"text,omitempty"`
				Message   *struct {
					Text string `json:"text"`
				} `json:"message,omitempty"`
			}
			if json.Unmarshal([]byte(data), &evt) == nil {
				text := evt.Text
				if evt.Message != nil && evt.Message.Text != "" {
					text = evt.Message.Text
				}
				if text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(text)
				}
			}
		}
	}
	if sb.Len() == 0 {
		return "(未收到回复)"
	}
	return sb.String()
}

// ---- Management ----

func (p *Plugin) handleConfigure(args map[string]interface{}) (interface{}, error) {
	listen, _ := args["listen"].(string)
	listen = strings.TrimSpace(listen)

	if err := p.sdk.Settings().Set("listen", listen); err != nil {
		return fmt.Sprintf("保存配置失败: %v", err), nil
	}

	if listen == "" || listen == "off" || listen == "disabled" {
		p.stopServer()
		return "ACP HTTP 服务已禁用", nil
	}

	if err := p.startServer(listen); err != nil {
		return fmt.Sprintf("ACP 配置已保存，但服务启动失败: %v", err), nil
	}
	return fmt.Sprintf("ACP 配置已更新，监听: %s", listen), nil
}

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	addr, _ := p.sdk.Settings().Get("listen")
	addrStr, _ := addr.(string)

	p.srvMu.Lock()
	serverRunning := p.server != nil
	listening := p.serverID
	p.srvMu.Unlock()

	p.mu.RLock()
	n := len(p.sessions)
	p.mu.RUnlock()

	if !serverRunning {
		listening = "未运行"
	}
	return fmt.Sprintf("配置监听地址: %s\n当前监听: %s\n服务状态: %s\n活跃会话: %d",
		addrStr, listening, map[bool]string{true: "运行中", false: "已停止"}[serverRunning], n), nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

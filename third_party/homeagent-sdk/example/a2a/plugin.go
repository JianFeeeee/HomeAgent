package main

import (
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

type Plugin struct {
	name       string
	sdk        *sdk.PluginSDK
	srvMu      sync.Mutex
	server     *http.Server
	serverAddr string

	// 会话表：session_id → 上下文前缀。A2A 无状态协议下由插件侧维护
	// 多轮上下文：同 session 的后续请求会把之前的对话拼进注入文本。
	sessMu    sync.Mutex
	sessions  map[string]*a2aSession
}

// a2aSession 记录一个会话的轮次历史，用于延续上下文。
type a2aSession struct {
	ID       string
	History  []string // 轮次文本 [user1, agent1, user2, agent2, ...]
	LastUsed time.Time
}

// maxSessionTurns 单会话保留的最大轮次对数（防上下文无限膨胀）。
const maxSessionTurns = 10

// sessionGCPeriod 会话过期清理周期；超过 2 小时未用的会话回收。
const sessionGCPeriod = 30 * time.Minute

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.sessions = make(map[string]*a2aSession)
	tp := p.name + "_"

	// 注册自身为输出通道：agent 回复 emit 到本通道时有落点，
	// 且 output_list_channels 可见（agent 能主动向 a2a 会话推送消息）。
	if err := s.RegisterOutputChannel(p.name, 1, "A2A Agent 互联通道（外部 agent 查询的回复由此返回）", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		log.Printf("[%s] channel output: %s", p.name, truncateRunes(payload, 120))
		return map[string]interface{}{"status": "ok"}, nil
	}); err != nil {
		log.Printf("[%s] register output channel: %v", p.name, err)
	}

	// 会话 GC：后台周期回收长期不用的会话
	go p.sessionGCLoop()

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "listen", Default: "127.0.0.1:12000",
		Type: "string", DisplayName: "监听地址",
		Description: "A2A 服务端监听地址，设为空可禁用 HTTP 服务",
		Category: p.name,
	})

	// Outbound: query + discover
	s.RegisterTool(tp+"a2a_query", sdk.ToolDef{
		Name: tp + "a2a_query", Description: "向另一个 A2A Agent 发送查询并获取回复",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_url": map[string]interface{}{"type": "string", "description": "目标 Agent 的 A2A 端点 URL"},
				"query":     map[string]interface{}{"type": "string", "description": "发送给目标 Agent 的文本查询"},
				"timeout":   map[string]interface{}{"type": "integer", "description": "超时时间（秒），默认 60"},
			},
			"required": []string{"agent_url", "query"},
		},
		Cleaner: func(output string) string {
			var r struct{ Content string }
			if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
				return r.Content
			}
			return output
		},
	}, p.handleA2AQuery)

	s.RegisterTool(tp+"a2a_discover", sdk.ToolDef{
		Name: tp + "a2a_discover", Description: "获取另一个 A2A Agent 的能力描述（Agent Card）",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_url": map[string]interface{}{"type": "string", "description": "目标 Agent 的 A2A 端点 URL"},
			},
			"required": []string{"agent_url"},
		},
	}, p.handleA2ADiscover)

	// Management tools
	s.RegisterTool(tp+"a2a_configure", sdk.ToolDef{
		Name: tp + "a2a_configure", Description: "修改 A2A 插件配置并自动重启服务。支持动态更改监听地址等参数。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"listen": map[string]interface{}{"type": "string", "description": "监听地址（如 0.0.0.0:12000，设为空字符串禁用 HTTP 服务）"},
			},
		},
	}, p.handleConfigure)

	s.RegisterTool(tp+"a2a_restart", sdk.ToolDef{
		Name: tp + "a2a_restart", Description: "重启 A2A HTTP 服务端。当连接异常或配置变更后需要重新加载时使用。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleRestart)

	s.RegisterTool(tp+"a2a_status", sdk.ToolDef{
		Name: tp + "a2a_status", Description: "查看 A2A 插件的运行状态，包括监听地址和当前配置。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleStatus)

	// Inbound HTTP server
	if addr, _ := s.Settings().Get("listen"); addr != nil {
		if addrStr, ok := addr.(string); ok && addrStr != "" {
			if err := p.startServer(addrStr); err != nil {
				log.Printf("[%s] start A2A server: %v", p.name, err)
			}
		}
	}

	log.Printf("[%s] started", p.name)
	return nil
}

func (p *Plugin) Stop() error {
	p.stopServer()
	return nil
}

// sessionGCLoop 周期清理超时会话。
func (p *Plugin) sessionGCLoop() {
	ticker := time.NewTicker(sessionGCPeriod)
	defer ticker.Stop()
	for range ticker.C {
		p.sessMu.Lock()
		for id, sess := range p.sessions {
			if time.Since(sess.LastUsed) > 2*time.Hour {
				delete(p.sessions, id)
			}
		}
		p.sessMu.Unlock()
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func (p *Plugin) stopServer() {
	p.srvMu.Lock()
	defer p.srvMu.Unlock()
	if p.server != nil {
		p.server.Close()
		p.server = nil
		p.serverAddr = ""
	}
}

// ---- Inbound HTTP Server ----

func (p *Plugin) startServer(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-card", p.handleAgentCard)
	mux.HandleFunc("/task", p.handleIncomingTask)
	mux.HandleFunc("/a2a", p.handleIncomingA2A)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %v", addr, err)
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	addrStr := listener.Addr().String()

	p.srvMu.Lock()
	if p.server != nil {
		p.server.Close()
	}
	p.server = srv
	p.serverAddr = addrStr
	p.srvMu.Unlock()

	go func() {
		log.Printf("[%s] A2A server on %s", p.name, addrStr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[%s] serve: %v", p.name, err)
		}
	}()
	return nil
}

func (p *Plugin) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	card := map[string]interface{}{
		"name":        p.name,
		"description": "HomeAgent A2A Agent - 支持多工具调用与记忆管理",
		"url":         r.Host,
		"version":     "1.0.0",
		"capabilities": []map[string]string{
			{"id": "a2a_query", "name": "查询", "description": "接收并处理文本查询"},
			{"id": "a2a_stream", "name": "流式响应", "description": "支持 SSE 流式回复"},
		},
		"skills": []map[string]string{
			{"id": "chat", "name": "对话", "description": "通用对话与问题回答"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(card)
}

func (p *Plugin) handleIncomingA2A(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		p.handleAgentCard(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Query     string `json:"query,omitempty"`
			SessionID string `json:"session_id,omitempty"`
			Message *struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text,omitempty"`
					Type string `json:"type,omitempty"`
				} `json:"parts"`
			} `json:"message,omitempty"`
		} `json:"params,omitempty"`
	}
	json.Unmarshal(body, &req)

	switch req.Method {
	case "tasks.send":
		// Extract query text
		queryText := req.Params.Query
		if queryText == "" && req.Params.Message != nil {
			for _, part := range req.Params.Message.Parts {
				if part.Text != "" {
					queryText += part.Text + "\n"
				}
			}
			queryText = strings.TrimSpace(queryText)
		}
		if queryText == "" {
			http.Error(w, "query/message.text required", http.StatusBadRequest)
			return
		}

		// 会话：调用方可指定 session_id 延续多轮上下文；不指定则新建。
		sessionID := strings.TrimSpace(req.Params.SessionID)
		injectText := queryText
		p.sessMu.Lock()
		if sessionID != "" {
			sess := p.sessions[sessionID]
			if sess == nil {
				sess = &a2aSession{ID: sessionID, LastUsed: time.Now()}
				p.sessions[sessionID] = sess
			}
			sess.LastUsed = time.Now()
			// 有历史则把上下文拼在前面（截尾防爆量）
			if len(sess.History) > 0 {
				ctxText := strings.Join(sess.History, "\n")
				injectText = "[对话上下文]\n" + ctxText + "\n[本轮输入]\n" + queryText
			}
		} else {
			sessionID = fmt.Sprintf("a2a_%d", time.Now().UnixNano())
			p.sessions[sessionID] = &a2aSession{ID: sessionID, LastUsed: time.Now()}
		}
		p.sessMu.Unlock()

		// 同步注入：阻塞等待 agent 处理完成拿回复（不再抢占打断、
		// 也不再回 202 让请求方永远等不到结果）。HTTP 超时由调用方控制。
		reply := p.sdk.InjectInputSync(p.name, p.name,
			fmt.Sprintf("[来自A2A Agent的查询 session=%s]\n%s\n[注意] 请直接以文本回复本查询，不要调用 output_send__%s——你的最终文本回复会被系统自动返回给请求方。", sessionID, injectText, p.name))

		// 回复写回会话历史（下一轮作为上下文）
		p.sessMu.Lock()
		if sess := p.sessions[sessionID]; sess != nil {
			sess.History = append(sess.History, "用户: "+queryText, "助手: "+reply)
			if len(sess.History) > maxSessionTurns*2 {
				sess.History = sess.History[len(sess.History)-maxSessionTurns*2 :]
			}
			sess.LastUsed = time.Now()
		}
		p.sessMu.Unlock()

		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]interface{}{
				"id":     fmt.Sprintf("task_%d", time.Now().UnixNano()),
				"status": "completed",
				"session_id": sessionID,
				"message": map[string]interface{}{
					"role": "agent",
					"parts": []map[string]string{{"type": "text", "text": reply}},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

	case "tasks.get":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]interface{}{"id": req.Params.Query, "status": "unknown"},
		})

	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func (p *Plugin) handleIncomingTask(w http.ResponseWriter, r *http.Request) {
	p.handleIncomingA2A(w, r)
}

// ---- A2A Protocol Types ----

type A2AAgentCard struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	URL          string          `json:"url"`
	Version      string          `json:"version,omitempty"`
	Capabilities []A2ACapability `json:"capabilities,omitempty"`
	Skills       []A2ASkill      `json:"skills,omitempty"`
}

type A2ACapability struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type A2ASkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema string `json:"input_schema,omitempty"`
}

type A2ARequest struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      string    `json:"id"`
	Method  string    `json:"method"`
	Params  A2AParams `json:"params,omitempty"`
}

type A2AParams struct {
	Query   string      `json:"query,omitempty"`
	Message *A2AMessage `json:"message,omitempty"`
	TaskID  string      `json:"id,omitempty"`
}

type A2AResponse struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      string     `json:"id"`
	Result  *A2AResult `json:"result,omitempty"`
	Error   *A2AError  `json:"error,omitempty"`
}

type A2AResult struct {
	TaskID    string       `json:"id,omitempty"`
	Status    string       `json:"status,omitempty"`
	Message   *A2AMessage  `json:"message,omitempty"`
	AgentCard *A2AAgentCard `json:"agent_card,omitempty"`
}

type A2AMessage struct {
	Role  string    `json:"role"`
	Parts []A2APart `json:"parts"`
}

type A2APart struct {
	Text string `json:"text,omitempty"`
	Data string `json:"data,omitempty"`
	Type string `json:"type,omitempty"`
}

type A2AError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- Outbound Handlers ----

func (p *Plugin) handleA2ADiscover(args map[string]interface{}) (interface{}, error) {
	agentURL, _ := args["agent_url"].(string)
	agentURL = strings.TrimRight(agentURL, "/")
	if !strings.HasPrefix(agentURL, "http://") && !strings.HasPrefix(agentURL, "https://") {
		agentURL = "http://" + agentURL
	}

	cardURL := agentURL
	if !strings.HasSuffix(cardURL, "/agent-card") {
		cardURL = agentURL + "/agent-card"
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(cardURL)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("连接失败: %v", err)}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return map[string]interface{}{"error": fmt.Sprintf("状态码 %d", resp.StatusCode), "raw_body": string(body)}, nil
	}

	var card A2AAgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		var fallback map[string]interface{}
		if err2 := json.Unmarshal(body, &fallback); err2 == nil {
			return map[string]interface{}{"agent_info": fallback, "format": "非标准格式"}, nil
		}
		return map[string]interface{}{"error": fmt.Sprintf("解析失败: %v", err), "raw_body": string(body)}, nil
	}

	return map[string]interface{}{
		"name": card.Name, "description": card.Description,
		"version": card.Version, "url": card.URL,
		"capabilities": card.Capabilities, "skills": card.Skills,
	}, nil
}

func (p *Plugin) handleA2AQuery(args map[string]interface{}) (interface{}, error) {
	agentURL, _ := args["agent_url"].(string)
	query, _ := args["query"].(string)
	timeoutSec := 60
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeoutSec = int(v)
	}

	agentURL = strings.TrimRight(agentURL, "/")
	if !strings.HasPrefix(agentURL, "http://") && !strings.HasPrefix(agentURL, "https://") {
		agentURL = "http://" + agentURL
	}

	taskURL := agentURL
	if strings.HasSuffix(agentURL, "/agent-card") {
		taskURL = strings.TrimSuffix(agentURL, "/agent-card")
	}
	taskURL = strings.TrimRight(taskURL, "/") + "/task"

	reqBody := A2ARequest{
		JSONRPC: "2.0",
		ID:      fmt.Sprintf("a2a_%d", time.Now().UnixNano()),
		Method:  "tasks.send",
		Params: A2AParams{
			Message: &A2AMessage{Role: "user", Parts: []A2APart{{Text: query, Type: "text"}}},
		},
	}

	bodyData, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := client.Post(taskURL, "application/json", bytes.NewReader(bodyData))
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("请求失败(超时%d秒): %v", timeoutSec, err)}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return map[string]interface{}{"error": fmt.Sprintf("状态码 %d", resp.StatusCode), "raw_body": string(body)}, nil
	}

	var a2aResp A2AResponse
	if err := json.Unmarshal(body, &a2aResp); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("解析响应失败: %v", err), "raw_body": string(body)}, nil
	}

	if a2aResp.Error != nil {
		return map[string]interface{}{"error": fmt.Sprintf("Agent错误 [%d]: %s", a2aResp.Error.Code, a2aResp.Error.Message)}, nil
	}
	if a2aResp.Result == nil {
		return map[string]interface{}{"error": "空结果", "raw_body": string(body)}, nil
	}

	var replyText string
	if a2aResp.Result.Message != nil {
		for _, part := range a2aResp.Result.Message.Parts {
			if part.Text != "" {
				replyText += part.Text + "\n"
			}
		}
		replyText = strings.TrimSpace(replyText)
	}

	return map[string]interface{}{
		"task_id": a2aResp.Result.TaskID, "status": a2aResp.Result.Status,
		"response": replyText,
	}, nil
}

// ---- Management Handlers ----

func (p *Plugin) handleConfigure(args map[string]interface{}) (interface{}, error) {
	listen, _ := args["listen"].(string)
	listen = strings.TrimSpace(listen)

	if err := p.sdk.Settings().Set("listen", listen); err != nil {
		return fmt.Sprintf("保存配置失败: %v", err), nil
	}

	if listen == "" || listen == "off" || listen == "disabled" {
		p.stopServer()
		return "A2A HTTP 服务已禁用（listen 设为空）", nil
	}

	if err := p.startServer(listen); err != nil {
		return fmt.Sprintf("A2A 配置已保存，但服务启动失败: %v", err), nil
	}
	return fmt.Sprintf("A2A 配置已更新。监听地址: %s (已启动)", listen), nil
}

func (p *Plugin) handleRestart(args map[string]interface{}) (interface{}, error) {
	p.stopServer()

	addr, _ := p.sdk.Settings().Get("listen")
	addrStr, _ := addr.(string)
	if addrStr == "" || addrStr == "off" || addrStr == "disabled" {
		return "A2A 服务未配置监听地址（listen 为空），无法启动", nil
	}

	if err := p.startServer(addrStr); err != nil {
		return fmt.Sprintf("A2A 服务启动失败: %v", err), nil
	}

	p.srvMu.Lock()
	listening := p.serverAddr
	p.srvMu.Unlock()
	return fmt.Sprintf("A2A 服务已重启，监听: %s", listening), nil
}

func (p *Plugin) handleStatus(args map[string]interface{}) (interface{}, error) {
	addr, _ := p.sdk.Settings().Get("listen")
	addrStr, _ := addr.(string)

	p.srvMu.Lock()
	serverRunning := p.server != nil
	listening := p.serverAddr
	p.srvMu.Unlock()
	if !serverRunning {
		listening = "未运行"
	}

	return fmt.Sprintf("配置监听地址: %s\n当前监听: %s\n服务状态: %s",
		addrStr, listening, map[bool]string{true: "运行中", false: "已停止"}[serverRunning]), nil
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// JSON-RPC 2.0 消息结构
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      int              `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP Tool 定义
type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// MCP 工具调用结果
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// MCPCallResult 是 tools/call 的响应体。
//
// ★ isError 字段必须声明（2026-10-05 补）。
//
//	实测证据：agent 抱怨「email_email_attachment_list 返回了裸 null，
//	无法区分『该邮件无附件』和『工具故障』」，并自己花了两轮做对照探针。
//	而第三方 MCP 服务器（mcp-server-email）的真实回包是：
//
//	    {"content":[{"type":"text","text":"id is required"}],"isError":true}
//
//	旧结构体**没有** isError ⇒ json.Unmarshal 静默丢弃它
//	⇒ CallTool 把错误当成普通文本返回 ⇒ **失败与成功对模型完全同形**。
//
//	这与本项目反复记的「报假成功比报错危险」同型：模型据此无法判断
//	该重试、该换参数、还是该放弃，只能反复试探浪费轮次。
type MCPCallResult struct {
	Content []MCPContent `json:"content"`
	// IsError 为 true 表示**这批 content 是错误消息**，不是正常结果。
	// MCP 规范里它是 result 内的字段（不是 JSON-RPC 层的 error），
	// 所以外层 resp.Error 会是 nil —— 不显式读就会当成成功。
	IsError bool `json:"isError"`
}

// Transport 抽象: 支持 stdio / SSE
type Transport interface {
	Send(req *rpcRequest) (*rpcResponse, error)
	Close() error
}

// Server 代表一个 MCP 服务器连接
type Server struct {
	name      string
	transport Transport
	mu        sync.Mutex
	nextID    int
}

func NewServer(name string, t Transport) *Server {
	return &Server{name: name, transport: t}
}

func (s *Server) Name() string { return s.name }

func (s *Server) nextRequestID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}

// ListTools 列举 MCP 服务器提供的所有工具
func (s *Server) ListTools() ([]MCPTool, error) {
	req := &rpcRequest{
		JSONRPC: "2.0",
		ID:      s.nextRequestID(),
		Method:  "tools/list",
	}
	resp, err := s.transport.Send(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s tools/list: %w", s.name, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp %s tools/list error: %s", s.name, resp.Error.Message)
	}
	if resp.Result == nil {
		return nil, nil
	}
	var result struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(*resp.Result, &result); err != nil {
		return nil, fmt.Errorf("mcp %s tools/list unmarshal: %w", s.name, err)
	}
	return result.Tools, nil
}

// CallTool 调用 MCP 工具
func (s *Server) CallTool(name string, args map[string]interface{}) (string, error) {
	req := &rpcRequest{
		JSONRPC: "2.0",
		ID:      s.nextRequestID(),
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name":      name,
			"arguments": args,
		},
	}
	resp, err := s.transport.Send(req)
	if err != nil {
		return "", fmt.Errorf("mcp %s tools/call %s: %w", s.name, name, err)
	}
	return classifyCallResult(s.name, name, resp)
}

// classifyCallResult 把 tools/call 的响应判成（文本, error）。
//
// 抽成独立函数是为了让判据能**直接拿真实回包驱动它**，而不必起一个
// 真的 MCP 子进程（第三方二进制 + 真实凭证，环境依赖太重）。
// 生产路径与判据走的是同一份逻辑。
func classifyCallResult(serverName, toolName string, resp *rpcResponse) (string, error) {
	if resp == nil {
		return "", fmt.Errorf("mcp %s tools/call %s: 无响应（连接已断开或服务端崩溃）", serverName, toolName)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("mcp %s tools/call %s error: %s", serverName, toolName, resp.Error.Message)
	}
	if resp.Result == nil {
		return "", fmt.Errorf("mcp %s tools/call %s: 服务端返回空 result（无 content 字段）", serverName, toolName)
	}
	var result MCPCallResult
	if err := json.Unmarshal(*resp.Result, &result); err != nil {
		return "", fmt.Errorf("mcp %s tools/call %s unmarshal: %w", serverName, toolName, err)
	}
	// 拼接所有文本片段
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	// ★ isError=true ⇒ 这是**失败**，必须以 error 形态上抛。
	//
	//   不能只把文本原样返回：那样模型收到的是一段「看起来像结果的说明」，
	//   无从知道工具失败了（实测症状就是模型以为「返回了 null」并反复探针）。
	//   也不能丢弃错误文本 —— 那正是服务端给的**唯一**诊断线索。
	if result.IsError {
		msg := strings.TrimSpace(sb)
		if msg == "" {
			msg = "（服务端未给出错误文本）"
		}
		return "", fmt.Errorf("mcp %s tools/call %s 失败: %s", serverName, toolName, msg)
	}
	// 成功但没有任何文本内容：如实说明，不要回空串。
	// 空串在工具结果里同样会被模型读成「没有内容」，与失败难以区分。
	if strings.TrimSpace(sb) == "" {
		return "", fmt.Errorf("mcp %s tools/call %s 成功但未返回任何文本内容（content 为空）", serverName, toolName)
	}
	return sb, nil
}

func (s *Server) Close() error {
	return s.transport.Close()
}

func (s *Server) SetTransport(t Transport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transport.Close()
	s.transport = t
	s.nextID = 0
}

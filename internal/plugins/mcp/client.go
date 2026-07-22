package mcp

import (
	"encoding/json"
	"fmt"
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
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
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

type MCPCallResult struct {
	Content []MCPContent `json:"content"`
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
	if resp.Error != nil {
		return "", fmt.Errorf("mcp %s tools/call %s error: %s", s.name, name, resp.Error.Message)
	}
	if resp.Result == nil {
		return "", nil
	}
	var result MCPCallResult
	if err := json.Unmarshal(*resp.Result, &result); err != nil {
		return "", fmt.Errorf("mcp %s tools/call %s unmarshal: %w", s.name, name, err)
	}
	// 拼接所有文本片段
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
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

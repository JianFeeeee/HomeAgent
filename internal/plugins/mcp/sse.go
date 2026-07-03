package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SSETransport 通过 HTTP POST 进行 JSON-RPC 通信（简化版，非流式）
type SSETransport struct {
	url     string
	client  *http.Client
	pending map[int]chan *rpcResponse
	done    chan struct{}
}

func NewSSETransport(url string) *SSETransport {
	return &SSETransport{
		url:     url,
		client:  &http.Client{},
		pending: make(map[int]chan *rpcResponse),
		done:    make(chan struct{}),
	}
}
func (t *SSETransport) Send(req *rpcRequest) (*rpcResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequest("POST", t.url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &rpcResp, nil
}

func (t *SSETransport) Close() error {
	close(t.done)
	return nil
}

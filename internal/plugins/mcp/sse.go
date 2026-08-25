package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// SSETransport 通过 HTTP POST 进行 JSON-RPC 通信。
// 兼容两种服务端响应：application/json 直连响应，以及 Streamable HTTP
// 的异步响应（HTTP 202 + text/event-stream 的 SSE data 帧）。
type SSETransport struct {
	url    string
	client *http.Client
}

func NewSSETransport(url string) *SSETransport {
	// 必须带超时：远程 MCP server 网络抖动/无响应时，
	// 无超时的 client 会让插件加载永久阻塞（webui 等后续插件全部起不来）
	return &SSETransport{
		url: url,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}
}

// parseResponseBody 根据 Content-Type 解析 JSON-RPC 响应
func parseResponseBody(contentType string, body []byte) (*rpcResponse, error) {
	if strings.Contains(contentType, "text/event-stream") {
		// SSE 流：逐行提取 data: 帧
		var lastJSON []byte
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" || data == "[DONE]" {
					continue
				}
				var frame map[string]json.RawMessage
				if err := json.Unmarshal([]byte(data), &frame); err == nil {
					if _, isResp := frame["id"]; isResp || frame["result"] != nil || frame["error"] != nil {
						lastJSON = []byte(data)
					}
				}
			}
		}
		if len(lastJSON) == 0 {
			return nil, fmt.Errorf("SSE 流中未找到 JSON-RPC 响应帧: %s", truncate(string(body), 300))
		}
		body = lastJSON
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w, body=%s", err, truncate(string(body), 300))
	}
	return &rpcResp, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
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
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	// 非 2xx：尝试提取错误信息
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	return parseResponseBody(ct, body)
}

func (t *SSETransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}

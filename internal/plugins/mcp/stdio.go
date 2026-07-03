package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// StdioTransport 通过子进程 stdin/stdout 进行 JSON-RPC 通信
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	pending map[int]chan *rpcResponse
	done    chan struct{}
}

func NewStdioTransport(command string, args []string, env []string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// 忽略 stderr（MCP 服务器可能输出日志到 stderr）
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	t := &StdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		pending: make(map[int]chan *rpcResponse),
		done:    make(chan struct{}),
	}

	go t.readLoop()

	return t, nil
}

func (t *StdioTransport) readLoop() {
	dec := json.NewDecoder(t.stdout)
	for {
		var resp rpcResponse
		if err := dec.Decode(&resp); err != nil {
			close(t.done)
			// 通知所有等待的请求
			t.mu.Lock()
			for _, ch := range t.pending {
				close(ch)
			}
			t.pending = make(map[int]chan *rpcResponse)
			t.mu.Unlock()
			return
		}

		t.mu.Lock()
		ch, ok := t.pending[resp.ID]
		delete(t.pending, resp.ID)
		t.mu.Unlock()

		if ok {
			ch <- &resp
		}
	}
}

func (t *StdioTransport) Send(req *rpcRequest) (*rpcResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ch := make(chan *rpcResponse, 1)
	t.mu.Lock()
	t.pending[req.ID] = ch
	t.mu.Unlock()

	if _, err := t.stdin.Write(data); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write stdin: %w", err)
	}
	if _, err := t.stdin.Write([]byte("\n")); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write newline: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-t.done:
		return nil, fmt.Errorf("mcp transport closed")
	}
}

func (t *StdioTransport) Close() error {
	t.stdin.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	<-t.done
	return t.cmd.Wait()
}

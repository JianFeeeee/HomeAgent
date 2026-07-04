package openclaw

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type sidecarRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type sidecarResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type OCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type OCCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
}

type sidecarProcess struct {
	name   string
	dir    string
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	stdout *bufio.Scanner
	mu     sync.Mutex
	nextID int
	closed bool
	stopped bool
}

func launchSidecar(dir, name string) (*sidecarProcess, error) {
	mainJS := filepath.Join(dir, "main.js")
	if _, err := os.Stat(mainJS); os.IsNotExist(err) {
		return nil, nil
	}
	return launchProcess("node", mainJS, dir, name)
}

func launchProcess(bin, arg, dir, name string) (*sidecarProcess, error) {
	nodePath := bin
	if bin == "node" {
		if p := os.Getenv("NODE_PATH"); p != "" {
			nodePath = filepath.Join(p, "node")
		}
	}

	// 将 dir（插件目录）作为最后一个参数传给 Node.js 进程
	// 这样: node <script> <plugin-dir>
	// echoplugin 的 main.js 忽略它, 模拟器用它加载真实插件
	cmd := exec.Command(nodePath, arg, dir)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	sp := &sidecarProcess{
		name:   name,
		dir:    dir,
		cmd:    cmd,
		stdin:  bufio.NewWriter(stdin),
		stdout: bufio.NewScanner(bufio.NewReader(stdout)),
	}

	if err := sp.waitReady(); err != nil {
		sp.Close()
		return nil, err
	}

	return sp, nil
}

func (s *sidecarProcess) waitReady() error {
	done := make(chan error, 1)
	go func() {
		_, err := s.call("ping", nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("sidecar %s ping timeout", s.name)
	}
}

func (s *sidecarProcess) call(method string, params interface{}) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.stopped {
		return nil, fmt.Errorf("sidecar %s closed", s.name)
	}
	s.nextID++
	id := s.nextID
	req := sidecarRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if _, err := s.stdin.Write(data); err != nil {
		return nil, err
	}
	if _, err := s.stdin.Write([]byte("\n")); err != nil {
		return nil, err
	}
	if err := s.stdin.Flush(); err != nil {
		return nil, err
	}

	if !s.stdout.Scan() {
		if s.stdout.Err() != nil {
			return nil, fmt.Errorf("sidecar %s read: %w", s.name, s.stdout.Err())
		}
		return nil, fmt.Errorf("sidecar %s closed unexpectedly", s.name)
	}
	line := s.stdout.Text()

	var resp sidecarResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("sidecar %s unmarshal: %w", s.name, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("sidecar %s error: %s", s.name, resp.Error.Message)
	}
	if resp.Result == nil {
		return nil, nil
	}
	return []byte(*resp.Result), nil
}

func (s *sidecarProcess) ListTools() ([]OCPTool, error) {
	data, err := s.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var result struct {
		Tools []OCPTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (s *sidecarProcess) CallTool(name string, args map[string]interface{}) (string, error) {
	data, err := s.call("tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", nil
	}
	var result OCCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	var sb string
	for _, c := range result.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	return sb, nil
}

func (s *sidecarProcess) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stopped {
		return
	}
	s.stopped = true
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}
	log.Printf("[openclaw] sidecar %s stopped", s.name)
}

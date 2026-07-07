package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Conn interface {
	Send(line string) error
	ReadLine() (string, error)
	Close() error
}

func dial(cfg *Config) (Conn, error) {
	if cfg.Remote != "" {
		return dialRemote(cfg.Remote, cfg.APIKey)
	}
	return dialLocal(cfg.Socket, cfg.APIKey)
}

func dialLocal(socket, apiKey string) (Conn, error) {
	if socket == "" {
		socket = discoverSocket("")
	}
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("unix %s: %w", socket, err)
	}
	lc := &localConn{conn: c, r: bufio.NewReader(c)}
	if apiKey != "" {
		if err := lc.Send("/auth " + apiKey); err != nil {
			c.Close()
			return nil, fmt.Errorf("auth send: %w", err)
		}
		line, err := lc.ReadLine()
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("auth read: %w", err)
		}
		if strings.Contains(line, "unauthorized") {
			c.Close()
			return nil, fmt.Errorf("auth rejected: %s", line)
		}
	}
	return lc, nil
}

type localConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func (c *localConn) Send(line string) error {
	_, err := fmt.Fprintf(c.conn, "%s\n", line)
	return err
}

func (c *localConn) ReadLine() (string, error) {
	s, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s, "\n"), nil
}

func (c *localConn) Close() error {
	return c.conn.Close()
}

func dialRemote(baseURL, apiKey string) (Conn, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	return &remoteConn{url: baseURL + "/api/v1/chat", apiKey: apiKey}, nil
}

type remoteConn struct {
	url    string
	apiKey string
	mu     sync.Mutex
	buf    []string
	closed bool
}

func (c *remoteConn) Send(line string) error {
	req, err := http.NewRequest("POST", c.url, strings.NewReader(
		fmt.Sprintf(`{"message":%q}`, line),
	))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	content, _ := result["response"].(string)

	c.mu.Lock()
	c.buf = append(c.buf, content)
	c.mu.Unlock()
	return nil
}

func (c *remoteConn) ReadLine() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.buf) == 0 && !c.closed {
		c.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		c.mu.Lock()
	}
	if c.closed && len(c.buf) == 0 {
		return "", io.EOF
	}
	s := c.buf[0]
	c.buf = c.buf[1:]
	return s, nil
}

func (c *remoteConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

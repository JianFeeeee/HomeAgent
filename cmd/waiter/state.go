package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

type State struct {
	conn Conn
	mu   sync.Mutex
}

func (s *State) Connect(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
	}
	c, err := dial(cfg)
	if err != nil {
		return err
	}
	s.conn = c
	return nil
}

func (s *State) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *State) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

func (s *State) RemoteConn() *remoteConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rc, ok := s.conn.(*remoteConn); ok {
		return rc
	}
	return nil
}

func (s *State) Send(line string) error {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return fmt.Errorf("not connected")
	}
	return c.Send(line)
}

func (s *State) SendChat(msg string) (string, error) {
	if err := s.Send(msg); err != nil {
		return "", err
	}
	line, err := s.readLine()
	if err != nil {
		return "", err
	}
	return parseRespLine(line)
}

func (s *State) SendBuiltin(cmd string) (string, error) {
	if err := s.Send(cmd); err != nil {
		return "", err
	}
	line, err := s.readLine()
	if err != nil {
		return "", err
	}
	return parseRespLine(line)
}

func (s *State) readLine() (string, error) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return "", fmt.Errorf("not connected")
	}
	return c.ReadLine()
}

type respLine struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Error   string `json:"error"`
}

func parseRespLine(line string) (string, error) {
	var rl respLine
	if err := json.Unmarshal([]byte(line), &rl); err != nil {
		return line, nil
	}
	switch rl.Type {
	case "response":
		return rl.Content, nil
	case "error":
		return "", fmt.Errorf("%s", rl.Error)
	default:
		return line, nil
	}
}

func (s *State) ReadLoop(ctx context.Context, cb func(string)) {
	for {
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c == nil {
			return
		}
		done := make(chan struct{})
		var line string
		var readErr error
		go func() {
			line, readErr = c.ReadLine()
			close(done)
		}()
		select {
		case <-done:
			if readErr != nil {
				return
			}
			cb(line)
		case <-ctx.Done():
			return
		}
	}
}

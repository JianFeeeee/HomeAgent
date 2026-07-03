package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// DefaultSocket 由 main.go 在 Load() 前设置，覆盖默认 socket 路径。
// 若为空，factory 使用 "<dataDir>/cli.sock"。
var DefaultSocket string

func init() {
	plugin.RegisterFactory("cli", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		sock := DefaultSocket
		if sock == "" {
			dataDir, ok := config["data_dir"].(string)
			if !ok {
				return nil, fmt.Errorf("cli plugin: config missing 'data_dir' or not a string")
			}
			sock = filepath.Join(dataDir, "cli.sock")
		}
		return New(name, sock), nil
	})
}

type Plugin struct {
	name   string
	socket string
	ln     net.Listener
	mu     sync.Mutex
	wg     sync.WaitGroup
}

func New(name, socketPath string) *Plugin {
	return &Plugin{
		name:   name,
		socket: socketPath,
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	dir := filepath.Dir(p.socket)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	os.Remove(p.socket)

	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		return fmt.Errorf("listen unix socket %s: %w", p.socket, err)
	}
	p.ln = ln

	os.Chmod(p.socket, 0666)

	p.wg.Add(1)
	go p.acceptLoop(s)

	log.Printf("[cli] unix socket listening on %s", p.socket)
	return nil
}

func (p *Plugin) acceptLoop(s *sdk.PluginSDK) {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			break
		}
		p.wg.Add(1)
		go p.handleConn(conn, s)
	}
}

func (p *Plugin) handleConn(conn net.Conn, s *sdk.PluginSDK) {
	defer conn.Close()
	defer p.wg.Done()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		resp := s.InjectTextSync("cli", "cli", line)
		if resp != nil {
			content, _ := resp.Payload["content"].(string)
			writeLine(conn, map[string]interface{}{
				"type":    "response",
				"content": content,
			})
		} else {
			writeLine(conn, map[string]interface{}{
				"type":  "error",
				"error": "agent is not available",
			})
		}
	}
}

func writeLine(conn net.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	conn.Write(data)
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
	}
	p.wg.Wait()
	os.Remove(p.socket)
	return nil
}

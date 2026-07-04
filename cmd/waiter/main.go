package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
	colorYellow = "\033[33m"
	colorBold   = "\033[1m"
)

type Config struct {
	Socket      string `yaml:"socket"`
	Remote      string `yaml:"remote"`
	Mode        string `yaml:"mode"`
	Colors      bool   `yaml:"colors"`
	HistorySize int    `yaml:"history_size"`
	Prompt      string `yaml:"prompt"`
}

func defaultConfig() Config {
	return Config{
		Mode:        "auto",
		Colors:      true,
		HistorySize: 1000,
		Prompt:      "waiter> ",
	}
}

func configPaths() []string {
	home, _ := os.UserHomeDir()
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(home, ".config")
	}
	return []string{
		filepath.Join(xdgConfig, "homeagent", "cli.yaml"),
		filepath.Join(home, ".homeagent.yaml"),
		".homeagent.yaml",
	}
}

func historyPath() string {
	home, _ := os.UserHomeDir()
	xdgData := os.Getenv("XDG_DATA_HOME")
	if xdgData == "" {
		xdgData = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(xdgData, "homeagent")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "cli_history")
}

func loadConfig() Config {
	cfg := defaultConfig()
	for _, p := range configPaths() {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		yaml.Unmarshal(data, &cfg)
		break
	}
	if cfg.HistorySize < 1 {
		cfg.HistorySize = 100
	}
	return cfg
}

func saveConfig(cfg Config) {
	for _, p := range configPaths() {
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0755); err != nil {
			continue
		}
		data, _ := yaml.Marshal(cfg)
		os.WriteFile(p, data, 0644)
		return
	}
}

func discoverSocket(configured string) string {
	if configured != "" {
		return configured
	}
	if s := os.Getenv("HOMEAGENT_SOCKET"); s != "" {
		return s
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".homeagent", "cli.sock"),
		"/var/lib/homeagent/cli.sock",
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append([]string{filepath.Join(xdg, "homeagent", "cli.sock")}, candidates...)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

func resolveEndpoint(cfg Config) (mode string, addr string) {
	switch cfg.Mode {
	case "local":
		return "local", discoverSocket(cfg.Socket)
	case "remote":
		return "remote", cfg.Remote
	default:
		sock := discoverSocket(cfg.Socket)
		if sock != "" {
			if _, err := os.Stat(sock); err == nil {
				return "local", sock
			}
		}
		if cfg.Remote != "" {
			return "remote", cfg.Remote
		}
		return "local", sock
	}
}

type respLine struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Error   string `json:"error"`
}

func printColored(cfg Config, color, msg string) {
	if !cfg.Colors {
		fmt.Println(msg)
		return
	}
	fmt.Printf("%s%s%s\n", color, msg, colorReset)
}

func main() {
	socket := flag.String("socket", "", "unix socket path (overrides config)")
	remote := flag.String("remote", "", "remote webui URL (overrides config)")
	say := flag.String("say", "", "send a message and print response (one-shot)")
	flag.Parse()

	cfg := loadConfig()
	if *socket != "" {
		cfg.Socket = *socket
	}
	if *remote != "" {
		cfg.Remote = *remote
		cfg.Mode = "remote"
	}
	mode, addr := resolveEndpoint(cfg)

	if *say != "" {
		oneShot(cfg, mode, addr, *say)
		return
	}

	runInteractive(cfg, mode, addr)
}

func oneShot(cfg Config, mode, addr, message string) {
	if mode == "remote" {
		resp, err := doRemoteOnce(addr, message)
		if err != nil {
			printColored(cfg, colorRed, fmt.Sprintf("error: %v", err))
			os.Exit(1)
		}
		fmt.Println(resp)
		return
	}

	conn, err := net.DialTimeout("unix", addr, 5*time.Second)
	if err != nil {
		printColored(cfg, colorRed, fmt.Sprintf("connect to %s: %v", addr, err))
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n", message)

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		var rl respLine
		if err := json.Unmarshal(scanner.Bytes(), &rl); err != nil {
			fmt.Println(scanner.Text())
			return
		}
		switch rl.Type {
		case "response":
			fmt.Println(rl.Content)
		case "error":
			printColored(cfg, colorRed, fmt.Sprintf("error: %s", rl.Error))
			os.Exit(1)
		default:
			fmt.Println(scanner.Text())
		}
	}
}

func doRemoteOnce(baseURL, message string) (string, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	body := fmt.Sprintf(`{"message":%q}`, message)
	resp, err := http.Post(baseURL+"/api/v1/chat", "application/json", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	content, _ := result["response"].(string)
	return content, nil
}

func runInteractive(cfg Config, mode, addr string) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	history := newHistory(historyPath(), cfg.HistorySize)
	history.load()

	line := newLineEditor(&history)

	restore, err := setRawMode(0)
	if err != nil {
		restore = func() {}
	}
	defer restore()

	if cfg.Colors {
		fmt.Print(colorBold)
	}
	fmt.Printf("HomeAgent CLI — %s://%s\n", mode, addr)
	if cfg.Colors {
		fmt.Print(colorReset)
	}
	fmt.Println("Type /help for commands.")

	var conn io.ReadWriteCloser
	var readerDone chan struct{}
	connect := func() error {
		if conn != nil {
			conn.Close()
		}
		if readerDone != nil {
			<-readerDone
		}
		c, err := dial(mode, addr)
		if err != nil {
			return err
		}
		conn = c
		readerDone = make(chan struct{})
		go readLoop(cfg, conn, readerDone)
		return nil
	}

	reconnect := func() {
		for i := 0; i < 30; i++ {
			if err := connect(); err != nil {
				if cfg.Colors {
					fmt.Printf("\r\n%sreconnecting (%d/30): %v%s\n", colorYellow, i+1, err, colorReset)
				} else {
					fmt.Printf("\r\nreconnecting (%d/30): %v\n", i+1, err)
				}
				time.Sleep(2 * time.Second)
				continue
			}
			if cfg.Colors {
				fmt.Printf("\r%s%sreconnected%s\n", clearLine, colorGreen, colorReset)
			} else {
				fmt.Printf("\r%sreconnected\n", clearLine)
			}
			return
		}
		if cfg.Colors {
			fmt.Printf("\r%s%sgiving up after 30 attempts%s\n", clearLine, colorRed, colorReset)
		} else {
			fmt.Printf("\r%sgiving up after 30 attempts\n", clearLine)
		}
	}

	// initial connect
	for {
		if err := connect(); err != nil {
			if cfg.Colors {
				fmt.Printf("%sconnect: %v, retrying in 2s...%s\n", colorYellow, err, colorReset)
			} else {
				fmt.Printf("connect: %v, retrying in 2s...\n", err)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}

	prompt := cfg.Prompt

	for {
		fmt.Print(prompt)
		text, err := line.read()
		if err != nil {
			// EOF or error
			break
		}
		line.clear()

		cmd := strings.TrimSpace(text)
		if cmd == "" {
			continue
		}

		if cmd[0] == '/' {
			if handleBuiltin(cfg, cmd, &mode, &addr, &prompt, reconnect, &history) {
				continue
			}
			// unknown command falls through to send as message
		}

		history.add(cmd)
		history.save()

		_, err = fmt.Fprintf(conn, "%s\n", cmd)
		if err != nil {
			if cfg.Colors {
				fmt.Printf("%sconnection lost, reconnecting...%s\n", colorYellow, colorReset)
			} else {
				fmt.Println("connection lost, reconnecting...")
			}
			line.redrawPending(cmd)
			reconnect()
			fmt.Fprintf(conn, "%s\n", cmd)
		}

		select {
		case <-sigCh:
			goto exit
		default:
		}
	}

exit:
	if conn != nil {
		conn.Close()
	}
	if readerDone != nil {
		<-readerDone
	}
}

func dial(mode, addr string) (io.ReadWriteCloser, error) {
	if mode == "remote" {
		return newHTTPConn(strings.TrimRight(addr, "/")), nil
	}
	return net.DialTimeout("unix", addr, 5*time.Second)
}

const clearLine = "\033[2K\r"

func readLoop(cfg Config, conn io.ReadWriteCloser, done chan struct{}) {
	defer close(done)
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if !cfg.Colors {
			fmt.Printf("%s%s\n", clearLine, scanner.Text())
			continue
		}
		var rl respLine
		if err := json.Unmarshal(scanner.Bytes(), &rl); err != nil {
			fmt.Printf("%s%s%s\n", clearLine, scanner.Text(), colorReset)
			continue
		}
		switch rl.Type {
		case "response":
			fmt.Printf("%s%s%s%s\n", clearLine, colorGreen, rl.Content, colorReset)
		case "error":
			fmt.Printf("%s%s%s%s\n", clearLine, colorRed, rl.Error, colorReset)
		default:
			fmt.Printf("%s%s%s\n", clearLine, scanner.Text(), colorReset)
		}
	}
}

// httpConn wraps an HTTP endpoint as a read/write/closer for chat.
type httpConn struct {
	url    string
	mu     sync.Mutex
	buf    []byte
	closed bool
}

func newHTTPConn(baseURL string) *httpConn {
	return &httpConn{url: baseURL + "/api/v1/chat"}
}

func (c *httpConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.buf) == 0 && !c.closed {
		c.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		c.mu.Lock()
	}
	if c.closed && len(c.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *httpConn) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	resp, err := doRemoteOnce(c.url, msg)
	if err != nil {
		return 0, err
	}
	data, _ := json.Marshal(respLine{Type: "response", Content: resp})
	data = append(data, '\n')
	c.mu.Lock()
	c.buf = append(c.buf, data...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *httpConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func handleBuiltin(cfg Config, cmd string, mode, addr *string, prompt *string, reconnect func(), history *History) bool {
	switch {
	case cmd == "/help":
		fmt.Println(`Built-in commands:
  /help            show this help
  /exit, /quit     exit waiter
  /clear           clear screen
  /reconnect       force reconnection
  /connect <path>  switch to a different unix socket
  /remote <url>    switch to remote HTTP mode
  /local           switch back to local socket mode
  /prompt <text>   change the prompt

Any other text is sent as a message to the agent.`)
		return true

	case cmd == "/exit" || cmd == "/quit":
		os.Exit(0)
		return true

	case cmd == "/clear":
		fmt.Print("\033[H\033[2J")
		return true

	case cmd == "/reconnect":
		if cfg.Colors {
			fmt.Printf("%sreconnecting...%s\n", colorYellow, colorReset)
		} else {
			fmt.Println("reconnecting...")
		}
		reconnect()
		return true

	case strings.HasPrefix(cmd, "/connect "):
		*mode = "local"
		*addr = strings.TrimSpace(cmd[9:])
		cfg.Socket = *addr
		saveConfig(cfg)
		reconnect()
		return true

	case strings.HasPrefix(cmd, "/remote "):
		*mode = "remote"
		*addr = strings.TrimSpace(cmd[8:])
		cfg.Remote = *addr
		saveConfig(cfg)
		reconnect()
		return true

	case cmd == "/local":
		*mode = "local"
		*addr = discoverSocket("")
		reconnect()
		return true

	case strings.HasPrefix(cmd, "/prompt "):
		*prompt = strings.TrimSpace(cmd[8:])
		return true

	default:
		return false
	}
}

type LineEditor struct {
	buf     []rune
	pos     int
	hist    *History
	histI   int
	pending string
}

func newLineEditor(h *History) *LineEditor {
	return &LineEditor{
		hist:  h,
		histI: -1,
	}
}

func (e *LineEditor) clear() {
	e.buf = e.buf[:0]
	e.pos = 0
	e.histI = -1
}

func (e *LineEditor) redrawPending(text string) {
	e.pending = text
}

func (e *LineEditor) read() (string, error) {
	if e.pending != "" {
		t := e.pending
		e.pending = ""
		return t, nil
	}

	e.buf = e.buf[:0]
	e.pos = 0
	e.histI = -1

	in := bufio.NewReader(os.Stdin)
	for {
		b := make([]byte, 1)
		_, err := in.Read(b)
		if err != nil {
			return "", err
		}

		switch b[0] {
		case '\r', '\n':
			fmt.Print("\n")
			return string(e.buf), nil

		case 0x03: // Ctrl+C
			fmt.Print("^C\n")
			os.Exit(130)
			return "", nil

		case 0x04: // Ctrl+D
			if len(e.buf) == 0 {
				return "", io.EOF
			}
			continue

		case 0x08, 0x7f: // Backspace
			if e.pos > 0 {
				e.pos--
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}

		case 0x1b: // Escape sequence
			seq := make([]byte, 2)
			if _, err := io.ReadFull(in, seq); err != nil {
				continue
			}
			if seq[0] != '[' {
				continue
			}
			switch seq[1] {
			case 'A': // Up
				e.historyPrev()
			case 'B': // Down
				e.historyNext()
			case 'C': // Right
				if e.pos < len(e.buf) {
					e.pos++
					e.redraw()
				}
			case 'D': // Left
				if e.pos > 0 {
					e.pos--
					e.redraw()
				}
			case 'H', '1': // Home (\x1b[H) or (\x1b[1~)
				if seq[1] == '1' {
					io.ReadFull(in, make([]byte, 1)) // consume ~
				}
				e.pos = 0
				e.redraw()
			case 'F', '4': // End (\x1b[F) or (\x1b[4~)
				if seq[1] == '4' {
					io.ReadFull(in, make([]byte, 1)) // consume ~
				}
				e.pos = len(e.buf)
				e.redraw()
			case '3': // Delete (\x1b[3~)
				io.ReadFull(in, make([]byte, 1)) // consume ~
				if e.pos < len(e.buf) {
					e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
					e.redraw()
				}
			}

		case '\t': // Tab
			e.doCompletion()

		default:
			if b[0] >= 0x20 {
				e.buf = append(e.buf, 0)
				copy(e.buf[e.pos+1:], e.buf[e.pos:])
				e.buf[e.pos] = rune(b[0])
				e.pos++
				e.redraw()
			}
		}
	}
}

func (e *LineEditor) historyPrev() {
	all := e.hist.all()
	if len(all) == 0 {
		return
	}
	if e.histI == -1 {
		e.histI = len(all) - 1
	} else if e.histI > 0 {
		e.histI--
	}
	e.buf = []rune(all[e.histI])
	e.pos = len(e.buf)
	e.redraw()
}

func (e *LineEditor) historyNext() {
	if e.histI == -1 {
		return
	}
	all := e.hist.all()
	e.histI++
	if e.histI >= len(all) {
		e.histI = -1
		e.buf = e.buf[:0]
		e.pos = 0
	} else {
		e.buf = []rune(all[e.histI])
		e.pos = len(e.buf)
	}
	e.redraw()
}

func (e *LineEditor) doCompletion() {
	cmds := []string{"/help", "/exit", "/quit", "/clear", "/reconnect", "/connect ", "/remote ", "/local", "/prompt "}
	prefix := string(e.buf)
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) && c != prefix {
			e.buf = []rune(c)
			e.pos = len(e.buf)
			e.redraw()
			return
		}
	}
}

func (e *LineEditor) redraw() {
	fmt.Print("\r\033[K") // clear line
	fmt.Print(string(e.buf))
	if e.pos < len(e.buf) {
		// move cursor back
		skip := len(e.buf) - e.pos
		fmt.Printf("\033[%dD", skip)
	}
}

type History struct {
	path   string
	lines  []string
	max    int
	mu     sync.Mutex
}

func newHistory(path string, max int) History {
	return History{path: path, max: max}
}

func (h *History) load() {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	h.lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(h.lines) > h.max {
		h.lines = h.lines[len(h.lines)-h.max:]
	}
}

func (h *History) save() {
	h.mu.Lock()
	defer h.mu.Unlock()
	data := strings.Join(h.lines, "\n") + "\n"
	os.WriteFile(h.path, []byte(data), 0644)
}

func (h *History) add(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) > 0 && h.lines[len(h.lines)-1] == line {
		return
	}
	h.lines = append(h.lines, line)
	if len(h.lines) > h.max {
		h.lines = h.lines[len(h.lines)-h.max:]
	}
}

func (h *History) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := make([]string, len(h.lines))
	copy(r, h.lines)
	return r
}

// setRawMode sets stdin to raw mode (non-canonical, no echo).
func setRawMode(fd int) (func(), error) {
	if fd == 0 {
		fd = int(os.Stdin.Fd())
	}
	if !isTerminal(fd) {
		return func() {}, nil
	}
	var oldState syscall.Termios
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&oldState)), 0, 0, 0); err != 0 {
		return func() {}, fmt.Errorf("tcgets: %v", err)
	}
	newState := oldState
	newState.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	newState.Oflag &^= syscall.OPOST
	newState.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	newState.Cflag &^= syscall.CSIZE | syscall.PARENB
	newState.Cflag |= syscall.CS8
	newState.Cc[syscall.VMIN] = 1
	newState.Cc[syscall.VTIME] = 0
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&newState)), 0, 0, 0); err != 0 {
		return func() {}, fmt.Errorf("tcset: %v", err)
	}
	return func() {
		syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&oldState)), 0, 0, 0)
	}, nil
}

func isTerminal(fd int) bool {
	var t syscall.Termios
	_, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return err == 0
}

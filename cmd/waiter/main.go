package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

const clearLine = "\033[2K\r"

var colors = true

func init() {
	if os.Getenv("NO_COLOR") != "" {
		colors = false
	}
}

func printlnC(color, msg string) {
	if !colors {
		fmt.Println(msg)
		return
	}
	fmt.Printf("%s%s%s\n", color, msg, colorReset)
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

func main() {
	socket := flag.String("socket", "", "unix socket path")
	remote := flag.String("remote", "", "remote webui URL")
	apiKey := flag.String("api-key", "", "API key for remote mode")
	configPath := flag.String("config", "", "config file path")
	chat := flag.String("chat", "", "send a message and print final text (one-shot)")
	say := flag.String("say", "", "deprecated alias of -chat")
	flag.Parse()

	cfg := discoverConfig(*configPath)
	cfg.MergeCLI(*socket, *remote, *apiKey)

	if cfg.Socket == "" && cfg.Remote == "" {
		cfg.Socket = discoverSocket("")
	}

	oneShotMsg := *chat
	if oneShotMsg == "" {
		oneShotMsg = *say
	}

	state := &State{}
	if err := state.Connect(cfg); err != nil {
		printlnC(colorRed, fmt.Sprintf("connect: %v", err))
		os.Exit(1)
	}
	defer state.Disconnect()

	if oneShotMsg != "" {
		oneshot(state, oneShotMsg)
		return
	}
	runInteractive(state, cfg)
}

func oneshot(state *State, msg string) {
	resp, err := state.SendChat(msg)
	if err != nil {
		printlnC(colorRed, fmt.Sprintf("error: %v", err))
		os.Exit(1)
	}
	fmt.Println(resp)
}

func runInteractive(state *State, cfg *Config) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	history := newHistory(historyPath(), 1000)
	history.load()

	line := newLineEditor(&history)

	restore, err := setRawMode(0)
	if err != nil {
		restore = func() {}
	}
	defer restore()

	modeLabel := "local"
	addrLabel := cfg.Socket
	if cfg.Remote != "" {
		modeLabel = "remote"
		addrLabel = cfg.Remote
	}
	if colors {
		fmt.Printf("%sHomeAgent CLI%s %s(%s://%s)%s\n", colorBold, colorReset, colorDim, modeLabel, addrLabel, colorReset)
	} else {
		fmt.Printf("HomeAgent CLI (%s://%s)\n", modeLabel, addrLabel)
	}
	fmt.Println("Type /help for commands.")

	var readerCancel func()
	startReader := func() {
		ctx, cancel := context.WithCancel(context.Background())
		readerCancel = cancel
		go state.ReadLoop(ctx, printServerOutput)
	}
	startReader()

	reconnect := func() {
		if readerCancel != nil {
			readerCancel()
		}
		state.Disconnect()
		for i := 0; i < 30; i++ {
			if err := state.Connect(cfg); err != nil {
				printlnC(colorYellow, fmt.Sprintf("reconnecting (%d/30): %v", i+1, err))
				time.Sleep(2 * time.Second)
				continue
			}
			printlnC(colorGreen, "reconnected")
			startReader()
			return
		}
		printlnC(colorRed, "giving up after 30 attempts")
	}

loop:
	for {
		text, err := line.read()
		if err != nil {
			break
		}
		line.clear()

		cmd := strings.TrimSpace(text)
		if cmd == "" {
			continue
		}

		if cmd[0] == '/' {
			if handleBuiltin(cmd, cfg, state, reconnect) {
				if cmd == "/exit" || cmd == "/quit" {
					break loop
				}
				continue
			}
		}

		history.add(cmd)
		history.save()

		if err := state.Send(cmd); err != nil {
			printlnC(colorYellow, "connection lost, reconnecting...")
			line.redrawPending(cmd)
			reconnect()
			state.Send(cmd)
		}

		select {
		case <-sigCh:
			break loop
		default:
		}
	}

	if readerCancel != nil {
		readerCancel()
	}
}

func printServerOutput(content string) {
	if !colors {
		fmt.Printf("%s%s\n", clearLine, content)
		return
	}
	var rl respLine
	if err := json.Unmarshal([]byte(content), &rl); err != nil {
		fmt.Printf("%s%s%s\n", clearLine, content, colorReset)
		return
	}
	switch rl.Type {
	case "response":
		fmt.Printf("%s%s%s%s\n", clearLine, colorGreen, rl.Content, colorReset)
	case "error":
		fmt.Printf("%s%s%s%s\n", clearLine, colorRed, rl.Error, colorReset)
	default:
		fmt.Printf("%s%s%s\n", clearLine, content, colorReset)
	}
}

func setRawMode(fd int) (func(), error) {
	type termios struct {
		Iflag  uint32
		Oflag  uint32
		Cflag  uint32
		Lflag  uint32
		Cc     [20]byte
		Ispeed uint32
		Ospeed uint32
	}
	const (
		TCGETS = 0x5401
		TCSETS = 0x5402
		ICANON = 0x2
		ECHO   = 0x8
	)
	var old termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCGETS, uintptr(unsafe.Pointer(&old))); err != 0 {
		return func() {}, fmt.Errorf("ioctl TCGETS: %v", err)
	}
	new := old
	new.Lflag &^= ICANON | ECHO
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCSETS, uintptr(unsafe.Pointer(&new))); err != 0 {
		return func() {}, fmt.Errorf("ioctl TCSETS: %v", err)
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCSETS, uintptr(unsafe.Pointer(&old)))
	}, nil
}



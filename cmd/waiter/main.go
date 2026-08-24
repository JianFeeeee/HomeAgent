package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
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

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// isTTYFile 判断文件是否为字符终端（非终端时禁用 spinner 转圈）。
func isTTYFile(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// startSpinner 启动 npm 风格的加载动画，返回停止函数。
// stop() 幂等：终止动画并清除当前行。非终端环境直接空操作。
func startSpinner(label string) func() {
	if !colors || !isTTYFile(os.Stdout) {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Printf("%s%s %s%s\n", clearLine, colorDim, spinnerFrames[i%len(spinnerFrames)]+" "+label, colorReset)
				i++
			}
		}
	}()
	stop := func() {
		once.Do(func() {
			close(done)
			fmt.Print(clearLine)
		})
	}
	return stop
}

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
	deviceGateway := flag.String("device", "", "remotedevice 网关地址（如 127.0.0.1:9890），启动设备桥")
	deviceToken := flag.String("device-token", "", "设备接入 token")
	deviceAuthorized := flag.Bool("device-authorized", false, "客户端本地授权（允许远程操控本机；也可在 waiter.yaml 配 device_authorized: true）")
	testCap := flag.String("test-cap", "", "测试本地能力（screensue/speakeruse/screensee/clipboardsee/clipboardsue/computeruse/camerasue），如 --test-cap screensue")
	testCapArgs := flag.String("test-cap-args", "", "测试能力的参数")
	flag.Parse()

	// 本地能力测试模式（无需连接服务器）
	if *testCap != "" {
		runCapTest(*testCap, *testCapArgs)
		return
	}

	cfg := discoverConfig(*configPath)
	cfg.MergeCLI(*socket, *remote, *apiKey)
	cfg.ApplyDefault()

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

	// 设备桥：--device 或配置 device_gateway 时，waiter 作为被控设备接入 remotedevice
	dg := *deviceGateway
	if dg == "" {
		dg = cfg.DeviceGateway
	}
	dt := *deviceToken
	if dt == "" {
		dt = cfg.DeviceToken
	}
	if dg != "" && dt != "" {
		if err := startDeviceBridge(dg, dt); err != nil {
			printlnC(colorYellow, fmt.Sprintf("device bridge: %v (continue without)", err))
		} else {
			// 客户端本地授权：命令行 --device-authorized 或 waiter.yaml device_authorized
			auth := *deviceAuthorized || cfg.DeviceAuthorized
			deviceBridge.SetAuthorized(auth)
			printlnC(colorGreen, "device bridge active: "+deviceBridgeID+" authorized="+fmt.Sprint(auth))
			defer stopDeviceBridge()
		}
	}

	if oneShotMsg != "" {
		oneshot(state, oneShotMsg)
		return
	}

	runInteractive(state, cfg)
}

func oneshot(state *State, msg string) {
	stop := startSpinner("thinking...")
	resp, err := state.SendChatStream(msg, func(rl respLine) {
		// 第一个过程帧到达即停转，后续帧直接渲染
		stop()
		printServerEvent(rl)
	})
	stop()
	if err != nil {
		printlnC(colorRed, fmt.Sprintf("error: %v", err))
		os.Exit(1)
	}
	printlnC(colorGreen, resp)
}

// runCapTest 本地能力测试（无需连接服务器）
func runCapTest(capName, args string) {
	fmt.Printf("=== 测试能力: %s ===\n", capName)
	fmt.Printf("参数: %s\n", args)
	fmt.Println("===========================")

	// 覆盖 sendBridgeResult 为本地打印
	sendBridgeResult = func(reqID, status, output, errMsg string) {
		fmt.Printf("结果状态: %s\n", status)
		if output != "" {
			fmt.Printf("输出: %s\n", output)
		}
		if errMsg != "" {
			fmt.Printf("错误: %s\n", errMsg)
		}
	}

	handleHomeagentCmd("test-001", "homeagent-"+capName+" "+args)
	fmt.Println("===========================")
	fmt.Println("测试完成")
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
	var spinnerStopMu sync.Mutex
	var spinnerStop = func() {}
	startReader := func() {
		ctx, cancel := context.WithCancel(context.Background())
		readerCancel = cancel
		go state.ReadLoop(ctx, func(line string) {
			spinnerStopMu.Lock()
			stop := spinnerStop
			spinnerStopMu.Unlock()
			stop()
			printServerOutput(line)
		})
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

		// 发送成功后启动加载动画，收到第一帧服务器输出时自动停止
		spinnerStopMu.Lock()
		spinnerStop = startSpinner("thinking...")
		spinnerStopMu.Unlock()

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

// printServerOutput 渲染一行服务器输出（JSON 帧）。
func printServerOutput(content string) {
	rl := parseRespLineStruct(content)
	if !colors {
		fmt.Printf("%s%s\n", clearLine, renderPlain(rl, content))
		return
	}
	printServerEventColored(rl, content)
}

// printServerEvent 渲染一个已解析的过程/终结事件。
func printServerEvent(rl respLine) {
	if !colors {
		fmt.Printf("%s%s\n", clearLine, renderPlain(rl, ""))
		return
	}
	printServerEventColored(rl, "")
}

// renderPlain 无色模式下的纯文本渲染。
func renderPlain(rl respLine, raw string) string {
	switch rl.Type {
	case "reasoning":
		return "[思考] " + rl.Content
	case "tool_call":
		return fmt.Sprintf("[工具] %s (%s) %s", rl.Tool, rl.Status, rl.Result)
	case "response":
		return rl.Content
	case "error":
		return "[错误] " + rl.Error
	default:
		if raw != "" {
			return raw
		}
		return rl.Content
	}
}

// printServerEventColored 彩色模式下的帧渲染。
func printServerEventColored(rl respLine, raw string) {
	switch rl.Type {
	case "reasoning":
		fmt.Printf("%s%s· %s%s\n", clearLine, colorDim, rl.Content, colorReset)
	case "tool_call":
		mark, markColor := "⚙", colorYellow
		switch rl.Status {
		case "ok":
			mark, markColor = "✔", colorGreen
		case "denied", "interrupted", "error":
			mark, markColor = "✘", colorRed
		}
		preview := rl.Result
		if preview != "" {
			preview = " " + preview
		}
		fmt.Printf("%s%s%s %s [%s]%s%s\n", clearLine, markColor, mark, rl.Tool, rl.Status, preview, colorReset)
	case "response":
		fmt.Printf("%s%s%s%s\n", clearLine, colorGreen, rl.Content, colorReset)
	case "error":
		fmt.Printf("%s%s%s%s\n", clearLine, colorRed, rl.Error, colorReset)
	default:
		text := raw
		if text == "" {
			text = rl.Content
		}
		fmt.Printf("%s%s%s\n", clearLine, text, colorReset)
	}
}

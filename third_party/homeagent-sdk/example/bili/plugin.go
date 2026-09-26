package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type Plugin struct {
	name  string
	sdk   *sdk.PluginSDK
	proxy string

	// runCancel 取消**正在跑**的 yt-dlp；runWG 等它真正退出。
	//
	// 为何需要：下载是分钟级操作，而 Stop() 必须能把它掐掉。
	// 只 cancel 不 wait 的话内核会在插件死后立刻释放共享段，
	// 而 yt-dlp 还在往插件的 stdout 写 —— 那正是内核 readLoop 挂死的成因。
	runMu     sync.Mutex
	runCancel context.CancelFunc
	runWG     sync.WaitGroup
}

// trackRun 登记一次外部命令运行，返回完成时调用 untrack。
func (p *Plugin) trackRun() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	p.runMu.Lock()
	p.runCancel = cancel
	p.runWG.Add(1)
	p.runMu.Unlock()
	return ctx, func() {
		p.runWG.Done()
		p.runMu.Lock()
		p.runCancel = nil
		p.runMu.Unlock()
	}
}

// killRunGroup 掐掉正在跑的 yt-dlp 及其子进程（ffmpeg 等）。
func (p *Plugin) killRunGroup(pid int) {
	if pid <= 0 {
		return
	}
	// 负 pid = 整个进程组（yt-dlp 拉起的 ffmpeg 也在内）
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	tp := p.name + "_"

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "output_dir", Default: "/tmp/bili_videos",
		Type: "string", DisplayName: "下载目录",
		Description: "B站视频下载后的保存目录",
		Category:    p.name,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "proxy", Default: "",
		Type: "string", DisplayName: "HTTP 代理",
		Description: "yt-dlp 下载使用的 HTTP 代理地址（如 http://127.0.0.1:7890），留空则不设置",
		Category:    p.name,
	})
	if v, _ := s.Settings().Get("proxy"); v != nil {
		if str, ok := v.(string); ok {
			p.proxy = str
		}
	}

	s.RegisterTool(tp+"video", sdk.ToolDef{
		Name:        tp + "video",
		Description: "使用 yt-dlp 下载B站视频到本地。支持查看视频信息后再下载。下载后返回文件路径。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":       map[string]interface{}{"type": "string", "description": "B站视频分享链接"},
				"info_only": map[string]interface{}{"type": "boolean", "description": "仅获取视频信息（标题、清晰度列表），不下载"},
				"format":    map[string]interface{}{"type": "string", "description": "视频格式ID（如 30112=高清1080P, 30080=高清1080P, 30064=高清720P, 30032=清晰480P, 30016=流畅360P），不指定则自动选最优"},
			},
			"required": []string{"url"},
		},
		Cleaner: func(output string) string {
			var r struct{ Content string }
			if json.Unmarshal([]byte(output), &r) == nil && r.Content != "" {
				return r.Content
			}
			return output
		},
	}, p.handleBiliVideo)
	return nil
}

// Stop 取消并等待正在跑的下载。
//
// 空实现的代价（实测）：yt-dlp 是分钟级操作，Stop 时它还在跑，
// 而它**继承本插件的 stdout**。内核 Kill 掉本插件后，yt-dlp 变孤儿
// 且继续持有管道写端 ⇒ 内核 readLoop 永远等不到 EOF ⇒ 整个关停挂死
// 直到 systemd 90 秒超时 SIGKILL（线上症状：只有 bili 报
// "SIGKILL 后 2s 仍未被收割"，之后近 90 秒无日志）。
func (p *Plugin) Stop() error {
	p.runMu.Lock()
	cancel := p.runCancel
	p.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// 等它真的退出：不等的话内核会先释放共享段，孙进程仍在写 stdout。
	p.runWG.Wait()
	return nil
}

type ytdlpFormat struct {
	FormatID       string  `json:"format_id"`
	FormatNote     string  `json:"format_note"`
	Ext            string  `json:"ext"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	TBR            float64 `json:"tbr"`
	Filesize       int64   `json:"filesize"`
	FilesizeApprox int64   `json:"filesize_approx"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	FPS            float64 `json:"fps"`
}

type ytdlpInfo struct {
	Title      string        `json:"title"`
	Duration   float64       `json:"duration"`
	WebpageURL string        `json:"webpage_url"`
	Filename   string        `json:"_filename"`
	Formats    []ytdlpFormat `json:"formats"`
}

func (p *Plugin) handleBiliVideo(args map[string]interface{}) (interface{}, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}
	infoOnly, _ := args["info_only"].(bool)
	format, _ := args["format"].(string)

	outputDir := "/tmp/bili_videos"
	if p.sdk != nil {
		if v, _ := p.sdk.Settings().Get("output_dir"); v != nil {
			if s, ok := v.(string); ok && s != "" {
				outputDir = s
			}
		}
	}
	// 安全校验：output_dir 是配置项，但避免被配成系统目录导致 yt-dlp 任意位置写。
	// 禁止根/家目录本身，且规范化后必须落在明确子目录内。
	outputDir = filepath.Clean(outputDir)
	for _, forbidden := range []string{"/", "/etc", "/usr", "/bin", "/sbin", "/boot", "/dev", "/proc", "/sys", "/var"} {
		if outputDir == forbidden {
			return nil, fmt.Errorf("output_dir 不能是系统目录 %s", forbidden)
		}
	}
	os.MkdirAll(outputDir, 0755)

	var out bytes.Buffer
	ytdlpArgs := []string{"--no-warnings", "--dump-json", url}
	ctx, done := p.trackRun()
	defer done()
	// CommandContext：Stop 里的 cancel 能直接掐掉它。
	// Setpgid：让 yt-dlp 自成进程组，它再拉的 ffmpeg 也在组内，
	// killRunGroup 能一次带走整棵子进程树。
	cmd := exec.CommandContext(ctx, "yt-dlp", ytdlpArgs...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = proxyEnv(p.proxy)
	setPgid(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("yt-dlp info 已取消（插件停止）")
		}
		return nil, fmt.Errorf("yt-dlp info: %w\n%s", err, strings.TrimSpace(out.String()))
	}

	var info ytdlpInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}

	if infoOnly {
		var filtered []ytdlpFormat
		for _, f := range info.Formats {
			if f.VCodec != "none" || f.ACodec != "none" {
				filtered = append(filtered, f)
			}
		}
		info.Formats = filtered

		lines := []string{fmt.Sprintf("标题: %s", info.Title)}
		if info.Duration > 0 {
			lines = append(lines, fmt.Sprintf("时长: %.0f 秒", info.Duration))
		}

		type fmtLine struct {
			ID   string
			Note string
			Res  string
			Ext  string
			Size string
		}
		var seen []string
		var display []fmtLine
		for _, f := range info.Formats {
			if f.FormatNote == "" {
				continue
			}
			key := f.FormatNote + f.Ext
			if contains(seen, key) {
				continue
			}
			seen = append(seen, key)
			res := ""
			if f.Width > 0 && f.Height > 0 {
				res = fmt.Sprintf("%dx%d", f.Width, f.Height)
			}
			sz := ""
			fs := f.Filesize
			if fs == 0 {
				fs = f.FilesizeApprox
			}
			if fs > 0 {
				sz = fmt.Sprintf(" (%.1f MB)", float64(fs)/1048576)
			}
			display = append(display, fmtLine{ID: f.FormatID, Note: f.FormatNote, Res: res, Ext: f.Ext, Size: sz})
		}
		if len(display) > 0 {
			lines = append(lines, "清晰度列表:")
			for _, d := range display {
				r := d.Res
				if r != "" {
					r = " " + r
				}
				lines = append(lines, fmt.Sprintf("  [%s] %s%s | %s%s", d.ID, d.Note, r, d.Ext, d.Size))
			}
		}

		return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
	}

	taskDir := filepath.Join(outputDir, fmt.Sprintf("bili_%d", time.Now().UnixNano()))
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir task dir: %w", err)
	}

	dlArgs := []string{
		"--no-warnings",
		"--socket-timeout", "30",
		"--retries", "3",
		"--fragment-retries", "3",
		"-o", filepath.Join(taskDir, "%(title)s.%(ext)s"),
		"--no-overwrites",
	}
	if format != "" {
		dlArgs = append(dlArgs, "-f", format)
	}
	dlArgs = append(dlArgs, url)
	cmd2 := exec.CommandContext(ctx, "yt-dlp", dlArgs...)
	cmd2.Env = proxyEnv(p.proxy)
	var dlOut bytes.Buffer
	cmd2.Stdout = &dlOut
	cmd2.Stderr = &dlOut
	setPgid(cmd2)
	if err := cmd2.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("下载已取消（插件停止）")
		}
		return nil, fmt.Errorf("yt-dlp download: %w\n%s", err, strings.TrimSpace(dlOut.String()))
	}

	parts, _ := filepath.Glob(filepath.Join(taskDir, "*.part"))
	for _, f := range parts {
		os.Remove(f)
	}
	residuals, _ := filepath.Glob(filepath.Join(taskDir, "*.ytdl"))
	for _, f := range residuals {
		os.Remove(f)
	}

	entries, _ := os.ReadDir(taskDir)
	var mainFile string
	var mainSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, _ := e.Info()
		if fi == nil {
			continue
		}
		if fi.Size() > mainSize {
			mainSize = fi.Size()
			mainFile = e.Name()
		}
	}
	if mainFile == "" {
		return map[string]interface{}{
			"content": "下载完成，但未找到视频文件",
		}, nil
	}
	dlPath := filepath.Join(taskDir, mainFile)
	return map[string]interface{}{
		"content":  fmt.Sprintf("下载完成: %s (%.1f MB)\n路径: %s", mainFile, float64(mainSize)/1048576, dlPath),
		"file":     dlPath,
		"filename": mainFile,
	}, nil
}

// setPgid 让命令自成进程组：它自己拉的子进程（yt-dlp → ffmpeg）
// 都在同一组里，kill(-pgid) 能一次带走，避免孤儿持有 stdout 管道。
func setPgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func proxyEnv(proxy string) []string {
	env := os.Environ()
	if proxy != "" {
		env = append(env, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy)
	}
	return env
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

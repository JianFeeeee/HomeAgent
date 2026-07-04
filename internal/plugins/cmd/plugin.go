package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
	plugin.RegisterFactory("cmd", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name string
}

func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("cmd_run", sdk.ToolDef{
		Name:        "cmd_run",
		Description: "执行一条系统命令并返回输出。适用于查询系统信息、运行脚本、操作文件等单次命令场景。命令在临时 shell 中执行，不支持交互。如需交互式终端（如 vim、ssh、top），请使用 terminal_create 相关工具。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的命令",
				},
				"timeout": map[string]interface{}{
					"type":        "string",
					"description": "超时时间，例如 10s, 1m, 30s（默认 30s）",
				},
				"workdir": map[string]interface{}{
					"type":        "string",
					"description": "工作目录（可选，默认当前目录）",
				},
			},
			"required": []string{"command"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return map[string]interface{}{"error": "command is required"}, nil
		}

		timeoutStr, _ := args["timeout"].(string)
		if timeoutStr == "" {
			timeoutStr = "30s"
		}
		timeout, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("invalid timeout %q: %v", timeoutStr, err)}, nil
		}

		workdir, _ := args["workdir"].(string)

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		if workdir != "" {
			cmd.Dir = workdir
		}

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return map[string]interface{}{
					"status": "timeout",
					"stdout": truncateOutput(stdout.String()),
					"stderr": truncateOutput(stderr.String()),
					"error":  fmt.Sprintf("命令执行超时（%s）", timeoutStr),
				}, nil
			}
		}

		return map[string]interface{}{
			"status":     "ok",
			"stdout":     truncateOutput(stdout.String()),
			"stderr":     truncateOutput(stderr.String()),
			"exit_code":  cmd.ProcessState.ExitCode(),
			"command":    command,
		}, nil
	})

	return nil
}

func (p *Plugin) Stop() error {
	return nil
}

func truncateOutput(s string) string {
	const maxLen = 32000
	if len(s) > maxLen {
		return s[:maxLen] + fmt.Sprintf("\n... [输出被截断，共 %d 字节]", len(s))
	}
	return strings.TrimRight(s, "\n")
}

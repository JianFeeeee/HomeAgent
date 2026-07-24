package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	mu       sync.RWMutex
	filesDir string
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key:         "plugin.files.dir",
		Default:     "/",
		Type:        "string",
		DisplayName: "文件系统根目录",
		Description: "文件操作允许访问的根目录（设为 / 表示完整主机文件系统）",
		Category:    "files",
	})

	dir := getSetting[string](s.Settings(), "dir", "/")
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve files.dir: %w", err)
	}
	p.filesDir = abs
	os.MkdirAll(p.filesDir, 0755)

	tp := p.name + "_"

	s.RegisterTool(tp+"read", sdk.ToolDef{
		Name:        tp + "read",
		Description: fmt.Sprintf("Read file contents within the sandbox directory (%s). Supports offset/limit for large files.", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":   map[string]interface{}{"type": "string", "description": "File path relative to sandbox or absolute"},
				"offset": map[string]interface{}{"type": "integer", "description": "Starting line number (1-indexed, optional)"},
				"limit":  map[string]interface{}{"type": "integer", "description": "Max lines to return (optional)"},
			},
			"required": []string{"path"},
		},
	}, p.handleRead)

	s.RegisterTool(tp+"write", sdk.ToolDef{
		Name:        tp + "write",
		Description: fmt.Sprintf("Write content to a file. Creates parent directories automatically. Sandbox: %s", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "File path"},
				"content": map[string]interface{}{"type": "string", "description": "Content to write"},
				"mode":    map[string]interface{}{"type": "string", "description": "Write mode: overwrite (default) | append | insert | create"},
				"line":    map[string]interface{}{"type": "integer", "description": "Line number for insert mode (1-indexed)"},
			},
			"required": []string{"path", "content"},
		},
	}, p.handleWrite)

	s.RegisterTool(tp+"edit", sdk.ToolDef{
		Name:        tp + "edit",
		Description: fmt.Sprintf("Apply exact string replacements to a file within the sandbox (%s). All edits are matched against the original file content.", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "File path relative to sandbox or absolute"},
				"edits": map[string]interface{}{
					"type": "array",
					"description": "One or more targeted replacements. Each old must match exactly once in the original file. Do not include overlapping edits.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"old": map[string]interface{}{"type": "string", "description": "Exact text to find (must be unique)"},
							"new": map[string]interface{}{"type": "string", "description": "Replacement text"},
						},
						"required": []string{"old", "new"},
					},
				},
			},
			"required": []string{"path", "edits"},
		},
	}, p.handleEdit)

	s.RegisterTool(tp+"ls", sdk.ToolDef{
		Name:        tp + "ls",
		Description: fmt.Sprintf("List directory contents within the sandbox (%s). Directories are marked with / suffix.", p.filesDir),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string", "description": "Directory path (optional, defaults to sandbox root)"},
				"limit": map[string]interface{}{"type": "integer", "description": "Max entries (optional, default 500)"},
			},
		},
	}, p.handleLs)

	log.Printf("[%s] started, sandbox: %s", p.name, p.filesDir)
	return nil
}

func (p *Plugin) Stop() error {
	log.Printf("[%s] stopped", p.name)
	return nil
}

// resolvePath resolves user-provided path to an absolute path within filesDir.
func (p *Plugin) resolvePath(userPath string) (string, error) {
	if userPath == "" {
		userPath = "."
	}
	if !filepath.IsAbs(userPath) {
		userPath = filepath.Join(p.filesDir, userPath)
	}
	abs, err := filepath.Abs(userPath)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	base := filepath.Clean(p.filesDir)
	if base != "/" && !strings.HasPrefix(abs, base+string(filepath.Separator)) && abs != base {
		return "", fmt.Errorf("path outside sandbox: %s", userPath)
	}
	return abs, nil
}

// handleRead implements the read tool.
func (p *Plugin) handleRead(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("stat error: " + err.Error()), nil
	}
	if info.IsDir() {
		return errorResult("is a directory, use ls instead: " + path), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return errorResult("read error: " + err.Error()), nil
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	offset := 0
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v) - 1
	}
	if offset >= totalLines {
		return errorResult(fmt.Sprintf("offset %d exceeds file length (%d lines)", offset+1, totalLines)), nil
	}

	limit := totalLines - offset
	if v, ok := args["limit"].(float64); ok && v > 0 {
		if int(v) < limit {
			limit = int(v)
		}
	}

	end := offset + limit
	if end > totalLines {
		end = totalLines
	}

	selected := lines[offset:end]
	output := strings.Join(selected, "\n")

	truncated := false
	if limit < totalLines-offset {
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(output)
	if truncated {
		nextOffset := end + 1
		sb.WriteString(fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", offset+1, end, totalLines, nextOffset))
	} else if offset > 0 || end < totalLines {
		sb.WriteString(fmt.Sprintf("\n\n[%d lines total]", totalLines))
	}

	return map[string]interface{}{
		"content": sb.String(),
	}, nil
}

// handleWrite implements the write tool.
func (p *Plugin) handleWrite(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}
	content, _ := args["content"].(string)
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "overwrite"
	}

	line := 0
	if v, ok := args["line"].(float64); ok && v > 0 {
		line = int(v)
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	switch mode {
	case "create":
		if _, err := os.Stat(absPath); err == nil {
			return errorResult("file already exists: " + path), nil
		}
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Created %s (%d bytes)", path, len(content)),
		}, nil

	case "append":
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return errorResult("open error: " + err.Error()), nil
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return errorResult("append error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Appended %d bytes to %s", len(content), path),
		}, nil

	case "insert":
		if line < 1 {
			return errorResult("line must be >= 1 for insert mode"), nil
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				return errorResult("file not found: " + path), nil
			}
			return errorResult("read error: " + err.Error()), nil
		}
		lines := strings.Split(string(data), "\n")
		if line > len(lines)+1 {
			return errorResult(fmt.Sprintf("line %d exceeds file length (%d lines)", line, len(lines))), nil
		}
		idx := line - 1
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:idx]...)
		newLines = append(newLines, content)
		newLines = append(newLines, lines[idx:]...)
		result := strings.Join(newLines, "\n")
		if err := os.WriteFile(absPath, []byte(result), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Inserted %d bytes at line %d in %s", len(content), line, path),
		}, nil

	default: // overwrite
		dir := filepath.Dir(absPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errorResult("mkdir error: " + err.Error()), nil
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			return errorResult("write error: " + err.Error()), nil
		}
		return map[string]interface{}{
			"content": fmt.Sprintf("Wrote %d bytes to %s", len(content), path),
		}, nil
	}
}

// handleEdit implements the edit tool.
func (p *Plugin) handleEdit(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return errorResult("path is required"), nil
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	rawEdits, ok := args["edits"].([]interface{})
	if !ok || len(rawEdits) == 0 {
		return errorResult("edits must be a non-empty array"), nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: " + path), nil
		}
		return errorResult("read error: " + err.Error()), nil
	}

	original := string(data)
	content := original
	applied := 0
	var errors []string

	for i, raw := range rawEdits {
		edit, ok := raw.(map[string]interface{})
		if !ok {
			errors = append(errors, fmt.Sprintf("edit[%d]: invalid format", i))
			continue
		}
		oldText, _ := edit["old"].(string)
		newText, _ := edit["new"].(string)
		if oldText == "" {
			errors = append(errors, fmt.Sprintf("edit[%d]: old is required", i))
			continue
		}

		count := strings.Count(content, oldText)
		if count == 0 {
			errors = append(errors, fmt.Sprintf("edit[%d]: could not find %q in %s", i, oldText, path))
			continue
		}
		if count > 1 {
			errors = append(errors, fmt.Sprintf("edit[%d]: found %d occurrences of %q, must be unique", i, count, oldText))
			continue
		}

		content = strings.Replace(content, oldText, newText, 1)
		applied++
	}

	if applied == 0 {
		msg := "no edits applied"
		if len(errors) > 0 {
			msg += ": " + strings.Join(errors, "; ")
		}
		return errorResult(msg), nil
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return errorResult("write error: " + err.Error()), nil
	}

	msg := fmt.Sprintf("Successfully applied %d/%d edits to %s", applied, len(rawEdits), path)
	if len(errors) > 0 {
		msg += "\nWarnings:\n" + strings.Join(errors, "\n")
	}

	return map[string]interface{}{
		"content": msg,
	}, nil
}

// handleLs implements the ls tool.
func (p *Plugin) handleLs(args map[string]interface{}) (interface{}, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := p.resolvePath(path)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("path not found: " + path), nil
		}
		return errorResult("stat error: " + err.Error()), nil
	}
	if !info.IsDir() {
		return errorResult("not a directory: " + path), nil
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return errorResult("readdir error: " + err.Error()), nil
	}

	limit := 500
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var lines []string
	entryLimitReached := false
	for i, entry := range entries {
		if i >= limit {
			entryLimitReached = true
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}

	if len(lines) == 0 {
		return map[string]interface{}{
			"content": "(empty directory)",
		}, nil
	}

	output := strings.Join(lines, "\n")
	if entryLimitReached {
		output += fmt.Sprintf("\n\n[%d entries limit reached. Use limit=N for more.]", limit)
	}

	return map[string]interface{}{
		"content": output,
	}, nil
}

// errorResult returns a standardized error result.
func errorResult(msg string) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

// getSetting reads a setting with generic type assertion.
func getSetting[T any](s sdk.SettingsAPI, key string, def T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return def
	}
	val, ok := v.(T)
	if !ok {
		return def
	}
	return val
}

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

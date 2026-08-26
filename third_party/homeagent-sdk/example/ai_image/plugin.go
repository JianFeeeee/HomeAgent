package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

type Plugin struct {
	name    string
	sdk     *sdk.PluginSDK
	client  *http.Client
	apiKey  string
	provider string
	model   string
	size    string
	baseURL string
	dataDir string // <data>/ai_images：生成本地图片存放目录
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func getSetting[T string | int64 | float64](s sdk.SettingsAPI, key string, def T) T {
	v, err := s.Get(key)
	if err != nil || v == nil {
		return def
	}
	switch any(def).(type) {
	case string:
		if sv, ok := v.(string); ok {
			return any(sv).(T)
		}
	case int64:
		switch n := v.(type) {
		case float64:
			return any(int64(n)).(T)
		case int64:
			return any(n).(T)
		case string:
			if i, err := strconv.ParseInt(n, 10, 64); err == nil {
				return any(i).(T)
			}
		}
	case float64:
		switch n := v.(type) {
		case float64:
			return any(n).(T)
		case int64:
			return any(float64(n)).(T)
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return any(f).(T)
			}
		}
	}
	return def
}

func getArg[T string | int64 | float64](args map[string]interface{}, key string, def T) T {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch any(def).(type) {
	case string:
		if s, ok := v.(string); ok {
			return any(s).(T)
		}
	case int64:
		switch n := v.(type) {
		case float64:
			return any(int64(n)).(T)
		case int64:
			return any(n).(T)
		case string:
			if i, err := strconv.ParseInt(n, 10, 64); err == nil {
				return any(i).(T)
			}
		}
	case float64:
		switch n := v.(type) {
		case float64:
			return any(n).(T)
		case int64:
			return any(float64(n)).(T)
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return any(f).(T)
			}
		}
	}
	return def
}

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.client = &http.Client{Timeout: 120 * time.Second}

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "api_key", Default: "", Type: "string",
		DisplayName: "API Key", Description: "OpenAI / Stable Diffusion API Key",
		Category: "ai_image", Secret: true,
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "base_url", Default: "", Type: "string",
		DisplayName: "Base URL", Description: "自定义 OpenAI 兼容网关地址（不带 /v1 尾缀，如 http://127.0.0.1:8081）；为空走官方 https://api.openai.com",
		Category:    "ai_image",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "provider", Default: "openai", Type: "string",
		DisplayName: "Provider", Description: "Image generation provider: openai / stability",
		Category:    "ai_image",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "model", Default: "dall-e-3", Type: "string",
		DisplayName: "Model", Description: "Model name (dall-e-3, sd-xl, etc.)",
		Category: "ai_image",
	})
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "size", Default: "1024x1024", Type: "string",
		DisplayName: "Size", Description: "Default image size (1024x1024, 1024x1792, 1792x1024)",
		Category: "ai_image",
	})

	p.apiKey = getSetting(s.Settings(), "api_key", "")
	p.provider = getSetting(s.Settings(), "provider", "openai")
	p.model = getSetting(s.Settings(), "model", "dall-e-3")
	p.size = getSetting(s.Settings(), "size", "1024x1024")
	p.baseURL = strings.TrimRight(strings.TrimSpace(getSetting(s.Settings(), "base_url", "")), "/")

// 生图本地存放目录：插件专属数据目录（SDK DataDir API，内核保证存在）。
if p.sdk != nil {
	if dd := s.Settings().DataDir(); dd != "" {
		p.dataDir = dd
	}
}
if p.dataDir == "" {
	// 旧版内核无 DataDir API 时退到 /tmp
	p.dataDir = filepath.Join(os.TempDir(), "homeagent_ai_images")
}
os.MkdirAll(p.dataDir, 0755)

	tp := p.name + "_"
	s.RegisterTool(tp+"generate", sdk.ToolDef{
		Name: tp + "generate", Description: "Generate image from text prompt using AI. Downloads the result locally and returns a local file path (permanent, no expiry). To show the user, send it via output_send with type=image and payload=the returned path.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{"type": "string", "description": "Text description of the image to generate"},
				"size":   map[string]interface{}{"type": "string", "description": "Image size (1024x1024, 1024x1792, 1792x1024), default from config"},
				"model":  map[string]interface{}{"type": "string", "description": "Model override (dall-e-3, dall-e-2)"},
				"n":      map[string]interface{}{"type": "integer", "description": "Number of images to generate (1-10), default 1"},
			},
			"required": []string{"prompt"},
		},
	}, p.handleGenerate)

	fmt.Printf("[%s] started (provider=%s, model=%s)\n", p.name, p.provider, p.model)
	return nil
}

func (p *Plugin) Stop() error {
	fmt.Printf("[%s] stopped\n", p.name)
	return nil
}

type openAIReq struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

type openAIResp struct {
	Created int64 `json:"created"`
	Data    []struct {
		RevisedPrompt string `json:"revised_prompt"`
		URL           string `json:"url"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (p *Plugin) handleGenerate(args map[string]interface{}) (interface{}, error) {
	prompt := getArg(args, "prompt", "")
	if prompt == "" {
		return map[string]interface{}{"isError": true, "content": "prompt is required"}, nil
	}

	key := getSetting(p.sdk.Settings(), "api_key", p.apiKey)
	if key == "" {
		return map[string]interface{}{"isError": true, "content": "API key not configured. Set plugin.ai_image.api_key via CLI."}, nil
	}

	provider := getSetting(p.sdk.Settings(), "provider", p.provider)
	model := getArg(args, "model", getSetting(p.sdk.Settings(), "model", p.model))
	size := getArg(args, "size", getSetting(p.sdk.Settings(), "size", p.size))
	n := getArg(args, "n", int64(1))
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}

	switch provider {
	case "openai":
		return p.generateOpenAI(prompt, model, size, int(n), key)
	case "stability":
		return p.generateStability(prompt, model, size, int(n), key)
	default:
		return map[string]interface{}{"isError": true, "content": "Unknown provider: " + provider + ". Supported: openai, stability"}, nil
	}
}

func (p *Plugin) generateOpenAI(prompt, model, size string, n int, apiKey string) (interface{}, error) {
	// 上游地址：base_url 非空时走自定义网关（如本机 llmsproxy），约定不带 /v1 尾缀；
	// 为空保持官方直连。兼容误配了 /v1 尾缀的情况（去重）。
	endpoint := "https://api.openai.com/v1/images/generations"
	if p.baseURL != "" {
		base := strings.TrimSuffix(p.baseURL, "/v1")
		endpoint = base + "/v1/images/generations"
	}

	body := openAIReq{
		Model:          model,
		Prompt:         prompt,
		N:              n,
		Size:           size,
		ResponseFormat: "url",
	}

	log.Printf("[ai_image] endpoint=%s baseURL=%q model=%q", endpoint, p.baseURL, model)
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return map[string]interface{}{"isError": true, "content": "Request failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result openAIResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return map[string]interface{}{"isError": true, "content": "Failed to parse response: " + err.Error()}, nil
	}

	if result.Error != nil {
		return map[string]interface{}{"isError": true, "content": "API error: " + result.Error.Message}, nil
	}

	if len(result.Data) == 0 {
		return map[string]interface{}{"isError": true, "content": "No images returned"}, nil
	}

	urls := make([]string, len(result.Data))
	for i, d := range result.Data {
		urls[i] = d.URL
	}

	// 下载到本地 data 目录，返回本地文件路径（而非临时 S3 URL）：
	// - S3 临时 URL 约 1 小时过期，且对无浏览器 UA 的客户端拒绝访问
	// - 本地路径可经 webui /files/ 永久下发给所有客户端（含 API key 客户端）
	localPaths := make([]string, len(urls))
	var errs []string
	for i, u := range urls {
		path, err := p.downloadImage(u, fmt.Sprintf("ai_%s_%d", model, time.Now().UnixNano()))
		if err != nil {
			errs = append(errs, fmt.Sprintf("第%d张下载失败: %v", i+1, err))
			continue
		}
		localPaths[i] = path
	}

	content := fmt.Sprintf("Generated %d image(s) with model %s:", len(urls), model)
	for _, pth := range localPaths {
		if pth != "" {
			content += "\n" + pth
		}
	}
	if len(errs) > 0 {
		content += "\n\n" + strings.Join(errs, "\n")
	}
	content += "\n\n已将图片保存到本地（不会过期）。如需展示请用 output_send__webui(payload=本地路径, type=image)。"
	return map[string]interface{}{
		"content":     content,
		"images":      localPaths,
		"prompt":      prompt,
		"model":       model,
		"local_paths": localPaths,
	}, nil
}

// downloadImage 把生图返回的临时 URL 下载为本地文件，返回本地路径。
// 带浏览器 UA 以规避图床对无 UA 客户端的拦截。
func (p *Plugin) downloadImage(url, baseName string) (string, error) {
	dl := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; HomeAgent/1.0)")
	resp, err := dl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b))[:200])
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ext := ".png"
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "jpeg") || strings.Contains(ct, "jpg") {
		ext = ".jpg"
	} else if strings.Contains(ct, "webp") {
		ext = ".webp"
	}
	path := filepath.Join(p.dataDir, baseName+ext)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

type stabilityReq struct {
	TextPrompts []stabilityPrompt `json:"text_prompts"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Samples     int               `json:"samples"`
}

type stabilityPrompt struct {
	Text   string  `json:"text"`
	Weight float64 `json:"weight,omitempty"`
}

type stabilityArtifact struct {
	Base64 string `json:"base64"`
	Seed   int    `json:"seed"`
}

type stabilityResp struct {
	Artifacts []stabilityArtifact `json:"artifacts"`
	Message   string              `json:"message,omitempty"`
}

func (p *Plugin) generateStability(prompt, model, size string, n int, apiKey string) (interface{}, error) {
	width, height := 1024, 1024
	if parts := strings.Split(size, "x"); len(parts) == 2 {
		if w, err := strconv.Atoi(parts[0]); err == nil {
			width = w
		}
		if h, err := strconv.Atoi(parts[1]); err == nil {
			height = h
		}
	}

	body := stabilityReq{
		TextPrompts: []stabilityPrompt{{Text: prompt, Weight: 1.0}},
		Width:       width,
		Height:      height,
		Samples:     n,
	}

	apiURL := "https://api.stability.ai/v1/generation/stable-diffusion-xl-1024-v1-0/text-to-image"

	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiURL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return map[string]interface{}{"isError": true, "content": "Request failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return map[string]interface{}{"isError": true, "content": fmt.Sprintf("API error (status %d): %s", resp.StatusCode, string(respBody))}, nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result stabilityResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return map[string]interface{}{"isError": true, "content": "Failed to parse response: " + err.Error()}, nil
	}

	if len(result.Artifacts) == 0 {
		msg := result.Message
		if msg == "" {
			msg = "No images returned"
		}
		return map[string]interface{}{"isError": true, "content": msg}, nil
	}

	urls := make([]string, len(result.Artifacts))
	for i, a := range result.Artifacts {
		urls[i] = "data:image/png;base64," + a.Base64
	}

	return map[string]interface{}{
		"content": fmt.Sprintf("Generated %d image(s) via Stability AI:\n%s\n\n图片已保存到本地，如需展示请用 output_send(type=image)。", len(urls), strings.Join(urls, "\n")),
		"images":  urls,
		"prompt":  prompt,
		"model":   model,
	}, nil
}

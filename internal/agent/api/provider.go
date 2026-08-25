package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	luaVM "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
)

// ContentBlock 定义多模态内容块，用于图片/音频等非文本输入。
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	AudioURL *AudioURL `json:"audio_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type AudioURL struct {
	URL string `json:"url"`
}

// MarshalJSON 把音频块序列化成 OpenAI「input_audio」多模态格式（base64 内嵌），
// 供支持音频的模型识别。audio_url 非 OpenAI 标准块；含 base64 数据时转
// input_audio，否则回落到原生 audio_url（透传）。
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if b.Type == "audio_url" && b.AudioURL != nil && b.AudioURL.URL != "" {
		if data, format, ok := parseAudioDataURL(b.AudioURL.URL); ok {
			return json.Marshal(map[string]interface{}{
				"type": "input_audio",
				"input_audio": map[string]string{
					"data":   data,
					"format": format,
				},
			})
		}
	}
	type alias ContentBlock
	return json.Marshal(alias(b))
}

// parseAudioDataURL 从 data:<mime>;base64,<data> 提取 base64 与 format。
// 非 base64（如 http url）返回 ok=false。
func parseAudioDataURL(url string) (data, format string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	rest := url[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	mime := rest[:comma]
	data = rest[comma+1:]
	if mime == "" || data == "" {
		return "", "", false
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "", "", false
	}
	format = audioFormatFromMIME(mime)
	return data, format, true
}

func audioFormatFromMIME(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.Contains(m, "wav"):
		return "wav"
	case strings.Contains(m, "mp3"), strings.Contains(m, "mpeg"):
		return "mp3"
	case strings.Contains(m, "mp4"), strings.Contains(m, "m4a"):
		return "mp4"
	case strings.Contains(m, "ogg"), strings.Contains(m, "opus"):
		return "ogg"
	case strings.Contains(m, "flac"):
		return "flac"
	default:
		return "wav"
	}
}

// Message 表示对话消息。当 Blocks 不为空时 content 在 JSON 中序列化为数组（多模态格式）。
type Message struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	Blocks           []ContentBlock `json:"-"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall     `json:"-"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	raw := map[string]interface{}{
		"role": m.Role,
	}
	if len(m.Blocks) > 0 {
		raw["content"] = m.Blocks
	} else if m.Content != "" || len(m.ToolCalls) == 0 {
		raw["content"] = m.Content
	} else {
		raw["content"] = nil
	}
	if m.ReasoningContent != "" {
		raw["reasoning_content"] = m.ReasoningContent
	}
	if m.ToolCallID != "" {
		raw["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		apiTCs := make([]apiToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			argsBytes, _ := json.Marshal(tc.Arguments)
			apiTCs[i] = apiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: apiFunction{
					Name:      tc.Name,
					Arguments: string(argsBytes),
				},
			}
		}
		raw["tool_calls"] = apiTCs
	}
	return json.Marshal(raw)
}

type CompletionRequest struct {
	Model           string                 `json:"model,omitempty"`
	Messages        []Message              `json:"messages"`
	Temperature     float64                `json:"temperature,omitempty"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	Stream          bool                   `json:"stream,omitempty"`
	Tools           []interface{}          `json:"tools,omitempty"`
	ToolChoice      interface{}            `json:"tool_choice,omitempty"`
	DisableThinking bool                   `json:"disable_thinking"`
	ExtraBody       map[string]interface{} `json:"-"`
}

func (r *CompletionRequest) MarshalJSON() ([]byte, error) {
	type Alias CompletionRequest
	data, err := json.Marshal((*Alias)(r))
	if err != nil {
		return nil, err
	}
	if len(r.ExtraBody) == 0 {
		return data, nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for k, v := range r.ExtraBody {
		raw[k] = v
	}
	return json.Marshal(raw)
}

type CompletionResponse struct {
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	FinishReason     string     `json:"finish_reason,omitempty"`
	TokenUsage       TokenUsage `json:"token_usage,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type TokenUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type apiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}

type apiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type StreamChunk struct {
	Content          string      `json:"content"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
	Done             bool        `json:"done"`
	FinishReason     string      `json:"finish_reason,omitempty"`
	ToolCall         *ToolCall   `json:"tool_call,omitempty"`
	ToolCalls        []ToolCall  `json:"tool_calls,omitempty"`
	Usage            *TokenUsage `json:"usage,omitempty"`
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
	ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
	MaxContextTokens() int
}

// RoutableProvider 是支持精确模型路由/AUTO 优先级的 provider。
// 不与 Provider 强绑定，避免破坏第三方 Provider 实现。
type RoutableProvider interface {
	Provider
	Model() string // 该 provider 提供的模型名（可能为空表示 AUTO）
	Priority() int // AUTO 跨源选择的优先级，大者优先
}

// ModelContextWindow 返回模型的最大上下文窗口（token 数）
// 标称窗口 ≠ 有效窗口：接近满时注意力涣散，调用方应取 70-80% 为目标利用率
func ModelContextWindow(model string) int {
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "deepseek-r1") || strings.Contains(model, "deepseek-chat"):
		return 65536
	case strings.Contains(model, "gpt-4") && (strings.Contains(model, "turbo") || strings.Contains(model, "mini") || strings.Contains(model, "omni")):
		return 128000
	case strings.Contains(model, "gpt-4"):
		return 8192
	case strings.Contains(model, "gpt-3.5"):
		return 16384
	case strings.Contains(model, "claude-3.5") || strings.Contains(model, "claude-3"):
		return 200000
	case strings.Contains(model, "claude"):
		return 100000
	case strings.Contains(model, "gemini-1.5") || strings.Contains(model, "gemini-2"):
		return 1048576
	case strings.Contains(model, "gemini"):
		return 32768
	case strings.Contains(model, "qwen"):
		return 131072
	case strings.Contains(model, "glm") || strings.Contains(model, "chatglm"):
		return 131072
	case strings.Contains(model, "llama-3"):
		return 8192
	case strings.Contains(model, "llama-2"):
		return 4096
	case strings.Contains(model, "mistral") || strings.Contains(model, "mixtral"):
		return 32768
	case strings.Contains(model, "yi-") || strings.Contains(model, "零一"):
		return 200000
	case strings.Contains(model, "moonshot") || strings.Contains(model, "kimi"):
		return 131072
	default:
		return 32768
	}
}

type BaseConfig struct {
	Model         string  `json:"model"`
	BaseURL       string  `json:"base_url"`
	APIKey        string  `json:"api_key"`
	Temperature   float64 `json:"temperature"`
	MaxTokens     int     `json:"max_tokens"`
	ContextWindow int     `json:"context_window"`
	MaxConcurrent int     `json:"max_concurrent"`
	Priority      int     `json:"priority"`
}

// LuaAdaptedProvider 使用 Lua 脚本做请求/响应变换，直接发起 HTTP 调用
// 不再包裹其他 Provider，协议差异全部在 Lua 层处理
type LuaAdaptedProvider struct {
	name    string
	cfg     BaseConfig
	vm      *luaVM.VM
	adapter string
	client  *http.Client
	// streamClient 专用于 SSE 流式调用：无整体超时（SSE 长连接不被截断），
	// 仅保留拨号超时。懒初始化，首次 ChatStream 时创建。
	streamClient *http.Client
	streamMu     sync.Mutex
}

func NewLuaAdaptedProvider(cfg BaseConfig, vm *luaVM.VM, name, adapter string) *LuaAdaptedProvider {
	if cfg.Temperature == 0 {
		cfg.Temperature = 0.7
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
	}
	return &LuaAdaptedProvider{
		name:    name,
		cfg:     cfg,
		vm:      vm,
		adapter: adapter,
		// 180s: llmsproxy 的 AUTO 链会串行尝试多个 tier，每个失败 tier 耗
		// busyWait(2s)+上游超时；120s 曾导致网关侧记录大量 "context canceled"
		// (客户端先放弃)。放宽到 180s 给链式 failover 留足时间。
		client: &http.Client{Timeout: 180 * time.Second},
	}
}

func IsValidSourceConfig(name, baseURL, model, adapter string) bool {
	return validConfigValue(name) && validConfigValue(baseURL) && validConfigValue(model) && validConfigValue(adapter) &&
		(strings.HasPrefix(baseURL, "http://") || strings.HasPrefix(baseURL, "https://"))
}

func validConfigValue(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return !strings.EqualFold(s, "<nil>") && !strings.EqualFold(s, "null") && !strings.EqualFold(s, "nil")
}

func (p *LuaAdaptedProvider) MaxContextTokens() int {
	if p.cfg.ContextWindow > 0 {
		return p.cfg.ContextWindow
	}
	return ModelContextWindow(p.cfg.Model)
}

func (p *LuaAdaptedProvider) Name() string  { return p.name }
func (p *LuaAdaptedProvider) Model() string { return p.cfg.Model }
func (p *LuaAdaptedProvider) Priority() int { return p.cfg.Priority }

func (p *LuaAdaptedProvider) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if p.cfg.Model != "" && (req.Model == "" || req.Model == "AUTO") {
		req.Model = p.cfg.Model
	}
	rawReq, _ := json.Marshal(req)

	transformedBody, err := p.vm.CallTransformRequest(p.adapter, string(rawReq))
	if err != nil {
		return nil, fmt.Errorf("lua transform_request: %w", err)
	}

	endpoint := p.vm.GetAdapterEndpoint(p.adapter)
	if endpoint == "" {
		endpoint = "/chat/completions"
	}
	url := strings.TrimRight(p.cfg.BaseURL, "/") + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(transformedBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.applyAdapterHeaders(httpReq, url, transformedBody)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, &ProviderError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("api error %d: %s", resp.StatusCode, string(rawResp)),
		}
	}

	unifiedJSON, err := p.vm.CallTransformResponse(p.adapter, string(rawResp))
	if err != nil {
		if parsed, perr := parseOpenAICompatibleResponse(rawResp); perr == nil {
			return parsed, nil
		}
		return nil, fmt.Errorf("lua transform_response: %w", err)
	}

	var result CompletionResponse
	if err := json.Unmarshal([]byte(unifiedJSON), &result); err != nil {
		if parsed, perr := parseOpenAICompatibleResponse(rawResp); perr == nil {
			return parsed, nil
		}
		return nil, fmt.Errorf("unmarshal unified response: %w (body: %s)", err, unifiedJSON)
	}

	return &result, nil
}

// applyAdapterHeaders 优先调用 adapter.build_headers(meta) 动态签名钩子，
// 未定义时回落到静态 adapter.headers，最后确保带 Authorization。
func (p *LuaAdaptedProvider) applyAdapterHeaders(httpReq *http.Request, url, body string) {
	meta := map[string]interface{}{
		"url":       url,
		"method":    http.MethodPost,
		"body":      body,
		"api_key":   p.cfg.APIKey,
		"timestamp": time.Now().Unix(),
		"source": map[string]interface{}{
			"name": p.name,
		},
	}
	hdrs, err := p.vm.BuildHeaders(p.adapter, meta)
	if err != nil {
		hdrs = p.vm.GetAdapterHeaders(p.adapter)
	}
	for k, v := range hdrs {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("Authorization") == "" && p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
}

func parseOpenAICompatibleResponse(raw []byte) (*CompletionResponse, error) {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          interface{}      `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	out := &CompletionResponse{
		TokenUsage: TokenUsage{
			Prompt:     resp.Usage.PromptTokens,
			Completion: resp.Usage.CompletionTokens,
			Total:      resp.Usage.TotalTokens,
		},
	}
	if len(resp.Choices) == 0 {
		return out, nil
	}
	ch := resp.Choices[0]
	out.FinishReason = ch.FinishReason
	out.Content = stringifyContent(ch.Message.Content)
	out.ReasoningContent = ch.Message.ReasoningContent
	out.ToolCalls = normalizeOpenAIToolCalls(ch.Message.ToolCalls)
	return out, nil
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string      `json:"name"`
		Arguments interface{} `json:"arguments"`
	} `json:"function"`
	Name      string      `json:"name"`
	Arguments interface{} `json:"arguments"`
}

func normalizeOpenAIToolCalls(raw []openAIToolCall) []ToolCall {
	if len(raw) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(raw))
	for _, tc := range raw {
		name := tc.Function.Name
		argsRaw := tc.Function.Arguments
		if name == "" {
			name = tc.Name
			argsRaw = tc.Arguments
		}
		if name == "" {
			continue
		}
		typ := tc.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, ToolCall{
			ID:        tc.ID,
			Type:      typ,
			Name:      name,
			Arguments: parseToolArguments(argsRaw),
		})
	}
	return out
}

func parseToolArguments(v interface{}) map[string]interface{} {
	switch x := v.(type) {
	case nil:
		return map[string]interface{}{}
	case map[string]interface{}:
		return x
	case string:
		if strings.TrimSpace(x) == "" {
			return map[string]interface{}{}
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(x), &m); err == nil && m != nil {
			return m
		}
		var any interface{}
		if err := json.Unmarshal([]byte(x), &any); err == nil {
			return map[string]interface{}{"value": any}
		}
		return map[string]interface{}{"raw": x}
	default:
		b, _ := json.Marshal(x)
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err == nil && m != nil {
			return m
		}
		return map[string]interface{}{"value": x}
	}
}

func stringifyContent(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []interface{}:
		var b strings.Builder
		for _, part := range x {
			if m, ok := part.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// parseOpenAICompatibleStreamChunkFull 解析标准 OpenAI SSE 块（含 usage 字段）。
// 兼容多种 token 用量键名（prompt_tokens/prompt、total_tokens/total 等）
// 与 prompt cache 细节字段。返回 false 表示非内容块（纯 usage 心跳等）。
func parseOpenAICompatibleStreamChunkFull(data string) (StreamChunk, bool) {
	var raw struct {
		Choices []struct {
			Delta struct {
				Content          interface{}      `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []openAIToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		UpstreamUsage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			Prompt              int `json:"prompt"`
			Completion          int `json:"completion"`
			Total               int `json:"total"`
			PromptCacheHit      int `json:"prompt_cache_hit_tokens"`
			PromptCacheMiss     int `json:"prompt_cache_miss_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return StreamChunk{}, false
	}

	var usage *TokenUsage
	pu := raw.UpstreamUsage
	if pu.Total > 0 || pu.TotalTokens > 0 || pu.Prompt > 0 || pu.PromptTokens > 0 {
		usage = &TokenUsage{
			Prompt:     pickFirstInt(pu.PromptTokens, pu.Prompt),
			Completion: pickFirstInt(pu.CompletionTokens, pu.Completion),
			Total:      pickFirstInt(pu.TotalTokens, pu.Total),
		}
	}

	if len(raw.Choices) == 0 {
		// 纯 usage 心跳块：有 usage 就透传，否则丢弃
		if usage != nil {
			return StreamChunk{Usage: usage}, true
		}
		return StreamChunk{}, false
	}

	choice := raw.Choices[0]
	ck := StreamChunk{
		Content:          stringifyContent(choice.Delta.Content),
		ReasoningContent: choice.Delta.ReasoningContent,
		ToolCalls:        normalizeOpenAIToolCalls(choice.Delta.ToolCalls),
		Usage:            usage,
	}
	// finish reason 为空字符串不算终止信号（sensenova 每块都发 ""）
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		ck.Done = true
		ck.FinishReason = *choice.FinishReason
	}
	return ck, true
}

// pickFirstInt 返回 a 非零时的 a，否则 b（兼容 *_tokens 与短键名两种 usage 格式）。
func pickFirstInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// streamHTTPClient 返回专用的流式 HTTP client（懒初始化）。
// SSE 长连接不能套整体超时（非流式 180s 会在长流中途报断），
// 只保留拨号/握手超时。
func (p *LuaAdaptedProvider) streamHTTPClient() *http.Client {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if p.streamClient == nil {
		p.streamClient = &http.Client{
			Timeout: 0, // 无整体超时：SSE 流持续时间不可预知
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2: true,
				MaxIdleConns:      10,
				IdleConnTimeout:   90 * time.Second,
			},
		}
	}
	return p.streamClient
}

// errorOnlyChunk 判断一个流块是否只携带上游错误信号：done 块带非标准
// finish_reason 且无任何内容/工具调用/推理文本。标准 OpenAI finish reason
// 不算错误，正常的空补全（finish_reason:"stop" 无输出）仍会送达调用方。
func errorOnlyChunk(ck StreamChunk) bool {
	if !ck.Done || ck.FinishReason == "" {
		return false
	}
	switch ck.FinishReason {
	case "stop", "length", "tool_calls", "function_call", "content_filter":
		return false
	}
	return ck.Content == "" && len(ck.ToolCalls) == 0 && ck.ReasoningContent == ""
}

func (p *LuaAdaptedProvider) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	if p.cfg.Model != "" && (req.Model == "" || req.Model == "AUTO") {
		req.Model = p.cfg.Model
	}
	req.Stream = true
	rawReq, _ := json.Marshal(req)

	transformedBody, err := p.vm.CallTransformRequest(p.adapter, string(rawReq))
	if err != nil {
		return nil, fmt.Errorf("lua transform_request (stream): %w", err)
	}

	endpoint := p.vm.GetAdapterEndpoint(p.adapter)
	if endpoint == "" {
		endpoint = "/chat/completions"
	}
	url := strings.TrimRight(p.cfg.BaseURL, "/") + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(transformedBody))
	if err != nil {
		return nil, fmt.Errorf("create stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 与 Chat() 一致走 applyAdapterHeaders：支持 build_headers 动态签名钩子
	p.applyAdapterHeaders(httpReq, url, transformedBody)

	resp, err := p.streamHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stream api: %w", err)
	}

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// transform_error 钩子优先（适配器层协议知识）；未定义时回退标准解析
		if reason, ok, _ := p.vm.TransformError(p.adapter, resp.StatusCode, string(raw)); ok && strings.TrimSpace(reason) != "" {
			return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, truncateOneLineStr(reason, 200))
		}
		return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, truncateOneLineStr(string(raw), 300))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var doneSent bool // 适配器已发过带真实 finish_reason 的终止块则不重复发 [DONE]

		emit := func(ck StreamChunk) bool {
			if ck.Done {
				doneSent = true
			}
			select {
			case ch <- ck:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				if !doneSent {
					if !emit(StreamChunk{Done: true}) {
						return
					}
				}
				continue
			}

			// Lua transform_stream_chunk 优先；透传/无钩子时用标准解析
			unified, terr := p.vm.CallTransformStreamChunk(p.adapter, data)
			var ck StreamChunk
			if terr == nil && unified != "" && unified != data {
				if json.Unmarshal([]byte(unified), &ck) != nil {
					continue
				}
			} else {
				parsed, ok := parseOpenAICompatibleStreamChunkFull(data)
				if !ok {
					continue
				}
				ck = parsed
			}
			if !emit(ck) {
				return
			}
		}

		// 干净 EOF 但无 done 块：补一个，保证消费方能收到终止信号
		if !doneSent && ctx.Err() == nil {
			select {
			case ch <- StreamChunk{Done: true}:
			default:
			}
		}
	}()

	// 扣住首块校验流是否真的携带内容：部分上游返回 HTTP 200 但流里只有
	// 错误 finish_reason 的退化块（如 zen 免费池 network_error）。在这里
	// 失败该候选，让上层 fallback 到下一源，而不是给客户端吐空响应。
	select {
	case first, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("provider %s: empty stream", p.Name())
		}
		if errorOnlyChunk(first) {
			go func() {
				for range ch { //nolint:revive
				} // 排空避免生产 goroutine 阻塞泄漏
			}()
			return nil, fmt.Errorf("provider %s: upstream returned %q stream", p.Name(), first.FinishReason)
		}
		out := make(chan StreamChunk, 64)
		go func() {
			defer close(out)
			out <- first
			for ck := range ch {
				out <- ck
			}
		}()
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// truncateOneLineStr 截断为单行且限制最大长度（用于错误消息防 HTML dump 泄漏）。
func truncateOneLineStr(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

type providerStatus struct {
	failCount        int
	unavailableUntil time.Time
	permanent        bool // 401/403 永久不可用，不自动恢复
}

type ProviderManager struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string
	default_  string
	status    map[string]*providerStatus
}

const (
	providerCooldownBase = 30 * time.Second
	providerCooldownMax  = 30 * time.Minute
)

func NewProviderManager() *ProviderManager {
	return &ProviderManager{
		providers: make(map[string]Provider),
		status:    make(map[string]*providerStatus),
	}
}

func (m *ProviderManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers = make(map[string]Provider)
	m.order = nil
	m.default_ = ""
	m.status = make(map[string]*providerStatus)
}

func (m *ProviderManager) Register(name string, p Provider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers[name] = p
	m.order = append(m.order, name)
	if m.default_ == "" {
		m.default_ = name
	}
}

func (m *ProviderManager) SetDefault(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[name]; !ok {
		return fmt.Errorf("provider %s not found", name)
	}
	m.default_ = name
	return nil
}

func (m *ProviderManager) Get(name string) Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if name == "" {
		name = m.default_
	}
	return m.providers[name]
}

func (m *ProviderManager) Default() Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providers[m.default_]
}

// QuickChat 向默认 LLM Provider 发送一条简短消息并返回回复。
// 这是一个"非记忆"调用——直接通过 Provider HTTP 调用，不经过 Agent 的记忆/蒸馏管线。
// 适用于健康检查、系统自检等不需要产生记忆碎片的场景。
func (m *ProviderManager) QuickChat(ctx context.Context, prompt string) (*CompletionResponse, error) {
	p := m.Default()
	if p == nil {
		return nil, fmt.Errorf("no default provider")
	}
	return p.Chat(ctx, &CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: prompt},
		},
		MaxTokens: 128,
	})
}

func (m *ProviderManager) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, len(m.order))
	copy(names, m.order)
	return names
}

func (m *ProviderManager) MarkUnavailable(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[name]
	if st == nil {
		st = &providerStatus{}
		m.status[name] = st
	}
	st.failCount++
	cooldown := providerCooldownBase * time.Duration(1<<(st.failCount-1))
	if cooldown > providerCooldownMax {
		cooldown = providerCooldownMax
	}
	st.unavailableUntil = time.Now().Add(cooldown)
}

// ReportStatus records an HTTP status code for a provider.
// 401/403 = credential error → permanently unavailable (never retry).
// Other codes → MarkUnavailable with exponential backoff.
func (m *ProviderManager) ReportStatus(name string, statusCode int) {
	if statusCode == 401 || statusCode == 403 {
		m.mu.Lock()
		defer m.mu.Unlock()
		st := m.status[name]
		if st == nil {
			st = &providerStatus{}
			m.status[name] = st
		}
		st.permanent = true
		st.unavailableUntil = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		return
	}
	m.MarkUnavailable(name)
}

func (m *ProviderManager) ResetAvailability(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.status, name)
}

func (m *ProviderManager) MarkPermanent(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[name]
	if st == nil {
		st = &providerStatus{}
		m.status[name] = st
	}
	st.permanent = true
	st.unavailableUntil = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
}

func (m *ProviderManager) IsAvailable(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.status[name]
	if !ok {
		return true
	}
	if st.permanent {
		return false
	}
	return time.Now().After(st.unavailableUntil)
}

func (m *ProviderManager) OrderedProviders() []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.orderedLocked("")
}

// orderedLocked 返回 provider 候选链。model 为 "" 表示 AUTO：
// 按 (优先级 desc, 可用性, 默认优先) 稳定排序；model 非空且存在归属源时，
// 命中的源排在最前（精确模型路由），其余按优先级跟随。
func (m *ProviderManager) orderedLocked(model string) []Provider {
	list := make([]Provider, 0, len(m.order))
	for _, name := range m.order {
		if p, ok := m.providers[name]; ok && p != nil {
			list = append(list, p)
		}
	}
	type pp struct {
		p       Provider
		prio    int
		isDef   bool
		isMatch bool
	}
	items := make([]pp, 0, len(list))
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, p := range list {
		it := pp{p: p, prio: 0, isDef: p.Name() == m.default_}
		if rp, ok := p.(RoutableProvider); ok {
			it.prio = rp.Priority()
			if lower != "" && lower != "auto" {
				if strings.EqualFold(rp.Model(), model) {
					it.isMatch = true
				}
			}
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].isMatch != items[j].isMatch {
			return items[i].isMatch
		}
		if items[i].prio != items[j].prio {
			return items[i].prio > items[j].prio
		}
		if items[i].isDef != items[j].isDef {
			return items[i].isDef
		}
		return items[i].p.Name() < items[j].p.Name()
	})
	out := make([]Provider, len(items))
	for i := range items {
		out[i] = items[i].p
	}
	return out
}

// ResolveForModel 按精确模型名路由到归属 provider；找不到则回落到 AUTO 链。
func (m *ProviderManager) ResolveForModel(model string) []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.orderedLocked(model)
}

func (m *ProviderManager) ProviderCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.providers)
}

type rawToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ProviderError wraps an HTTP-level error with status code for precise auth detection.
type ProviderError struct {
	StatusCode int
	Message    string
}

func (e *ProviderError) Error() string {
	return e.Message
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return 0
}

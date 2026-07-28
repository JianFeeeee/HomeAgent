package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function apiFunction    `json:"function"`
}

type apiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type StreamChunk struct {
	Content   string `json:"content"`
	Done      bool   `json:"done"`
	ToolCall  *ToolCall `json:"tool_call,omitempty"`
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
	ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
	MaxContextTokens() int
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
}

// LuaAdaptedProvider 使用 Lua 脚本做请求/响应变换，直接发起 HTTP 调用
// 不再包裹其他 Provider，协议差异全部在 Lua 层处理
type LuaAdaptedProvider struct {
	name    string
	cfg     BaseConfig
	vm      *luaVM.VM
	adapter string
	client  *http.Client
}

func NewLuaAdaptedProvider(cfg BaseConfig, vm *luaVM.VM, adapter string) *LuaAdaptedProvider {
	if cfg.Temperature == 0 {
		cfg.Temperature = 0.7
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
	}
	return &LuaAdaptedProvider{
		name:    fmt.Sprintf("lua_%s", adapter),
		cfg:     cfg,
		vm:      vm,
		adapter: adapter,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *LuaAdaptedProvider) MaxContextTokens() int {
	if p.cfg.ContextWindow > 0 {
		return p.cfg.ContextWindow
	}
	return ModelContextWindow(p.cfg.Model)
}

func (p *LuaAdaptedProvider) Name() string { return p.name }

func (p *LuaAdaptedProvider) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	req.Model = p.cfg.Model

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
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	for k, v := range p.vm.GetAdapterHeaders(p.adapter) {
		httpReq.Header.Set(k, v)
	}

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
		return nil, fmt.Errorf("lua transform_response: %w", err)
	}

	var result CompletionResponse
	if err := json.Unmarshal([]byte(unifiedJSON), &result); err != nil {
		return nil, fmt.Errorf("unmarshal unified response: %w (body: %s)", err, unifiedJSON)
	}

	return &result, nil
}

func (p *LuaAdaptedProvider) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	req.Model = p.cfg.Model
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
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	for k, v := range p.vm.GetAdapterHeaders(p.adapter) {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stream api: %w", err)
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := NewSSEScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			// 尝试用 Lua 变换流块（如果 adapter 定义了 transform_stream_chunk）
			unified, err := p.vm.CallTransformStreamChunk(p.adapter, line)
			if err != nil || unified == line {
				// 无流变换函数或变换透传，尝试标准 SSE 解析
				var raw struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(unified), &raw); err != nil {
					continue
				}
				if len(raw.Choices) > 0 {
					select {
					case ch <- StreamChunk{
						Content: raw.Choices[0].Delta.Content,
						Done:    raw.Choices[0].FinishReason != nil,
					}:
					case <-ctx.Done():
						return
					}
				}
				continue
			}

			// Lua 返回了变换后的统一格式
			var chunk StreamChunk
			if err := json.Unmarshal([]byte(unified), &chunk); err == nil {
				select {
				case ch <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// SSEScanner 读取 SSE 格式的流（data: ...）
type SSEScanner struct {
	reader  *bufio.Reader
	pending string
}

func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{reader: bufio.NewReader(r)}
}

func (s *SSEScanner) Scan() bool {
	s.pending = ""
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return false
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			s.pending = strings.TrimPrefix(line, "data: ")
			if s.pending == "[DONE]" {
				return false
			}
			return true
		}
	}
}

func (s *SSEScanner) Text() string { return s.pending }

type providerStatus struct {
	failCount    int
	unavailableUntil time.Time
	permanent    bool // 401/403 永久不可用，不自动恢复
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
	list := make([]Provider, 0, len(m.order))
	// 把默认 provider 放第一位，其余按注册顺序
	if def, ok := m.providers[m.default_]; ok {
		list = append(list, def)
	}
	for _, name := range m.order {
		if name == m.default_ {
			continue
		}
		if p, ok := m.providers[name]; ok {
			list = append(list, p)
		}
	}
	return list
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

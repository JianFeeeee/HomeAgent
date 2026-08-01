package sdk

import (
	"context"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// LLMAPI 是内置插件使用的全量 LLM Provider 接口。
// 内置 SDK 为全量接口，外部 SDK 仅暴露其中的安全子集（pubsdk.LLMAPI）。
type LLMAPI interface {
	pubsdk.LLMAPI
	// ReloadFromConfig 从内核配置重建所有 LLM Provider。
	ReloadFromConfig() error
	// Chat 向默认 Provider 发起一次补全调用（非记忆管线）。
	Chat(ctx context.Context, req *LLMCompletionRequest) (*LLMCompletionResponse, error)
}

// LLMMessage 是面向 LLM Provider 的中立对话消息。
type LLMMessage struct {
	Role             string        `json:"role"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
}

// LLMToolCall 是中立的工具调用请求。
type LLMToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// LLMTokenUsage 报告一次补全的 token 消耗。
type LLMTokenUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// LLMCompletionRequest 是中立的补全请求。
// Tools/ToolChoice 使用 OpenAI 风格载荷，便于 Provider 直接转发。
type LLMCompletionRequest struct {
	Model           string        `json:"model,omitempty"`
	Messages        []LLMMessage  `json:"messages"`
	Temperature     float64       `json:"temperature,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Stream          bool          `json:"stream,omitempty"`
	Tools           []interface{} `json:"tools,omitempty"`
	ToolChoice      interface{}   `json:"tool_choice,omitempty"`
	DisableThinking bool          `json:"disable_thinking,omitempty"`
}

// LLMCompletionResponse 是中立的补全响应。
type LLMCompletionResponse struct {
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	FinishReason     string        `json:"finish_reason,omitempty"`
	TokenUsage       LLMTokenUsage `json:"token_usage,omitempty"`
	ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
}

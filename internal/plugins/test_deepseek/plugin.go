package test_deepseek

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/plugin/sdk"
)

var defaultClient = &http.Client{Timeout: 30 * time.Second}

func New(bus sdk.EventBus) *sdk.PluginAPI {
	api := sdk.NewPluginAPI("test_deepseek", "1.0.0", bus, nil, nil)

	api.RegisterTool("test_deepseek", func(args map[string]interface{}) (interface{}, error) {
		prompt, _ := args["prompt"].(string)
		if prompt == "" {
			prompt = "你好，请用一句话介绍你自己"
		}
		return callDeepSeek(prompt, api.Settings(), defaultClient)
	})

	return api
}

func callDeepSeek(prompt string, sett sdk.SettingsAPI, client *http.Client) (interface{}, error) {
	baseURL := "https://api.deepseek.com/v1"
	model := "deepseek-chat"
	apiKey := ""
	if sett != nil {
		if v, err := sett.Get("base_url"); err == nil {
			if s, ok := v.(string); ok && s != "" {
				baseURL = s
			}
		}
		if v, err := sett.Get("model"); err == nil {
			if s, ok := v.(string); ok && s != "" {
				model = s
			}
		}
		if v, err := sett.Get("api_key"); err == nil {
			if s, ok := v.(string); ok && s != "" {
				apiKey = s
			}
		}
	}
	if apiKey == "" {
		apiKey = "REDACTED_API_KEY"
	}

	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.7,
		"max_tokens":  1024,
		"stream":      false,
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", strings.TrimRight(baseURL, "/")+"/chat/completions", strings.NewReader(string(bodyJSON)))
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("创建请求失败: %v", err)}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("API 调用失败: %v", err)}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return map[string]interface{}{
			"error":  fmt.Sprintf("API 返回 %d", resp.StatusCode),
			"body":   string(respBody),
			"status": "failed",
		}, nil
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return map[string]interface{}{"error": fmt.Sprintf("解析响应失败: %v", err)}, nil
	}

	content := ""
	if len(result.Choices) > 0 {
		content = result.Choices[0].Message.Content
	}

	return map[string]interface{}{
		"prompt":   prompt,
		"response": content,
		"model":    model,
		"usage":    result.Usage,
		"status":   "ok",
		"base_url": baseURL,
	}, nil
}

func NewWithClient(bus sdk.EventBus, client *http.Client) *sdk.PluginAPI {
	api := sdk.NewPluginAPI("test_deepseek", "1.0.0", bus, nil, nil)
	api.RegisterTool("test_deepseek", func(args map[string]interface{}) (interface{}, error) {
		prompt, _ := args["prompt"].(string)
		if prompt == "" {
			prompt = "你好，请用一句话介绍你自己"
		}
		return callDeepSeek(prompt, api.Settings(), client)
	})
	return api
}

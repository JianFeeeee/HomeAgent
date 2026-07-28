package core

import (
	"context"
	"fmt"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
)

func (a *Agent) mediaDataURL(defaultMime string) string {
	if a.pendingMedia == nil {
		return ""
	}
	data, _ := a.pendingMedia["data"].(string)
	mime, _ := a.pendingMedia["mime"].(string)
	url, _ := a.pendingMedia["url"].(string)
	if data != "" {
		if mime == "" {
			mime = defaultMime
		}
		return "data:" + mime + ";base64," + data
	}
	return url
}

func (a *Agent) mediaRequest(p agentAPI.Provider, mime, emptyPendingMsg, emptyDataMsg, prompt, resultPrefix string, maxTokens int, blockType string, detail string) string {
	if a.pendingMedia == nil {
		return emptyPendingMsg
	}
	url := a.mediaDataURL(mime)
	if url == "" {
		return emptyDataMsg
	}
	msg := agentAPI.Message{
		Role: "user",
		Blocks: []agentAPI.ContentBlock{
			{Type: "text", Text: prompt},
		},
	}
	if blockType == "image_url" {
		msg.Blocks = append(msg.Blocks, agentAPI.ContentBlock{
			Type:     "image_url",
			ImageURL: &agentAPI.ImageURL{URL: url, Detail: detail},
		})
	} else {
		msg.Blocks = append(msg.Blocks, agentAPI.ContentBlock{
			Type:     "audio_url",
			AudioURL: &agentAPI.AudioURL{URL: url},
		})
	}
	return a.mediaChat(p, msg, resultPrefix, maxTokens)
}

func (a *Agent) mediaChat(p agentAPI.Provider, msg agentAPI.Message, resultPrefix string, maxTokens int) string {
	ctx, cancel := context.WithTimeout(a.ctx, 120*time.Second)
	defer cancel()
	resp, err := p.Chat(ctx, &agentAPI.CompletionRequest{
		Messages:  []agentAPI.Message{msg},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return fmt.Sprintf("%s失败: %v", resultPrefix, err)
	}
	return fmt.Sprintf("[%s] %s", resultPrefix, resp.Content)
}

func (a *Agent) executeDescribeImage(tc agentAPI.ToolCall) string {
	providerName, _ := tc.Arguments["provider"].(string)
	p := a.providerManager.Get(providerName)
	if p == nil {
		p = a.provider
	}
	detail, _ := tc.Arguments["detail"].(string)
	if detail == "" {
		detail = "high"
	}
	return a.mediaRequest(p, "image/png", "没有待处理的图片数据", "图片数据为空",
		a.inputCfg.Image.DescribePrompt, "图片描述", 2048, "image_url", detail)
}

func (a *Agent) executeTranscribeAudio(tc agentAPI.ToolCall) string {
	providerName, _ := tc.Arguments["provider"].(string)
	p := a.providerManager.Get(providerName)
	if p == nil {
		p = a.provider
	}
	return a.mediaRequest(p, "audio/wav", "没有待处理的音频数据", "音频数据为空",
		a.inputCfg.Audio.DescribePrompt, "音频转写", 2048, "audio_url", "")
}

func (a *Agent) executeOCRImage(tc agentAPI.ToolCall) string {
	return a.mediaRequest(a.provider, "image/png", "没有待处理的图片数据", "图片数据为空",
		a.inputCfg.Image.OCRPrompt, "OCR 结果", 4096, "image_url", "high")
}

package multimodal

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

func init() {
	plugin.RegisterPluginMeta("multimodal", "多模态感知", "Multimodal Perception")
	plugin.RegisterFactory("multimodal", NewPluginFactory)
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

type Plugin struct {
	name string
	sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	tp := p.name + "_"

	s.RegisterTool(tp+"see_picture", sdk.ToolDef{
		Name:        tp + "see_picture",
		Description: "让模型看到一张图片。输入文件路径或 URL，图片以 image_url 格式注入后续对话，模型可看到并描述/分析图片内容。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "图片的本地文件路径或 HTTP URL",
				},
			},
			"required": []string{"path"},
		},
	}, p.handleSeePicture)

	s.RegisterTool(tp+"see_video", sdk.ToolDef{
		Name:        tp + "see_video",
		Description: "让模型看到一段视频的关键帧。输入视频文件路径，ffmpeg 提取 N 帧作为 image_url 注入后续对话，模型可分析视频内容。需要 ffmpeg 已安装。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "视频的本地文件路径",
				},
				"frames": map[string]interface{}{
					"type":        "integer",
					"description": "提取关键帧数量（默认 4，最大 10）",
				},
			},
			"required": []string{"path"},
		},
	}, p.handleSeeVideo)

	s.RegisterTool(tp+"listen", sdk.ToolDef{
		Name:        tp + "listen",
		Description: "让模型听到一段音频。输入音频文件路径（mp3/wav/ogg/m4a），音频注入后续对话，支持音频的模型可识别语音/声音内容。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "音频文件路径",
				},
			},
			"required": []string{"path"},
		},
	}, p.handleListen)

	log.Printf("[%s] multimodal perception tools registered", p.name)
	return nil
}

func (p *Plugin) Stop() error { return nil }

// ── see_picture ──────────────────────────────────────────────────

func (p *Plugin) handleSeePicture(args map[string]interface{}) (interface{}, error) {
	path := getArgStr(args, "path")
	if path == "" {
		return "path is required", nil
	}

	var dataURL string
	var mime string

	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		// 远程 URL：直接用作 image URL，不下载
		dataURL = path
		mime = "image/png"
	} else {
		// 本地文件
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Sprintf("文件不存在: %s", path), nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".jpg", ".jpeg":
			mime = "image/jpeg"
		case ".gif":
			mime = "image/gif"
		case ".webp":
			mime = "image/webp"
		default:
			mime = "image/png"
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("读取文件失败: %v", err), nil
		}
		// 检查大小上限（3MB，防止 context 爆炸）
		if len(b) > 3*1024*1024 {
			return fmt.Sprintf("图片过大（%d bytes，超过 3MB），无法注入上下文", len(b)), nil
		}
		dataURL = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
	}

	// 注入多模态块：模型下一轮可看到图片
	p.sdk.SetToolBlocks([]pubsdk.ContentBlock{
		{Type: "image_url", ImageURL: &pubsdk.ImageURL{URL: dataURL, Detail: "auto"}},
	})

	text := fmt.Sprintf("[已将图片注入后续对话] %s", path)
	return text, nil
}

// ── see_video ────────────────────────────────────────────────────

func (p *Plugin) handleSeeVideo(args map[string]interface{}) (interface{}, error) {
	path := getArgStr(args, "path")
	if path == "" {
		return "path is required", nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Sprintf("文件不存在: %s", path), nil
	}

	// 检查 ffmpeg
	ffmpegPath := ""
	for _, c := range []string{"ffmpeg", "/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if _, err := os.Stat(c); err == nil {
			ffmpegPath = c
			break
		}
	}
	if ffmpegPath == "" {
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = "ffmpeg"
		} else {
			return "ffmpeg 未安装，无法提取视频关键帧。请先安装: apt install ffmpeg", nil
		}
	}

	nFrames := 4
	if n, ok := args["frames"].(float64); ok && n > 0 {
		nFrames = int(n)
		if nFrames > 10 {
			nFrames = 10
		}
	}

	// 用 ffmpeg 提取关键帧
	tmpDir, err := os.MkdirTemp("", "mm_video_*")
	if err != nil {
		return fmt.Sprintf("创建临时目录失败: %v", err), nil
	}
	defer os.RemoveAll(tmpDir)

	outPattern := filepath.Join(tmpDir, "frame_%03d.jpg")
	cmd := exec.Command(ffmpegPath, "-i", path, "-vf", fmt.Sprintf("fps=1/%d", nFrames),
		"-q:v", "5", outPattern)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("ffmpeg 提取帧失败: %v\n%s", err, string(out)), nil
	}

	// 读取提取的帧
	entries, _ := os.ReadDir(tmpDir)
	var blocks []pubsdk.ContentBlock
	for i, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jpg") {
			b, err := os.ReadFile(filepath.Join(tmpDir, entry.Name()))
			if err != nil {
				continue
			}
			if len(b) > 2*1024*1024 {
				continue // 跳过过大帧
			}
			dURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b)
			blocks = append(blocks, pubsdk.ContentBlock{
				Type: "image_url",
				ImageURL: &pubsdk.ImageURL{URL: dURL, Detail: "low"},
			})
			if i >= 9 { // 最多 10 帧
				break
			}
		}
	}

	if len(blocks) == 0 {
		return "视频中未提取到有效帧", nil
	}

	// 全部帧注入（一次 SetToolBlocks 调用，下一轮 LLM 可看到）
	p.sdk.SetToolBlocks(blocks)

	text := fmt.Sprintf("[已将 %d 个视频关键帧注入后续对话] %s", len(blocks), path)
	return text, nil
}

// ── listen ───────────────────────────────────────────────────────

func (p *Plugin) handleListen(args map[string]interface{}) (interface{}, error) {
	path := getArgStr(args, "path")
	if path == "" {
		return "path is required", nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Sprintf("文件不存在: %s", path), nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	var mime string
	switch ext {
	case ".mp3":
		mime = "audio/mpeg"
	case ".wav":
		mime = "audio/wav"
	case ".ogg":
		mime = "audio/ogg"
	case ".m4a", ".aac":
		mime = "audio/mp4"
	default:
		mime = "audio/ogg" // 默认
	}

	// 检查大小（5MB 限制，避免上下文爆炸）
	info, _ := os.Stat(path)
	if info != nil && info.Size() > 5*1024*1024 {
		return fmt.Sprintf("音频过大（%d bytes，超过 5MB），无法注入上下文", info.Size()), nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("读取音频文件失败: %v", err), nil
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)

	p.sdk.SetToolBlocks([]pubsdk.ContentBlock{
		{Type: "audio_url", AudioURL: &pubsdk.AudioURL{URL: dataURL}},
	})

	text := fmt.Sprintf("[已将音频注入后续对话] %s（%s，%.1fKB）", path, mime, float64(len(b))/1024)
	return text, nil
}

// ── helpers ──────────────────────────────────────────────────────

func getArgStr(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

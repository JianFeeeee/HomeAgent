//go:build onnxruntime

// Package qwen 的 ONNX 推理实现（构建标签 onnxruntime，与 internal/memory/clip 同模式）。
//
// 加载契约：调用方传入模型目录，内核不硬编码模型名。
//
//	TextTower.onnx + 外部权重分片 — 文本塔图（input_ids/attention_mask → embedding）
//	tokenizer.json                — 字节级 BPE 词表与 merges
//	embed_config.json             — dim / max_length / instruction / pooling
//
// 图内已含 last-token 池化，输出即 [batch, dim]；L2 归一化在 Go 侧做（便宜且便于测试）。
package qwen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// embedConfig 对应导出脚本产出的 embed_config.json。
type embedConfig struct {
	Dimension   int    `json:"dim"`
	MaxLength   int    `json:"max_length"`
	Instruction string `json:"instruction"`
	Pooling     string `json:"pooling"`
}

// Embedder 是基于内嵌 ONNX 文本塔的稠密嵌入器。
//
// 只提供文本能力：导出的是文本塔，视觉塔未导出。EmbedImageDense 会明确报错，
// 而不是返回一个「看起来能用」的零向量——后者会让跨模态检索静默失效。
type Embedder struct {
	mu sync.RWMutex

	loaded bool
	config embedConfig
	tok    *Tokenizer
	sess   *ort.DynamicAdvancedSession
	fp     string
}

// New 从模型目录加载文本塔。
func New(modelDir string) (*Embedder, error) {
	if modelDir == "" {
		return nil, fmt.Errorf("qwen model dir not specified")
	}

	cfgRaw, err := os.ReadFile(filepath.Join(modelDir, "embed_config.json"))
	if err != nil {
		return nil, fmt.Errorf("read embed_config.json: %w", err)
	}
	var cfg embedConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return nil, fmt.Errorf("parse embed_config.json: %w", err)
	}
	if cfg.Dimension <= 0 {
		return nil, fmt.Errorf("embed_config.json 的 dim 无效: %d", cfg.Dimension)
	}
	if cfg.MaxLength <= 0 {
		cfg.MaxLength = 512
	}
	if cfg.Pooling != "" && cfg.Pooling != "last_token" {
		return nil, fmt.Errorf("不支持的池化方式 %q（导出脚本只产出 last_token）", cfg.Pooling)
	}

	tok, err := LoadTokenizer(modelDir)
	if err != nil {
		return nil, err
	}
	tok.MaxLen = cfg.MaxLength

	if !ort.IsInitialized() {
		if lib := findOnnxLib(); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("init onnx env: %w", err)
		}
	}

	sess, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "TextTower.onnx"),
		[]string{"input_ids", "attention_mask"},
		[]string{"embedding"},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create text tower session: %w", err)
	}

	return &Embedder{
		loaded: true,
		config: cfg,
		tok:    tok,
		sess:   sess,
		fp:     computeFingerprint(modelDir),
	}, nil
}

// renderInput 按模型自带的对话模板拼输入（实现在 tokenizer.go，无构建标签）。
func (e *Embedder) renderInput(text string) string {
	return renderInstructionInput(e.config.Instruction, text)
}

// VectorizeDense 把文本编码为 L2 归一化的稠密向量。
func (e *Embedder) VectorizeDense(text string) ([]float64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.loaded {
		return nil, fmt.Errorf("qwen embedder not loaded")
	}

	ids, err := e.tok.encodeModelInput(e.renderInput(text), e.config.MaxLength)
	if err != nil {
		return nil, err
	}

	seq := len(ids)
	inputIDs := make([]int64, seq)
	attn := make([]int64, seq)
	for i, id := range ids {
		inputIDs[i] = int64(id)
		attn[i] = 1
	}

	idTensor, err := ort.NewTensor(ort.Shape{1, int64(seq)}, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("input_ids tensor: %w", err)
	}
	defer idTensor.Destroy()

	maskTensor, err := ort.NewTensor(ort.Shape{1, int64(seq)}, attn)
	if err != nil {
		return nil, fmt.Errorf("attention_mask tensor: %w", err)
	}
	defer maskTensor.Destroy()

	outTensor, err := ort.NewEmptyTensor[float32](ort.Shape{1, int64(e.config.Dimension)})
	if err != nil {
		return nil, fmt.Errorf("output tensor: %w", err)
	}
	defer outTensor.Destroy()

	if err := e.sess.Run([]ort.Value{idTensor, maskTensor}, []ort.Value{outTensor}); err != nil {
		return nil, fmt.Errorf("text tower run: %w", err)
	}

	raw := outTensor.GetData()
	out := make([]float64, len(raw))
	var norm float64
	for i, v := range raw {
		out[i] = float64(v)
		norm += out[i] * out[i]
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range out {
			out[i] /= norm
		}
	}
	return out, nil
}

// EmbedImageDense 不支持：导出的是**文本塔**，视觉塔未导出。
//
// 明确报错而不是返回零向量或占位：调用方（mediaref.go）会 log 后跳过写向量，
// 若返回零向量则「写入了但检索不到」，失败会静默化。要支持图像检索需另外
// 导出视觉塔并实现 Qwen3-VL 的图像预处理（patch/merge/缩放规则）。
func (e *Embedder) EmbedImageDense(_ []byte, _ string) ([]float64, error) {
	return nil, fmt.Errorf("qwen text tower 不支持图像嵌入；图像检索请用 clip 或 http 路径")
}

func (e *Embedder) Fingerprint() string { return e.fp }
func (e *Embedder) Dim() int            { return e.config.Dimension }

func (e *Embedder) Loaded() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loaded
}

func (e *Embedder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess != nil {
		e.sess.Destroy()
		e.sess = nil
	}
	e.loaded = false
}

// computeFingerprint 计算模型指纹，用于 vec_model 持久化与切换后重算判定。
//
// 为什么不直接哈希全部权重：这个模型目录有 6.5GB 外部权重分片，启动时读一遍
// 要几十秒，会阻塞 homeagent 启动。这里哈希「图文件 + 配置 + 全部外部权重的
// 文件名与大小」——换模型（哪怕只是换了权重）几乎必然改变文件集合或大小，
// 足以识别切换；代价是理论上存在「大小相同但内容不同」的漏判，对本地单机
// 部署可接受。
func computeFingerprint(modelDir string) string {
	h := sha256.New()

	for _, name := range []string{"TextTower.onnx", "embed_config.json"} {
		if data, err := os.ReadFile(filepath.Join(modelDir, name)); err == nil {
			h.Write([]byte(name))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
	}

	entries, _ := os.ReadDir(modelDir)
	var names []string
	for _, e := range entries {
		n := e.Name()
		// 外部权重分片：torch 新版导出器使用 onnx__<op>_<id> 与模型张量同名文件。
		if strings.HasPrefix(n, "onnx__") || strings.HasSuffix(n, ".weight") || strings.HasSuffix(n, ".onnx.data") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		info, err := os.Stat(filepath.Join(modelDir, n))
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s:%d\n", n, info.Size())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// findOnnxLib 在常见路径中查找 libonnxruntime.so。
func findOnnxLib() string {
	for _, p := range []string{
		"/opt/onnxruntime/libonnxruntime.so",
		"/opt/onnxruntime/lib/libonnxruntime.so",
		"/usr/local/lib/libonnxruntime.so",
		"/usr/lib/libonnxruntime.so",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

//go:build onnxruntime

// Package clip 提供基于 CLIP ONNX 的多模态向量化器。
//
// 构建标签 onnxruntime 控制是否编译此实现（与 internal/nlp/onnx.go 同模式）。
// 未配置 clip_model_dir 时不会初始化 ONNX Runtime，现有 fastText/TF-IDF 行为不变。
//
// 支持的模型文件（统一放置于 clip_model_dir 目录）：
//
//	text.onnx   — CLIP 文本编码器（input_ids + attention_mask → text_features [1,512]）
//	vision.onnx — CLIP 图像编码器（pixel_values → image_features [1,512]）
//	clip_config.json — 模型元数据（dimension, context_length, image_size, mean, std）
//	tokenizer.json — HuggingFace tokenizer.json（含 vocab + merges）
//	merges.txt     — BPE merges 文件（CLIP 使用的字节级 BPE）
//
// 设计：同时满足 vector.Vectorizer 接口（稀疏 map，供文档检索复用）和直接返回
// []float64 的方法（供媒体嵌入与 QueryMedia 直接调用）。
package clip

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
	ort "github.com/yalue/onnxruntime_go"
)

// clipConfig 描述模型的超参数与归一化常数。
type clipConfig struct {
	Model         string    `json:"model"`
	Dimension     int       `json:"dimension"`
	ContextLength int       `json:"context_length"`
	ImageSize     int       `json:"image_size"`
	Mean          []float64 `json:"mean"`
	Std           []float64 `json:"std"`
}

// Embedder 实现 vector.Vectorizer，提供文本向量化与图像向量化。
type Embedder struct {
	mu          sync.RWMutex
	config      clipConfig
	vocab       map[string]int64
	merges      []string
	textSess    *ort.DynamicAdvancedSession
	imgSess     *ort.DynamicAdvancedSession
	close       sync.Once
	loaded      bool
	fingerprint string
}

// Fingerprint 返回当前模型目录的指纹（文本+视觉模型文件 SHA256 拼接），
// 用于检测模型切换后触发重算。
func (e *Embedder) Fingerprint() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.fingerprint
}

// Dim 返回向量维度。
func (e *Embedder) Dim() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.config.Dimension
}

// Loaded 返回加载状态。
func (e *Embedder) Loaded() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loaded
}

// Vectorize 将文本转为向量，供 vector.Vectorizer 接口使用（稀疏 map）。
func (e *Embedder) Vectorize(text string) vector.Vector {
	dense, err := e.VectorizeDense(text)
	if err != nil {
		log.Printf("[clip] Vectorize 失败: %v", err)
		return vector.Vector{}
	}
	return denseToVector(dense)
}

// EmbedImage 将图像字节转为向量，供 vector.Vectorizer 接口使用（稀疏 map）。
func (e *Embedder) EmbedImage(img []byte, mime string) (vector.Vector, error) {
	dense, err := e.EmbedImageDense(img, mime)
	if err != nil {
		return nil, err
	}
	return denseToVector(dense), nil
}

// VectorizeDense 将文本转为归一化的 []float64 向量（CLIP 共享空间）。
func (e *Embedder) VectorizeDense(text string) ([]float64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.loaded {
		return nil, fmt.Errorf("clip embedder not loaded")
	}

	tokens := tokenizeCLIP(text, e.vocab, e.merges, e.config.ContextLength)
	if len(tokens) == 0 {
		return make([]float64, e.config.Dimension), nil
	}

	dim := e.config.Dimension
	inputIDs := make([]int64, e.config.ContextLength)
	attnMask := make([]int64, e.config.ContextLength)
	for i, tok := range tokens {
		if i >= e.config.ContextLength {
			break
		}
		inputIDs[i] = tok
		attnMask[i] = 1
	}

	idTensor, err := ort.NewTensor(ort.Shape{1, int64(e.config.ContextLength)}, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("input_ids tensor: %w", err)
	}
	defer idTensor.Destroy()

	maskTensor, err := ort.NewTensor(ort.Shape{1, int64(e.config.ContextLength)}, attnMask)
	if err != nil {
		return nil, fmt.Errorf("attention_mask tensor: %w", err)
	}
	defer maskTensor.Destroy()

	featTensor, err := ort.NewEmptyTensor[float32](ort.Shape{1, int64(dim)})
	if err != nil {
		return nil, fmt.Errorf("output tensor: %w", err)
	}
	defer featTensor.Destroy()

	if err := e.textSess.Run([]ort.Value{idTensor, maskTensor}, []ort.Value{featTensor}); err != nil {
		return nil, fmt.Errorf("text run: %w", err)
	}

	raw := featTensor.GetData()
	out := make([]float64, dim)
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

// EmbedImageDense 将图像字节转为归一化的 []float64 向量（CLIP 共享空间）。
func (e *Embedder) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.loaded {
		return nil, fmt.Errorf("clip embedder not loaded")
	}
	return e.embedImageDenseUnlocked(img, mime)
}

func (e *Embedder) embedImageDenseUnlocked(img []byte, mime string) ([]float64, error) {
	decoded, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	size := e.config.ImageSize
	resized := resizeImage(decoded, size, size)

	pixels := make([]float32, 3*size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			r, g, b, _ := resized.At(x, y).RGBA()
			rf := float64(r) / 65535.0
			gf := float64(g) / 65535.0
			bf := float64(b) / 65535.0

			for c, v := range []float64{rf, gf, bf} {
				norm := (v - e.config.Mean[c]) / e.config.Std[c]
				pixels[c*size*size+y*size+x] = float32(norm)
			}
		}
	}

	pixelTensor, err := ort.NewTensor(ort.Shape{1, 3, int64(size), int64(size)}, pixels)
	if err != nil {
		return nil, fmt.Errorf("pixel_values tensor: %w", err)
	}
	defer pixelTensor.Destroy()

	dim := e.config.Dimension
	featTensor, err := ort.NewEmptyTensor[float32](ort.Shape{1, int64(dim)})
	if err != nil {
		return nil, fmt.Errorf("output tensor: %w", err)
	}
	defer featTensor.Destroy()

	if err := e.imgSess.Run([]ort.Value{pixelTensor}, []ort.Value{featTensor}); err != nil {
		return nil, fmt.Errorf("vision run: %w", err)
	}

	raw := featTensor.GetData()
	out := make([]float64, dim)
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

// Close 释放 ONNX Runtime 资源。
func (e *Embedder) Close() {
	e.close.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.textSess != nil {
			e.textSess.Destroy()
		}
		if e.imgSess != nil {
			e.imgSess.Destroy()
		}
		e.loaded = false
	})
}

// New 从目录加载 CLIP 模型。目录需包含 text.onnx、vision.onnx、
// clip_config.json、tokenizer.json、merges.txt。
func New(modelDir string) (*Embedder, error) {
	if modelDir == "" {
		return nil, fmt.Errorf("clip model dir not specified")
	}

	// 读取配置
	cfgData, err := os.ReadFile(filepath.Join(modelDir, "clip_config.json"))
	if err != nil {
		return nil, fmt.Errorf("read clip_config.json: %w", err)
	}
	var cfg clipConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("parse clip_config.json: %w", err)
	}
	if cfg.Dimension <= 0 || cfg.ContextLength <= 0 || cfg.ImageSize <= 0 {
		return nil, fmt.Errorf("invalid clip config: dim=%d ctx=%d img=%d", cfg.Dimension, cfg.ContextLength, cfg.ImageSize)
	}
	if len(cfg.Mean) != 3 || len(cfg.Std) != 3 {
		return nil, fmt.Errorf("clip config mean/std must have 3 channels")
	}

	// 加载 tokenizer
	vocab, err := loadTokenizerVocab(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	merges, err := loadMerges(filepath.Join(modelDir, "merges.txt"))
	if err != nil {
		return nil, fmt.Errorf("load merges: %w", err)
	}

	// 初始化 ONNX Runtime（只初始化一次）
	if !ort.IsInitialized() {
		// 尝试从 nlp 同样的路径查找 libonnxruntime.so
		libPath := findOnnxLib()
		if libPath != "" {
			ort.SetSharedLibraryPath(libPath)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("init onnx env: %w", err)
		}
	}

	// 创建文本编码器会话
	textSess, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "text.onnx"),
		[]string{"input_ids", "attention_mask"},
		[]string{"text_embed"},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create text session: %w", err)
	}

	// 创建视觉编码器会话
	imgSess, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "vision.onnx"),
		[]string{"pixel_values"},
		[]string{"image_embed"},
		nil,
	)
	if err != nil {
		textSess.Destroy()
		return nil, fmt.Errorf("create vision session: %w", err)
	}

	// 计算模型指纹
	fp := computeFingerprint(modelDir)

	log.Printf("[clip] loaded %s dim=%d ctx=%d img=%d from %s (fp=%s)", cfg.Model, cfg.Dimension, cfg.ContextLength, cfg.ImageSize, modelDir, fp[:12])

	return &Embedder{
		config:      cfg,
		vocab:       vocab,
		merges:      merges,
		textSess:    textSess,
		imgSess:     imgSess,
		loaded:      true,
		fingerprint: fp,
	}, nil
}

// computeFingerprint 计算模型文件指纹（text.onnx + vision.onnx 的 SHA256）。
func computeFingerprint(modelDir string) string {
	h := sha256.New()
	for _, name := range []string{"text.onnx", "vision.onnx"} {
		data, err := os.ReadFile(filepath.Join(modelDir, name))
		if err != nil {
			continue
		}
		h.Write(data)
		h.Write([]byte{0}) // 分隔符
	}
	return hex.EncodeToString(h.Sum(nil))
}

// findOnnxLib 在常见路径中查找 libonnxruntime.so。
func findOnnxLib() string {
	for _, p := range []string{
		"/opt/onnxruntime/libonnxruntime.so",
		"libonnxruntime.so",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// resizeImage 使用最近邻将 src 缩放到 dstW×dstH。
// 生产中应使用双线性插值，此处为 MVP 简化。
func resizeImage(src image.Image, dstW, dstH int) image.Image {
	srcB := src.Bounds()
	srcW := srcB.Dx()
	srcH := srcB.Dy()
	if srcW == dstW && srcH == dstH {
		return src
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			sx := srcB.Min.X + x*srcW/dstW
			sy := srcB.Min.Y + y*srcH/dstH
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// denseToVector 将 []float64 稀疏化为 vector.Vector（CLIP 维度通常 512，不会太大）。
func denseToVector(d []float64) vector.Vector {
	vec := make(vector.Vector, len(d))
	for i, v := range d {
		if v != 0 {
			vec[strconv.Itoa(i)] = v
		}
	}
	return vec
}

// ---- BPE Tokenizer ----

// loadTokenizerVocab 从 HuggingFace tokenizer.json 中提取 vocab（token→id 映射）。
func loadTokenizerVocab(path string) (map[string]int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok struct {
		Model struct {
			Vocab map[string]int64 `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	if len(tok.Model.Vocab) == 0 {
		return nil, fmt.Errorf("empty vocab in %s", path)
	}
	return tok.Model.Vocab, nil
}

// loadMerges 从 merges.txt 加载 BPE 合并规则。
func loadMerges(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// 第一行是版本号（"#version: 0.2"），跳过
	var merges []string
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		merges = append(merges, line)
	}
	return merges, nil
}

// tokenizeCLIP 将文本分词为模型 vocab 中的 token id 序列。
//
// 此模型（transformers 5.x 导出的 CLIP tokenizer.json）是**词级 BPE**：
// 词末 token 带 </w> 后缀（"a</w>"=320、"red</w>"=736），词中片段不带。
// 流程：lowercase → 按空白/标点拆词 → 每词做字符级 BPE 合并 →
// 末尾片段加 </w> 查 vocab，其余片段直接查；查不到则丢弃。
func tokenizeCLIP(text string, vocab map[string]int64, merges []string, maxLen int) []int64 {
	rank := make(map[string]int, len(merges))
	for i, m := range merges {
		rank[m] = i
	}
	const endTok = "</w>"

	var tokens []int64
	if id, ok := vocab["<|startoftext|>"]; ok {
		tokens = append(tokens, id)
	}
	for _, word := range strings.Fields(strings.ToLower(text)) {
		seq := make([]string, 0, len(word))
		for _, ch := range word {
			seq = append(seq, string(ch))
		}
		merged := bpeMerge(seq, rank)
		for i, t := range merged {
			lookup := t
			if i == len(merged)-1 {
				// 词末片段带 </w>
				lookup = t + endTok
			}
			if id, ok := vocab[lookup]; ok {
				tokens = append(tokens, id)
			}
		}
	}
	if id, ok := vocab["<|endoftext|>"]; ok {
		tokens = append(tokens, id)
	}
	if len(tokens) > maxLen {
		tokens = tokens[:maxLen]
	}
	return tokens
}

// bpeMerge 对单个词的字符序列应用 BPE 合并直到无可合并对。
// rank[pair] 越小越优先（merges.txt 顺序）。
func bpeMerge(seq []string, rank map[string]int) []string {
	for len(seq) > 1 {
		// 找 rank 最低的可合并相邻对
		bestRank := -1
		bestPair := ""
		for i := 0; i < len(seq)-1; i++ {
			pair := seq[i] + " " + seq[i+1]
			if r, ok := rank[pair]; ok && (bestRank < 0 || r < bestRank) {
				bestRank = r
				bestPair = pair
			}
		}
		if bestPair == "" {
			break
		}
		parts := strings.SplitN(bestPair, " ", 2)
		merged := parts[0] + parts[1]

		// 一次性合并所有相邻的该 pair
		var out []string
		for i := 0; i < len(seq); i++ {
			if i < len(seq)-1 && seq[i] == parts[0] && seq[i+1] == parts[1] {
				out = append(out, merged)
				i++
			} else {
				out = append(out, seq[i])
			}
		}
		seq = out
	}
	return seq
}

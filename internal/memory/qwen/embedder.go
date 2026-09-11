//go:build onnxruntime

// Package qwen 提供 Qwen3-VL-Embedding 的完整图文共享 ONNX 编码器。
// 文本和图像共用 token embedding、28 层 Transformer、last-token 池化与
// fingerprint；Vision.onnx 只产生注入 Transformer 的中间特征。
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

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

type embedConfig struct {
	Arch          string    `json:"arch"`
	Dimension     int       `json:"dim"`
	MaxLength     int       `json:"max_length"`
	Instruction   string    `json:"instruction"`
	Pooling       string    `json:"pooling"`
	ImageSize     int       `json:"image_size"`
	PatchSize     int       `json:"patch_size"`
	TemporalPatch int       `json:"temporal_patch_size"`
	SpatialMerge  int       `json:"spatial_merge_size"`
	ImageMean     []float64 `json:"image_mean"`
	ImageStd      []float64 `json:"image_std"`
	RopeTheta     float64   `json:"rope_theta"`
	MRopeSection  []int     `json:"mrope_section"`
}

type Embedder struct {
	mu sync.RWMutex

	loaded     bool
	config     embedConfig
	tok        *Tokenizer
	token      *ort.DynamicAdvancedSession
	transform  *ort.DynamicAdvancedSession
	vision     *ort.DynamicAdvancedSession
	fp         string
	close      sync.Once
}

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
	if cfg.Dimension != 2048 || cfg.MaxLength < 598 || cfg.Pooling != "last_token" {
		return nil, fmt.Errorf("qwen: incompatible config dim=%d max_length=%d pooling=%q", cfg.Dimension, cfg.MaxLength, cfg.Pooling)
	}
	if cfg.ImageSize != qwenImageSize || cfg.PatchSize != qwenPatchSize || cfg.TemporalPatch != qwenTemporalPatch || cfg.SpatialMerge != qwenSpatialMerge {
		return nil, fmt.Errorf("qwen: incompatible vision layout image=%d patch=%d temporal=%d merge=%d", cfg.ImageSize, cfg.PatchSize, cfg.TemporalPatch, cfg.SpatialMerge)
	}
	if cfg.RopeTheta <= 0 || len(cfg.MRopeSection) != 3 || cfg.MRopeSection[0]+cfg.MRopeSection[1]+cfg.MRopeSection[2] != qwenRotaryHalfDim {
		return nil, fmt.Errorf("qwen: incompatible rope theta=%g section=%v", cfg.RopeTheta, cfg.MRopeSection)
	}

	tok, err := LoadTokenizer(modelDir)
	if err != nil {
		return nil, err
	}
	if !ort.IsInitialized() {
		if lib := findOnnxLib(); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("init onnx env: %w", err)
		}
	}

	token, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "TokenEmbedding.onnx"),
		[]string{"input_ids"}, []string{"hidden"}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create qwen token embedding session: %w", err)
	}
	transform, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "Transformer.onnx"),
		[]string{"hidden", "deepstack_0", "deepstack_1", "deepstack_2", "rotary_cos", "rotary_sin", "causal_mask"},
		[]string{"embedding"}, nil,
	)
	if err != nil {
		token.Destroy()
		return nil, fmt.Errorf("create qwen transformer session: %w", err)
	}
	vision, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "Vision.onnx"), []string{"pixel_values"},
		[]string{"deepstack_feature_0", "deepstack_feature_1", "deepstack_feature_2", "vision_hidden_states"}, nil,
	)
	if err != nil {
		token.Destroy()
		transform.Destroy()
		return nil, fmt.Errorf("create qwen vision session: %w", err)
	}

	return &Embedder{
		loaded: true, config: cfg, tok: tok,
		token: token, transform: transform, vision: vision,
		fp: computeFingerprint(modelDir),
	}, nil
}

func (e *Embedder) VectorizeDense(text string) ([]float64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.loaded {
		return nil, fmt.Errorf("qwen embedder not loaded")
	}
	ids, _, position, _, err := e.tok.textModelInput(e.config.Instruction, text, e.config.MaxLength)
	if err != nil {
		return nil, err
	}
	hidden, err := e.runTokenEmbedding(ids)
	if err != nil {
		return nil, err
	}
	deep := make([][]float32, 3)
	for i := range deep {
		deep[i] = make([]float32, len(hidden))
	}
	return e.runTransformer(hidden, deep, position, len(ids))
}

// EmbedImageDense 把一张图片编码到统一空间。
//
// mime 决定这个模态是否在本空间的原生覆盖范围内：Qwen3-VL 能原生编码文本与
// 图像，但**不原生支持音频**。音频（以及未抽帧的视频文件）必须返回
// ErrModalityUnsupported，而不是拿视觉塔去编码——那会往统一空间里灌入
// 语义错误的坐标，而错误是静默的。视频请由上层抽帧后逐帧当作图像编码。
func (e *Embedder) EmbedImageDense(raw []byte, mime string) ([]float64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.loaded {
		return nil, fmt.Errorf("qwen embedder not loaded")
	}
	switch {
	case strings.HasPrefix(mime, "audio/"):
		return nil, fmt.Errorf("%w: audio (%s) 需由真正的统一音频模型扩展", vector.ErrModalityUnsupported, mime)
	case strings.HasPrefix(mime, "video/"):
		return nil, fmt.Errorf("%w: 视频文件请先抽帧，逐帧按图像编码 (%s)", vector.ErrModalityUnsupported, mime)
	}
	pixels, err := preprocessImage(raw)
	if err != nil {
		return nil, err
	}
	features, err := e.runVision(pixels)
	if err != nil {
		return nil, err
	}
	ids, _, position, visual, err := e.tok.imageModelInput(e.config.Instruction, e.config.MaxLength)
	if err != nil {
		return nil, err
	}
	hidden, err := e.runTokenEmbedding(ids)
	if err != nil {
		return nil, err
	}
	deep := make([][]float32, 3)
	for i := range deep {
		deep[i] = make([]float32, len(hidden))
	}
	visualIndex := 0
	for tokenIndex, isVisual := range visual {
		if !isVisual {
			continue
		}
		dst := tokenIndex * e.config.Dimension
		src := visualIndex * e.config.Dimension
		copy(hidden[dst:dst+e.config.Dimension], features[3][src:src+e.config.Dimension])
		for layer := range deep {
			copy(deep[layer][dst:dst+e.config.Dimension], features[layer][src:src+e.config.Dimension])
		}
		visualIndex++
	}
	if visualIndex != qwenVisualTokens {
		return nil, fmt.Errorf("qwen: injected visual tokens=%d, want %d", visualIndex, qwenVisualTokens)
	}
	return e.runTransformer(hidden, deep, position, len(ids))
}

func (e *Embedder) runTokenEmbedding(ids []int) ([]float32, error) {
	inputIDs := make([]int64, len(ids))
	for i, id := range ids {
		inputIDs[i] = int64(id)
	}
	in, err := ort.NewTensor(ort.Shape{1, int64(len(ids))}, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("qwen token input: %w", err)
	}
	defer in.Destroy()
	outs := make([]ort.Value, 1)
	if err := e.token.Run([]ort.Value{in}, outs); err != nil {
		return nil, fmt.Errorf("qwen token embedding run: %w", err)
	}
	if outs[0] == nil {
		return nil, fmt.Errorf("qwen token embedding output is nil")
	}
	defer outs[0].Destroy()
	tensor, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("qwen token embedding output type %T", outs[0])
	}
	shape := tensor.GetShape()
	if len(shape) != 3 || shape[0] != 1 || shape[1] != int64(len(ids)) || shape[2] != int64(e.config.Dimension) {
		return nil, fmt.Errorf("qwen token embedding shape=%v", shape)
	}
	return append([]float32(nil), tensor.GetData()...), nil
}

func (e *Embedder) runVision(pixels []float32) ([][]float32, error) {
	in, err := ort.NewTensor(ort.Shape{qwenImagePatches, qwenPatchVectorSize}, pixels)
	if err != nil {
		return nil, fmt.Errorf("qwen vision input: %w", err)
	}
	defer in.Destroy()
	outs := make([]ort.Value, 4)
	if err := e.vision.Run([]ort.Value{in}, outs); err != nil {
		return nil, fmt.Errorf("qwen vision run: %w", err)
	}
	features := make([][]float32, 4)
	for i, value := range outs {
		if value == nil {
			return nil, fmt.Errorf("qwen vision output %d is nil", i)
		}
		defer value.Destroy()
		tensor, ok := value.(*ort.Tensor[float32])
		if !ok {
			return nil, fmt.Errorf("qwen vision output %d type %T", i, value)
		}
		shape := tensor.GetShape()
		if len(shape) != 2 || shape[0] != qwenVisualTokens || shape[1] != int64(e.config.Dimension) {
			return nil, fmt.Errorf("qwen vision output %d shape=%v", i, shape)
		}
		features[i] = append([]float32(nil), tensor.GetData()...)
	}
	return features, nil
}

func (e *Embedder) runTransformer(hidden []float32, deep [][]float32, position []int64, seq int) ([]float64, error) {
	if len(hidden) != seq*e.config.Dimension || len(deep) != 3 || len(position) != 3*seq {
		return nil, fmt.Errorf("qwen: invalid transformer inputs hidden=%d deep=%d position=%d seq=%d", len(hidden), len(deep), len(position), seq)
	}
	cos, sin := e.rotary(position, seq)
	causal := causalMask(seq)

	hiddenTensor, err := ort.NewTensor(ort.Shape{1, int64(seq), int64(e.config.Dimension)}, hidden)
	if err != nil {
		return nil, fmt.Errorf("qwen hidden tensor: %w", err)
	}
	defer hiddenTensor.Destroy()
	inputs := []ort.Value{hiddenTensor}
	var deepTensors []*ort.Tensor[float32]
	for i, data := range deep {
		if len(data) != len(hidden) {
			return nil, fmt.Errorf("qwen deepstack %d length=%d, want %d", i, len(data), len(hidden))
		}
		t, err := ort.NewTensor(ort.Shape{1, int64(seq), int64(e.config.Dimension)}, data)
		if err != nil {
			return nil, fmt.Errorf("qwen deepstack %d tensor: %w", i, err)
		}
		deepTensors = append(deepTensors, t)
		inputs = append(inputs, t)
	}
	defer func() {
		for _, t := range deepTensors {
			t.Destroy()
		}
	}()
	cosTensor, err := ort.NewTensor(ort.Shape{1, int64(seq), qwenRotaryDim}, cos)
	if err != nil {
		return nil, fmt.Errorf("qwen rotary cos: %w", err)
	}
	defer cosTensor.Destroy()
	sinTensor, err := ort.NewTensor(ort.Shape{1, int64(seq), qwenRotaryDim}, sin)
	if err != nil {
		return nil, fmt.Errorf("qwen rotary sin: %w", err)
	}
	defer sinTensor.Destroy()
	causalTensor, err := ort.NewTensor(ort.Shape{1, 1, int64(seq), int64(seq)}, causal)
	if err != nil {
		return nil, fmt.Errorf("qwen causal mask: %w", err)
	}
	defer causalTensor.Destroy()
	inputs = append(inputs, cosTensor, sinTensor, causalTensor)

	out, err := ort.NewEmptyTensor[float32](ort.Shape{1, int64(e.config.Dimension)})
	if err != nil {
		return nil, fmt.Errorf("qwen output tensor: %w", err)
	}
	defer out.Destroy()
	if err := e.transform.Run(inputs, []ort.Value{out}); err != nil {
		return nil, fmt.Errorf("qwen transformer run: %w", err)
	}
	return normalize(out.GetData()), nil
}

const (
	qwenRotaryHalfDim = 64
	qwenRotaryDim     = 128
)

func (e *Embedder) rotary(position []int64, seq int) ([]float32, []float32) {
	cos := make([]float32, seq*qwenRotaryDim)
	sin := make([]float32, seq*qwenRotaryDim)
	inv := make([]float64, qwenRotaryHalfDim)
	for i := range inv {
		inv[i] = 1 / math.Pow(e.config.RopeTheta, float64(2*i)/qwenRotaryDim)
	}
	for token := 0; token < seq; token++ {
		freq := make([]float64, qwenRotaryHalfDim)
		for i := range freq {
			freq[i] = float64(position[token]) * inv[i]
		}
		for dim, offset := range []int{0, 1, 2} {
			if dim == 0 {
				continue
			}
			limit := e.config.MRopeSection[dim] * 3
			for i := offset; i < limit; i += 3 {
				freq[i] = float64(position[dim*seq+token]) * inv[i]
			}
		}
		for i, f := range freq {
			c, s := float32(math.Cos(f)), float32(math.Sin(f))
			cos[token*qwenRotaryDim+i] = c
			cos[token*qwenRotaryDim+qwenRotaryHalfDim+i] = c
			sin[token*qwenRotaryDim+i] = s
			sin[token*qwenRotaryDim+qwenRotaryHalfDim+i] = s
		}
	}
	return cos, sin
}

func causalMask(seq int) []float32 {
	out := make([]float32, seq*seq)
	for row := 0; row < seq; row++ {
		for col := row + 1; col < seq; col++ {
			out[row*seq+col] = -math.MaxFloat32
		}
	}
	return out
}

func normalize(raw []float32) []float64 {
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
	return out
}

func (e *Embedder) Fingerprint() string { return e.fp }
func (e *Embedder) Dim() int            { return e.config.Dimension }
func (e *Embedder) Loaded() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loaded
}
func (e *Embedder) Close() {
	e.close.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.token != nil {
			e.token.Destroy()
			e.token = nil
		}
		if e.transform != nil {
			e.transform.Destroy()
			e.transform = nil
		}
		if e.vision != nil {
			e.vision.Destroy()
			e.vision = nil
		}
		e.loaded = false
	})
}

func computeFingerprint(modelDir string) string {
	h := sha256.New()
	for _, name := range []string{"TokenEmbedding.onnx", "Transformer.onnx", "Vision.onnx", "embed_config.json"} {
		if data, err := os.ReadFile(filepath.Join(modelDir, name)); err == nil {
			h.Write([]byte(name))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
	}
	entries, _ := os.ReadDir(modelDir)
	var names []string
	for _, entry := range entries {
		n := entry.Name()
		if strings.HasPrefix(n, "embed_tokens.") || strings.HasPrefix(n, "layers.") || strings.HasPrefix(n, "onnx__") || strings.HasSuffix(n, ".onnx.data") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if info, err := os.Stat(filepath.Join(modelDir, n)); err == nil {
			fmt.Fprintf(h, "%s:%d\n", n, info.Size())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func findOnnxLib() string {
	for _, p := range []string{"/opt/onnxruntime/libonnxruntime.so", "/usr/local/lib/libonnxruntime.so", "/usr/lib/libonnxruntime.so"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

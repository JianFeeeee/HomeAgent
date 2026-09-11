//go:build onnxruntime

package qwen

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// onnxModelDir 返回三段式 Qwen 多模态 ONNX 产物目录。
//
// 产物约 8GB（含外部权重），不进仓库；由 scripts/export_qwen3vl_embedding_onnx.py
// 自动拉取模型并导出。可通过 QWEN_ONNX_MODEL_DIR 指向别处；目录不存在时相关
// 测试跳过，而不是失败——CI 与本机开发者都不一定有这份产物。
func onnxModelDir() string {
	if v := os.Getenv("QWEN_ONNX_MODEL_DIR"); v != "" {
		return v
	}
	return "/home/newqqagent/models/qwen3-vl-embed-multimodal-onnx"
}

func requireONNXArtifacts(t *testing.T) string {
	t.Helper()
	dir := onnxModelDir()
	for _, name := range []string{"TokenEmbedding.onnx", "Transformer.onnx", "Vision.onnx", "embed_config.json", "tokenizer.json"} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Skipf("ONNX 产物不完整（%s: %v），跳过；用 scripts/export_qwen3vl_embedding_onnx.py 导出", name, err)
		}
	}
	return dir
}

// onnxReference 是导出脚本 `--emit-reference` 写出的冻结参考。
//
// 刻意不把浮点常量硬编码在测试里：参考值必须能追溯到「哪个模型、哪次导出、
// 什么输入」，而不是一组无人知道出处的数字。参考文件里的 RGB/尺寸同时用来
// 构造测试图片，保证输入与参考按构造一致，不会因测试改动而静默错位。
type onnxReference struct {
	Text              string    `json:"text"`
	TextVectorPrefix  []float64 `json:"text_vector_prefix"`
	ImageRGB          []int     `json:"image_rgb"`
	ImageSize         int       `json:"image_size"`
	ImageVectorPrefix []float64 `json:"image_vector_prefix"`
	Dim               int       `json:"dim"`
}

func loadReference(t *testing.T, dir string) *onnxReference {
	t.Helper()
	path := os.Getenv("QWEN_ONNX_REFERENCE")
	if path == "" {
		path = filepath.Join(dir, "qwen_reference.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("缺少冻结参考 %s（由 scripts/export_qwen3vl_embedding_onnx.py --emit-reference 生成）: %v", path, err)
	}
	var ref onnxReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("解析参考 %s: %v", path, err)
	}
	if ref.Text == "" || len(ref.TextVectorPrefix) == 0 || len(ref.ImageRGB) != 3 || ref.ImageSize <= 0 {
		t.Fatalf("参考 %s 不完整: %+v", path, ref)
	}
	return &ref
}

// solidPNG 生成一张 size×size 纯色 PNG，供跨语言冻结向量回归。
//
// 刻意用纯色且尺寸与视觉塔一致：Go 侧预处理对已是 768×768 的输入不做插值、
// 不补边，于是 patch 张量只由布局决定。一旦 patch 排列写错（内层循环顺序、
// merge 分组顺序、通道顺序），冻结向量立刻不匹配——而那类错误在人工看图时
// 几乎发现不了。
func solidPNG(t *testing.T, size int, r, g, b uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func newTestEmbedder(t *testing.T, dir string) *Embedder {
	t.Helper()
	e, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	if !e.Loaded() || e.Dim() != 2048 || e.Fingerprint() == "" {
		t.Fatalf("元数据异常: loaded=%v dim=%d fingerprint=%q", e.Loaded(), e.Dim(), e.Fingerprint())
	}
	return e
}

func assertNormalized(t *testing.T, name string, got []float64, dim int) {
	t.Helper()
	if dim > 0 && len(got) != dim {
		t.Fatalf("%s 维度 = %d，期望 %d", name, len(got), dim)
	}
	var norm float64
	for _, v := range got {
		norm += v * v
	}
	if diff := math.Abs(math.Sqrt(norm) - 1); diff > 1e-6 {
		t.Errorf("%s L2 norm = %.9f，期望 1", name, math.Sqrt(norm))
	}
}

func assertFrozenPrefix(t *testing.T, name string, got, want []float64) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("%s 向量过短: %d", name, len(got))
	}
	for i := range want {
		if diff := math.Abs(got[i] - want[i]); diff > 2e-5 {
			t.Errorf("%s 维度 %d = %.10g，参考 %.10g，差 %.3g", name, i, got[i], want[i], diff)
		}
	}
}

// TestEmbedderMatchesONNXReference 逐维对比导出脚本写出的冻结参考向量，
// 验证完整 Go 路径：模板渲染 → BPE → TokenEmbedding → Transformer →
// last-token 池化 → L2 normalize。
//
// 只覆盖前若干维不是因为放宽正确性（脚本侧的 PyTorch↔ONNX 校验是逐维的），
// 而是避免把 2048 个浮点常量塞进仓库；这里负责捕获 Go 张量形状、输入名、
// 输出名、池化/归一化或模板接线错误。
func TestEmbedderMatchesONNXReference(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)
	ref := loadReference(t, dir)

	ids, _, _, _, err := e.tok.textModelInput(e.config.Instruction, ref.Text, e.config.MaxLength)
	if err != nil {
		t.Fatalf("textModelInput: %v", err)
	}
	postID, ok := e.tok.SpecialID("<|endoftext|>")
	if !ok || ids[len(ids)-1] != postID {
		t.Fatalf("模型输入 post-processor 异常: len=%d tail=%v postID=%d ok=%v", len(ids), ids[len(ids)-1:], postID, ok)
	}

	got, err := e.VectorizeDense(ref.Text)
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	assertNormalized(t, "text", got, ref.Dim)
	assertFrozenPrefix(t, "text", got, ref.TextVectorPrefix)
}

// TestEmbedderImageMatchesONNXReference 冻结一张纯色图的参考向量，
// 验证 Go 侧的视觉预处理 + patch 排列 + 视觉注入 + 语言模型与 Python 参考一致。
func TestEmbedderImageMatchesONNXReference(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)
	ref := loadReference(t, dir)

	img := solidPNG(t, ref.ImageSize, uint8(ref.ImageRGB[0]), uint8(ref.ImageRGB[1]), uint8(ref.ImageRGB[2]))
	got, err := e.EmbedImageDense(img, "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	assertNormalized(t, "image", got, ref.Dim)
	assertFrozenPrefix(t, "image", got, ref.ImageVectorPrefix)
}

// TestEmbedderTextIsSensitiveToInput 阴性对照：冻结向量必须真的随输入变化。
//
// 没有这条对照，一个「永远返回同一向量」的错误实现也能通过上面的冻结回归
//（只要那个常量恰好等于参考值）。这里验证不同文本给出不同向量，且相似文本
// 的余弦高于无关文本——即嵌入确实携带语义，而不是常量。
func TestEmbedderTextIsSensitiveToInput(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	base, err := e.VectorizeDense("今天天气怎么样")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	same, err := e.VectorizeDense("今天天气怎么样")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if cosine(base, same) < 0.999999 {
		t.Errorf("同一输入两次嵌入不一致: cos=%.9f（ONNX 会话被并发复用或存在非确定性）", cosine(base, same))
	}

	other, err := e.VectorizeDense("数据库索引的选择性是怎么计算的")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if cosine(base, other) > 0.999 {
		t.Errorf("无关文本的余弦高达 %.6f，嵌入可能是常量", cosine(base, other))
	}
}

// TestEmbedderImageMatchesONNXReference 的替代：不依赖冻结参考的不变量检查。
//
// 即使参考文件缺失（没有导出产物）或未重新生成，这些不变量也应成立：
// 图像路径必须真的走了视觉塔，且不同图片给出不同坐标。
func TestEmbedderImageDiffersFromText(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	imgVec, err := e.EmbedImageDense(solidPNG(t, qwenImageSize, 200, 30, 30), "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	txtVec, err := e.VectorizeDense(DefaultInstruction)
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if cosine(imgVec, txtVec) > 0.999 {
		t.Error("图像向量与文本向量几乎相同，视觉塔可能没被真正执行")
	}

	// 不同颜色的图必须给出不同向量
	blue, err := e.EmbedImageDense(solidPNG(t, qwenImageSize, 30, 150, 220), "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	if cosine(imgVec, blue) > 0.999999 {
		t.Error("不同图片给出相同向量，视觉路径未生效")
	}
}

// TestEmbedderRejectsUnsupportedModalities 音频与视频文件必须显式报「不在本空间」，
// 而不是拿视觉塔硬编码一个语义错误的坐标。
//
// Qwen3-VL 原生支持文本与图像；音频需要未来接入真正的统一音频模型。
// 若这里退化成普通错误，调用方会把它当「本次失败、下次重试」，
// 于是每轮启动都重试一批永远不可能成功的条目。
func TestEmbedderRejectsUnsupportedModalities(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	for _, mime := range []string{"audio/wav", "audio/mpeg", "video/mp4", "video/quicktime"} {
		_, err := e.EmbedImageDense([]byte("not-a-real-media"), mime)
		if err == nil {
			t.Fatalf("%s 应返回错误而不是造出向量", mime)
		}
		if !errors.Is(err, vector.ErrModalityUnsupported) {
			t.Errorf("%s 错误应为 ErrModalityUnsupported，实际: %v", mime, err)
		}
	}
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

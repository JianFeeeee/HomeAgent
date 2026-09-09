//go:build onnxruntime

package clip

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

func TestSmokeLoadAndEncode(t *testing.T) {
	modelDir := os.Getenv("CLIP_MODEL_DIR")
	if modelDir == "" {
		modelDir = "/home/newqqagent/models/clip-vit-b32"
	}
	if _, err := os.Stat(modelDir + "/text.onnx"); err != nil {
		t.Skipf("模型目录不存在: %v", err)
	}

	emb, err := New(modelDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer emb.Close()

	if !emb.Loaded() {
		t.Fatal("loaded should be true")
	}
	if emb.Dim() != 512 {
		t.Fatalf("dim = %d, want 512", emb.Dim())
	}
	if emb.Fingerprint() == "" {
		t.Fatal("fingerprint should not be empty")
	}

	// 文本编码
	textVec, err := emb.VectorizeDense("a photo of a cat")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if len(textVec) != 512 {
		t.Fatalf("text vec len = %d, want 512", len(textVec))
	}
	fmt.Printf("text vec[:5] = %v\n", textVec[:5])

	// 同义文本应比远义文本更相似
	textVec2, _ := emb.VectorizeDense("a photograph of a dog")
	textVec3, _ := emb.VectorizeDense("quantum physics equations")

	sim12 := cosineSim(textVec, textVec2)
	sim13 := cosineSim(textVec, textVec3)
	fmt.Printf("cat vs dog = %.4f, cat vs physics = %.4f\n", sim12, sim13)
	if sim12 <= sim13 {
		t.Errorf("cat-dog sim (%.4f) should be > cat-physics sim (%.4f)", sim12, sim13)
	}

	// 通过 Vectorizer 接口（稀疏 map）
	sparseVec := emb.Vectorize("hello world")
	if len(sparseVec) == 0 {
		t.Error("sparse Vectorize should return non-empty")
	}
	fmt.Printf("sparse len = %d\n", len(sparseVec))
}

// TestCrossModalAlignment 验证图文在同一向量空间可比：
// 红底图的向量应与 "a red image" 更相似，而非 "a blue image"。
func TestCrossModalAlignment(t *testing.T) {
	modelDir := os.Getenv("CLIP_MODEL_DIR")
	if modelDir == "" {
		modelDir = "/home/newqqagent/models/clip-vit-b32"
	}
	emb, err := New(modelDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer emb.Close()

	// 生成 224x224 纯红底 PNG
	img := image.NewRGBA(image.Rect(0, 0, 224, 224))
	red := color.RGBA{220, 40, 40, 255}
	for y := 0; y < 224; y++ {
		for x := 0; x < 224; x++ {
			img.Set(x, y, red)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	imgVec, err := emb.EmbedImageDense(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	if len(imgVec) != 512 {
		t.Fatalf("img vec len = %d, want 512", len(imgVec))
	}

	redText, _ := emb.VectorizeDense("a red image")
	blueText, _ := emb.VectorizeDense("a blue image")
	redSim := cosineSim(imgVec, redText)
	blueSim := cosineSim(imgVec, blueText)
	fmt.Printf("red-image vs red-text = %.4f, vs blue-text = %.4f\n", redSim, blueSim)
	if redSim <= blueSim {
		t.Errorf("red image should align better with red text (%.4f) than blue (%.4f)", redSim, blueSim)
	}

	// 同图应比异图更相似：存两张不同颜色，query 用红底图应召回红图
	d1 := imgVec
	blueImg := image.NewRGBA(image.Rect(0, 0, 224, 224))
	blue := color.RGBA{40, 40, 220, 255}
	for y := 0; y < 224; y++ {
		for x := 0; x < 224; x++ {
			blueImg.Set(x, y, blue)
		}
	}
	var buf2 bytes.Buffer
	png.Encode(&buf2, blueImg)
	d2, _ := emb.EmbedImageDense(buf2.Bytes(), "image/png")
	if cosineSim(d1, d2) >= 0.99 {
		t.Errorf("red and blue images should differ (got sim %.4f)", cosineSim(d1, d2))
	}
}

func cosineSim(a, b []float64) float64 {
	if len(a) != len(b) {
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
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 50; i++ {
		z = (z + x/z) / 2
	}
	return z
}

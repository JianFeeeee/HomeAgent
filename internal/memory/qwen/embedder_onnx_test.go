//go:build onnxruntime

package qwen

import (
	"math"
	"os"
	"testing"
)

// 本地模型路径与 tokenizer_test.go 共用；产物不在仓库时跳过。
const onnxModelDir = "/home/newqqagent/models/qwen3-vl-embed-text-onnx"

// TestEmbedderMatchesONNXReference 冻结一条由 Python onnxruntime 1.28.0 生成的
// FP32 参考向量，验证完整 Go 路径：模板渲染 → BPE → ONNX → L2 normalize。
//
// 只校验前 12 维不是为了放宽正确性，而是避免把 2048 个浮点常量塞进仓库；
// tokenizer 的全部 token 已由 TestTokenizerMatchesReference 逐条精确校验，图本身
// 另有 PyTorch↔ONNX 的多形状验证。这里负责捕获 Go 张量形状、输入名、输出名、
// 池化/归一化或模板接线错误。
func TestEmbedderMatchesONNXReference(t *testing.T) {
	if _, err := os.Stat(onnxModelDir + "/TextTower.onnx"); err != nil {
		t.Skipf("ONNX 产物不可用，跳过: %v", err)
	}

	e, err := New(onnxModelDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	ids, err := e.tok.encodeModelInput(e.renderInput("今天天气怎么样"), e.config.MaxLength)
	if err != nil {
		t.Fatalf("encodeModelInput: %v", err)
	}
	postID, ok := e.tok.SpecialID("<|endoftext|>")
	if !ok || len(ids) != 23 || ids[len(ids)-1] != postID {
		t.Fatalf("模型输入 post-processor 异常: len=%d tail=%v postID=%d ok=%v", len(ids), ids[len(ids)-1:], postID, ok)
	}

	got, err := e.VectorizeDense("今天天气怎么样")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if len(got) != 2048 {
		t.Fatalf("向量维度 = %d，期望 2048", len(got))
	}

	want := []float64{
		-0.0288955811, 0.0522339381, 0.0360119902, -0.000676361844,
		-0.0431003496, 0.00342374574, -0.000226021366, 0.0157372113,
		-0.028889874, 0.0264931992, 0.0117276432, -0.00513622677,
	}
	for i := range want {
		if diff := math.Abs(got[i] - want[i]); diff > 2e-5 {
			t.Errorf("维度 %d = %.10g，参考 %.10g，差 %.3g", i, got[i], want[i], diff)
		}
	}

	var norm float64
	for _, v := range got {
		norm += v * v
	}
	if diff := math.Abs(math.Sqrt(norm) - 1); diff > 1e-6 {
		t.Errorf("L2 norm = %.9f，期望 1", math.Sqrt(norm))
	}
	if !e.Loaded() || e.Dim() != 2048 || e.Fingerprint() == "" {
		t.Errorf("元数据异常: loaded=%v dim=%d fingerprint=%q", e.Loaded(), e.Dim(), e.Fingerprint())
	}
}

package memory

import (
	"testing"
	"unsafe"
)

// 词向量必须用 float32 存。
//
// 这条判据是拿生产内存换来的：向量本体 = 词数 × 维数 × 每元素字节数。
// 生产配置加载了 200000(zh) + 378151(en) = 57.8 万词 × 300 维 ⇒
// float64 = 1.29GB、float32 = 0.65GB（差 0.65GB 常驻）。
// 源数据（fastText 文本格式）本身就是 float32 精度，用 float64 存没有任何收益。
//
// 若有人把类型改回 float64，本测试**编译失败**（`var vec []float32` 的类型断言），
// 这正是想要的效果。
func TestStaticEmbedder_VectorMemIsFloat32(t *testing.T) {
	e := newSynthEmbedder(t, 300)

	words := 0
	bytes := 0
	for _, vec := range e.words {
		var typed []float32 = vec // 编译期断言：存储必须是 []float32
		if len(typed) != e.dim {
			t.Fatalf("维度不符: %d != %d", len(typed), e.dim)
		}
		words++
		bytes += len(typed) * int(unsafe.Sizeof(typed[0]))
	}
	if words == 0 {
		t.Fatal("合成模型应至少加载一个词")
	}
	// float32：每词 300×4 = 1200 字节；float64 会是 2400
	if want := words * e.dim * 4; bytes != want {
		t.Fatalf("向量本体字节数应 %d（float32），实际 %d", want, bytes)
	}
}

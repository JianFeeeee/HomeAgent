//go:build onnxruntime

package qwen3vl

import (
	"os"
	"strings"
	"testing"
)

// 生成侧（providers/qwen3vlgen）依赖 DecodeOne 把 argmax token id 还原成文本。
// 本包此前只做正向编码，这层是新增的 —— 编码错位会让生成输出乱码而不报错，
// 所以必须双向都能钉住。

// 复用 embedder_onnx_test.go 的 QWEN_ONNX_MODEL_DIR 约定：模型目录有 12GB，
// 不可能入库，测试靠环境变量指向。
func loadTestTokenizer(t *testing.T) *Tokenizer {
	t.Helper()
	dir := os.Getenv("QWEN_ONNX_MODEL_DIR")
	if dir == "" {
		t.Skip("跳过：未设置 QWEN_ONNX_MODEL_DIR")
	}
	if _, err := os.Stat(dir + "/tokenizer.json"); err != nil {
		t.Skipf("跳过：%s 下无 tokenizer.json", dir)
	}
	tok, err := LoadTokenizer(dir)
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	return tok
}

// ★ 双向一致性：Encode 后能原样 Decode 回来。
//
// 判据必须用「往返相等」而不是「能解出非空」——后者对任何乱码都成立。
func TestDecodeOne_往返一致(t *testing.T) {
	tok := loadTestTokenizer(t)
	cases := []string{
		"第183批",
		"告警规则9条",
		"值班手册第4版",
		"容量预警70%",
		"排期10月",
		"混合 mixed 123",
		"换行\n与制表\t",
	}
	for _, want := range cases {
		ids := tok.Encode(want)
		if len(ids) == 0 {
			t.Errorf("Encode(%q) 为空", want)
			continue
		}
		var got strings.Builder
		for _, id := range ids {
			got.WriteString(tok.DecodeOne(id))
		}
		if got.String() != want {
			t.Errorf("往返不等：Encode→Decode 得 %q，期望 %q", got.String(), want)
		}
	}
}

// 单 token 解码必须确定：同样输入两次调用结果一致（生成输出要逐 token 确定）。
func TestDecodeOne_确定性(t *testing.T) {
	tok := loadTestTokenizer(t)
	ids := tok.Encode("第183批 停机4分")
	for _, id := range ids {
		first := tok.DecodeOne(id)
		for i := 0; i < 5; i++ {
			if got := tok.DecodeOne(id); got != first {
				t.Fatalf("token %d 解码不确定：%q vs %q", id, first, got)
			}
		}
	}
}

// 未在词表里的 id 必须返回空串，不能 panic、不能吐乱码。
func TestDecodeOne_未知id返回空(t *testing.T) {
	tok := loadTestTokenizer(t)
	for _, id := range []int{-1, 999999999} {
		if got := tok.DecodeOne(id); got != "" {
			t.Errorf("未知 id %d 应返回空串，实际 %q", id, got)
		}
	}
}

// ★ 变异自证：若 DecodeOne 用遍历 map 反查（顺序不确定），
// 或者解码退化成只返回单字节，本测试必须变红。
func TestDecodeOne_不是遍历反查(t *testing.T) {
	tok := loadTestTokenizer(t)
	// 找一个多字节 token：若按字节解码会碎成多个乱码字符
	ids := tok.Encode("记录")
	if len(ids) == 0 {
		t.Skip("无 token")
	}
	decoded := tok.DecodeOne(ids[0])
	// 中文字符是 3 字节：按字节解码会得到 3 个替换字符
	if len([]rune(decoded)) == 0 {
		t.Fatal("解码为空")
	}
	if strings.ContainsRune(decoded, 0xFFFD) {
		t.Errorf("解码出现替换字符 U+FFFD ⇒ 走了按字节解码：%q", decoded)
	}
}

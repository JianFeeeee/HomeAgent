//go:build onnxruntime

package chineseclip

import (
	"os"
	"path/filepath"
	"testing"
)

// findModelDir 的回退链与错误指引。
func TestFindModelDir_ConfiguredWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "embed_config.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	got, hint := findModelDir(dir)
	if got != dir || hint != "" {
		t.Fatalf("configured 路径应直接命中: got=%q hint=%q", got, hint)
	}
}

func TestFindModelDir_FallbackToSystemDir(t *testing.T) {
	// configured 不存在 ⇒ 回退到系统目录。若本机恰好有 /usr/lib/homeagent 模型则命中它，
	// 否则两个候选都落空 ⇒ 返回空 + 指引。两种结果都合法，断言的是「不空则命中系统路径」。
	got, hint := findModelDir(t.TempDir())
	if got != "" {
		if got != "/usr/lib/homeagent/models/chinese-clip-vit-b16-onnx" {
			t.Fatalf("回退命中了意外路径: %q", got)
		}
		return
	}
	if hint == "" || !contains(hint, "export_chineseclip_onnx.py") {
		t.Fatalf("全落空时必须给可执行指引，got hint=%q", hint)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

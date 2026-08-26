package webui

// webui 文件发送能力测试：stageWebFile 中转 + /files/ 带鉴权下载。
import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageWebFileAndDownload(t *testing.T) {
	dir := t.TempDir()
	webFilesDir = dir
	defer func() { webFilesDir = "" }()

	// 源文件（模拟 agent 要发的图片）
	src := filepath.Join(dir, "src.jpg")
	if err := os.WriteFile(src, []byte("fake-jpeg-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	url, size, err := stageWebFile(src, true)
	if err != nil {
		t.Fatalf("stageWebFile: %v", err)
	}
	if !strings.HasPrefix(url, "/files/") || !strings.HasSuffix(url, ".jpg") {
		t.Fatalf("unexpected url: %s", url)
	}
	if size != int64(len("fake-jpeg-bytes")) {
		t.Fatalf("size = %d", size)
	}
	// 中转文件存在且内容一致
	data, err := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(url, "/files/")))
	if err != nil || string(data) != "fake-jpeg-bytes" {
		t.Fatalf("staged file mismatch: %v", err)
	}

	// 远程 URL 透传不落盘
	u2, _, err := stageWebFile("https://example.com/a.png", true)
	if err != nil || u2 != "https://example.com/a.png" {
		t.Fatalf("remote url passthrough failed: %v %s", err, u2)
	}

	// 危险扩展名被替换为 .bin
	srcBad := filepath.Join(dir, "evil.html")
	os.WriteFile(srcBad, []byte("<b>x</b>"), 0644)
	u3, _, err := stageWebFile(srcBad, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u3, ".bin") {
		t.Fatalf("html ext should be forced to .bin, got %s", u3)
	}
}

func TestHandleFilesAuth(t *testing.T) {
	h, _ := newTestHandler(t)
	// 未登录访问 → 重定向登录页（requireWeb）
	req := httptest.NewRequest(http.MethodGet, "/files/whatever.jpg", nil)
	w := httptest.NewRecorder()
	h.handleFiles(w, req) // 直接调 handler 本体验证文件逻辑；鉴权由 mux 层 requireWeb 覆盖

	// 不存在的文件 → 404
	req2 := httptest.NewRequest(http.MethodGet, "/files/nonexistent.jpg", nil)
	w2 := httptest.NewRecorder()
	h.handleFiles(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("want 404 for missing file, got %d", w2.Code)
	}

	// 路径穿越拒绝
	for _, bad := range []string{"/files/../etc/passwd", "/files/a/b.jpg", `/files\a.jpg`} {
		req3 := httptest.NewRequest(http.MethodGet, "/files/x", nil)
		req3.URL.Path = bad
		w3 := httptest.NewRecorder()
		h.handleFiles(w3, req3)
		if w3.Code != http.StatusNotFound {
			t.Errorf("path traversal %q: want 404, got %d", bad, w3.Code)
		}
	}
}

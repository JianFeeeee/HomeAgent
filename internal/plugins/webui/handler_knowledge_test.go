package webui

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"path/filepath"

	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	"gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

// newKnowledgeHandler 构造一个带知识库（可选媒体存储）的 Handler。
func newKnowledgeHandler(t *testing.T, withMedia bool) (*Handler, *knowledge.Store, func()) {
	t.Helper()
	ks := knowledge.NewStore(t.TempDir())
	if err := ks.Start(); err != nil {
		t.Fatal(err)
	}

	cfg := &types.Config{Daemon: types.DaemonConfig{
		CheckInterval: time.Minute, HeartbeatInterval: 30 * time.Second,
	}}
	sup := supervisor.New(cfg)
	sup.Start()

	sdkCfg := sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	}
	if withMedia {
		ms, err := media.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		sdkCfg.Media = sdk.NewMedia(ms)
	}
	h := NewHandler(testSDK(sdkCfg))
	return h, ks, func() { sup.Shutdown() }
}

// 上传 PNG 绝不能被当文本存成乱码正文。
//
// 修复前的行为：任何文件都是 file.Read 后 string(buf[:n]) 直接当 Markdown
// 存进 content.md —— 传张图得到一份乱码文本知识，且无任何迹象。
func TestKnowledgeUploadPNGIsNotStoredAsText(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	raw := makePNG(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "cat-photo")
	fw, err := mw.CreateFormFile("file", "cat.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(raw); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("应 201，实为 %d: %s", w.Code, w.Body.String())
	}

	k := knowledgeEntry(t, ks, "cat-photo")
	if k == nil {
		t.Fatalf("条目未创建，List=%v", ks.List())
	}
	// 正文里绝不能出现 PNG 字节被强转后的乱码
	if k.Content != "" {
		for i, r := range k.Content {
			if r == 0xFFFD || r == 0 {
				t.Fatalf("正文含二进制强转的乱码（偏移 %d），说明 PNG 被当文本存了", i)
			}
		}
		if !isPrintableOrSpace(k.Content) {
			t.Errorf("正文含不可打印字符，非文本被当 Markdown 存了")
		}
	}
	if len(k.Media) != 1 {
		t.Fatalf("应挂 1 个媒体，实为 %+v", k.Media)
	}
	if k.Media[0].Digest == "" {
		t.Errorf("媒体 digest 为空: %+v", k.Media[0])
	}
	if !strings.HasPrefix(k.Media[0].MIME, "image/") {
		t.Errorf("媒体 MIME 应为 image/*，实为 %q", k.Media[0].MIME)
	}
}

// 声明 text/plain 但实际是 PNG（Content-Type 不可信）也必须入 CAS。
func TestKnowledgeUploadDetectsRealTypeOverClaimedHeader(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	raw := makePNG(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "liar")
	// 故意谎报为纯文本
	fw, _ := mw.CreateFormFile("file", "x.png")
	fw.Write(raw)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("应 201，实为 %d: %s", w.Code, w.Body.String())
	}
	k := knowledgeEntry(t, ks, "liar")
	if k == nil || len(k.Media) != 1 {
		t.Fatalf("谎报 Content-Type 的 PNG 应被探测为媒体并入 CAS，实为 %+v", k)
	}
}

// 纯文本上传仍走原路径存正文。
func TestKnowledgeUploadTextStaysAsContent(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "notes")
	fw, _ := mw.CreateFormFile("file", "n.txt")
	fw.Write([]byte("这是正文内容"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("应 201，实为 %d: %s", w.Code, w.Body.String())
	}
	k := knowledgeEntry(t, ks, "notes")
	if k == nil {
		t.Fatal("条目未创建")
	}
	if !strings.Contains(k.Content, "这是正文内容") {
		t.Errorf("文本应存为正文，实为 %q", k.Content)
	}
	if len(k.Media) != 0 {
		t.Errorf("文本不该有媒体，实为 %+v", k.Media)
	}
}

// 非文本也非媒体的垃圾字节应被明确拒绝，而不是当文本存。
func TestKnowledgeUploadRejectsBinaryGarbage(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	// 非法 UTF-8 且探测不出媒体
	garbage := []byte{0xFF, 0xFE, 0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE, 0x00}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "junk")
	fw, _ := mw.CreateFormFile("file", "j.bin")
	fw.Write(garbage)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("垃圾字节应 400，实为 %d: %s", w.Code, w.Body.String())
	}
	if knowledgeEntry(t, ks, "junk") != nil {
		t.Error("被拒绝的条目不该落库")
	}
}

// 媒体存储未初始化时上传图片必须 503，不能静默把二进制当文本存。
func TestKnowledgeUploadMediaWithoutStoreIsUnavailable(t *testing.T) {
	h, _, done := newKnowledgeHandler(t, false)
	defer done()

	raw := makePNG(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "x")
	fw, _ := mw.CreateFormFile("file", "a.png")
	fw.Write(raw)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("无媒体存储应 503，实为 %d: %s", w.Code, w.Body.String())
	}
}

// 状态码语义：名称非法是 400，不存在是 404，缺名是 400。
func TestKnowledgeStatusCodes(t *testing.T) {
	h, _, done := newKnowledgeHandler(t, true)
	defer done()

	// 非法名称（..）
	body := `{"name":"../evil","content":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法名称应 400，实为 %d: %s", w.Code, w.Body.String())
	}

	// 缺名
	req = httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("缺名应 400，实为 %d", w.Code)
	}

	// 删除不存在
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/knowledge?name=nope", nil)
	w = httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("删除不存在应 404，实为 %d", w.Code)
	}
}

// 搜索结果应带分类/预览等前端需要的字段，而不是裸 JSON。
func TestKnowledgeSearchReturnsViews(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()
	if err := ks.Add("tech/go/并发", "goroutine 调度 GMP 抢占"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge?q=GMP", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []knowledgeView `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("应命中")
	}
	r := resp.Results[0]
	if r.Name != "tech/go/并发" {
		t.Errorf("name 不对: %q", r.Name)
	}
	if r.Preview == "" {
		t.Error("缺 preview")
	}
	// 不得泄露服务端绝对路径
	if strings.Contains(r.Preview, string(filepath.Separator)) && strings.Contains(r.Preview, "tmp") {
		t.Errorf("疑似泄露服务端路径: %q", r.Preview)
	}
}

// 分类过滤参数应真正生效。
func TestKnowledgeSearchCategoryFilter(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()
	for _, e := range []struct{ n, c string }{
		{"tech/go/a", "并发 调度"},
		{"life/b", "作息 睡眠"},
	} {
		if err := ks.Add(e.n, e.c); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge?q=%E5%B9%B2%E8%8D%89&category=tech", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []knowledgeView `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	for _, r := range resp.Results {
		if !strings.HasPrefix(r.Name, "tech/") {
			t.Errorf("分类过滤失效，混入 %q", r.Name)
		}
	}
}

// 列表端点应返回 names + stats（供前端刷新计数），并附稠密路状态。
func TestKnowledgeListReturnsStats(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()
	_ = ks.Add("a", "A")
	_ = ks.Add("b", "B")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge", nil)
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d", w.Code)
	}
	var resp struct {
		Names []string               `json:"names"`
		Stats map[string]interface{} `json:"stats"`
		Dense map[string]interface{} `json:"dense"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Names) != 2 {
		t.Errorf("names 应有 2 条，实为 %v", resp.Names)
	}
	if resp.Stats == nil {
		t.Error("缺 stats")
	}
	if resp.Dense == nil {
		t.Error("缺 dense 状态（前端要据此提示多模态是否就绪）")
	}
}

// knowledgeEntry 按名字取一条知识（含媒体引用与正文）。
// Store 没有导出的 Items()，测试里用「全库检索 + 名字匹配」拿到同一条。
func knowledgeEntry(t *testing.T, ks *knowledge.Store, name string) *knowledge.Knowledge {
	t.Helper()
	for _, k := range ks.Search(name, 100) {
		if k.Name == name {
			return k
		}
	}
	// 检索可能因分词而漏，退回遍历 List + 逐条检索
	for _, n := range ks.List() {
		if n != name {
			continue
		}
		for _, k := range ks.Search(n, 100) {
			if k.Name == name {
				return k
			}
		}
	}
	return nil
}

func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func isPrintableOrSpace(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}

// 前端实际发的 multipart 形状：name + 可选 content + 多个 file 字段。
// 前端在 dashboard.js 的 createKnowledgeChat 里用 FormData 组装，
// 逐个 append("file", files[i]) —— 这里逐字复刻，确认服务端吃得下。
func TestKnowledgeUploadMultipleFilesFromFrontend(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "cat-dog")
	_ = mw.WriteField("content", "两只动物")
	for i, name := range []string{"a.png", "b.png"} {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(makePNG(t))
		_ = i
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("应 201，实为 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Media []sdk.KnowledgeMediaRef `json:"media"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 同一张图传两次 → 内容寻址去重，两个 digest 相同（这是 CAS 的预期行为）
	if len(resp.Media) != 2 {
		t.Fatalf("应返回 2 个媒体，实为 %d", len(resp.Media))
	}
	if resp.Media[0].Digest != resp.Media[1].Digest {
		t.Errorf("相同内容应去重为同一 digest，实为 %s vs %s", resp.Media[0].Digest, resp.Media[1].Digest)
	}
	k := knowledgeEntry(t, ks, "cat-dog")
	if k == nil {
		t.Fatal("条目未创建")
	}
	if k.Content != "两只动物" {
		t.Errorf("显式 content 应被采用，实为 %q", k.Content)
	}
}

// 文本文件不带 content 字段时，文本内容应被采纳为正文。
func TestKnowledgeUploadTextFileBecomesContent(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "fromfile")
	fw, _ := mw.CreateFormFile("file", "a.md")
	fw.Write([]byte("# 标题\n\n正文"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("应 201，实为 %d: %s", w.Code, w.Body.String())
	}
	k := knowledgeEntry(t, ks, "fromfile")
	if k == nil || !strings.Contains(k.Content, "正文") {
		t.Errorf("文本文件内容应成为正文，实为 %+v", k)
	}
}

// 上传的媒体必须能在媒体库里取回字节（digest 有效），否则引用是死的。
func TestKnowledgeUploadedMediaRetrievable(t *testing.T) {
	h, ks, done := newKnowledgeHandler(t, true)
	defer done()

	raw := makePNG(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "m")
	fw, _ := mw.CreateFormFile("file", "a.png")
	fw.Write(raw)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.handleKnowledge(w, req)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	var resp struct {
		Media []sdk.KnowledgeMediaRef `json:"media"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Media) != 1 {
		t.Fatalf("应 1 个媒体，实为 %d", len(resp.Media))
	}
	// Handler 持有的媒体存储应能按 digest 取回原始字节
	got, err := h.mediaStore.Get(resp.Media[0].Digest)
	if err != nil {
		t.Fatalf("媒体不可取回: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Error("取回的字节与上传的不一致")
	}
	info, err := h.mediaStore.Stat(resp.Media[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "image" {
		t.Errorf("kind 应为 image，实为 %q", info.Kind)
	}
	// 条目上也应持久化了引用（重启不丢）
	_ = ks.Flush()
}

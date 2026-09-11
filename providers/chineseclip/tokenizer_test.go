package chineseclip

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// referenceText 是官方导出时冻结的逐文本 token 参考。
type referenceText struct {
	Text         string    `json:"text"`
	InputIDs     []int64   `json:"input_ids"`
	Attention    []int64   `json:"attention_mask"`
	Vector       []float64 `json:"vector"`
	PixelsSHA256 string    `json:"pixels_sha256"`
	Name         string    `json:"name"`
}

type reference struct {
	Texts  []referenceText `json:"texts"`
	Images []referenceText `json:"images"`
}

func loadReference(t *testing.T, dir string) reference {
	t.Helper()
	path := filepath.Join(dir, "reference.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("缺少冻结参考 %s（由 scripts/export_chineseclip_onnx.py 生成）: %v", path, err)
	}
	var ref reference
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatalf("解析参考 %s: %v", path, err)
	}
	return ref
}

func modelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CHINESECLIP_MODEL_DIR")
	if dir == "" {
		t.Skip("未设置 CHINESECLIP_MODEL_DIR，跳过需要真实产物的用例")
	}
	if _, err := os.Stat(filepath.Join(dir, "embed_config.json")); err != nil {
		t.Skipf("模型目录 %s 缺 embed_config.json: %v", dir, err)
	}
	return dir
}

// 分词器必须与官方 Chinese-CLIP 逐 token 一致。
//
// 这条测试是有来历的：第一版探针自己拼 BertTokenizer（只给 vocab.txt、没删音标、
// 中文没逐字切），中文全被切成 [UNK]，三个不同句子产出几乎相同的向量
// （余弦 0.98）——差点把「模型坏了」当成结论。分词不一致会静默毁掉整个向量空间。
func TestTokenizerMatchesOfficialReference(t *testing.T) {
	dir := modelDir(t)
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("读取 embed_config.json: %v", err)
	}
	tok, err := LoadTokenizer(dir, cfg.MaxLength)
	if err != nil {
		t.Fatalf("加载分词器: %v", err)
	}
	ref := loadReference(t, dir)
	if len(ref.Texts) == 0 {
		t.Fatal("参考里没有文本用例")
	}
	for _, c := range ref.Texts {
		ids, mask := tok.Encode(c.Text)
		if !reflect.DeepEqual(ids, c.InputIDs) {
			t.Errorf("input_ids 不一致 %q\n  got  %v\n  want %v", c.Text, ids, c.InputIDs)
		}
		if !reflect.DeepEqual(mask, c.Attention) {
			t.Errorf("attention_mask 不一致 %q\n  got  %v\n  want %v", c.Text, mask, c.Attention)
		}
	}
}

// 不依赖真实产物的纯逻辑用例：覆盖中文逐字、删音标、标点切分、UNK、补齐。
func TestTokenizerUnitCases(t *testing.T) {
	dir := t.TempDir()
	vocab := []string{
		"[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]",
		"红", "色", "一", "张", "方", "块", "的", "图", "片",
		"cafe", "hello", "world", "##ive", "na", "a", "-", "b", "（", "）", "括", "号", "12345",
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.txt"),
		[]byte(joinLines(vocab)), 0o644); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadTokenizer(dir, 8)
	if err != nil {
		t.Fatalf("加载分词器: %v", err)
	}

	cases := []struct {
		name string
		text string
		want []string // 期望的 token 文本（不含特殊 token，便于阅读）
	}{
		{"中文逐字", "红色", []string{"红", "色"}},
		{"删音标+小写", "CAFÉ", []string{"cafe"}},
		{"删音标词内组合", "naïve", []string{"na", "##ive"}},
		{"标点切分", "a-b", []string{"a", "-", "b"}},
		{"全角括号按标点处理", "（括号）", []string{"（", "括", "号", "）"}},
		{"纯数字", "12345", []string{"12345"}},
	}
	for _, c := range cases {
		got := tok.tokenize(c.text)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q\n  got  %v\n  want %v", c.name, c.text, got, c.want)
		}
	}

	// 未登录词整体退化为 [UNK]（与 HF 一致）。
	if got := tok.tokenize("zzz"); !reflect.DeepEqual(got, []string{"[UNK]"}) {
		t.Errorf("未登录词应退化为 [UNK]，得到 %v", got)
	}

	// 补齐：maxLength=8，"红色" 只占 2 个位置，其余补 [PAD]。
	ids, mask := tok.Encode("红色")
	if len(ids) != 8 || len(mask) != 8 {
		t.Fatalf("补齐长度应为 8，得到 ids=%d mask=%d", len(ids), len(mask))
	}
	if ids[0] != 2 || ids[1] != 5 || ids[2] != 6 || ids[3] != 3 {
		t.Errorf("应为 [CLS] 红 色 [SEP]，得到 %v", ids[:4])
	}
	for i := 4; i < 8; i++ {
		if ids[i] != 0 {
			t.Errorf("位置 %d 应为 [PAD]，得到 %v", i, ids)
		}
	}
	wantMask := []int64{1, 1, 1, 1, 0, 0, 0, 0}
	if !reflect.DeepEqual(mask, wantMask) {
		t.Errorf("attention_mask 应为 %v，得到 %v", wantMask, mask)
	}

	// 截断：9 个汉字在 maxLength=8 下只保留 6 个，正好填满，不应出现 [PAD]。
	truncIDs, truncMask := tok.Encode("红色一张方块的图片")
	if len(truncIDs) != 8 {
		t.Fatalf("截断后长度应为 8，得到 %d", len(truncIDs))
	}
	if truncIDs[0] != 2 || truncIDs[7] != 3 {
		t.Errorf("截断后首尾应为 [CLS]/[SEP]，得到 %v", truncIDs)
	}
	for i, id := range truncIDs {
		if id == 0 {
			t.Errorf("截断后位置 %d 不应是 [PAD]: %v", i, truncIDs)
		}
	}
	if !reflect.DeepEqual(truncMask, []int64{1, 1, 1, 1, 1, 1, 1, 1}) {
		t.Errorf("截断后 attention_mask 应全为 1，得到 %v", truncMask)
	}
}

func joinLines(items []string) string {
	out := ""
	for _, item := range items {
		out += item + "\n"
	}
	return out
}

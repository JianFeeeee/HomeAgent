package plugin

import "testing"

func TestExtractDescriptionSkipsFrontmatter(t *testing.T) {
	// 带 frontmatter：描述取正文首行
	c := "---\nname: demo\nversion: 1.0.0\n---\n\n# Demo\n\n这是一句描述。\n"
	if d := extractDescription(c); d != "这是一句描述。" {
		t.Fatalf("got %q, want 这是一句描述。", d)
	}
	// 无 frontmatter：兼容旧格式
	c2 := "# Demo\n\n老格式描述\n"
	if d := extractDescription(c2); d != "老格式描述" {
		t.Fatalf("got %q, want 老格式描述", d)
	}
	// frontmatter 闭合后紧跟标题仍不误判
	c3 := "---\nname: x\n---\n## 步骤\n\n正文描述\n"
	if d := extractDescription(c3); d != "正文描述" {
		t.Fatalf("got %q, want 正文描述", d)
	}
}

func TestValidateSKILLContent(t *testing.T) {
	good := "---\nname: ok\n---\n\n# OK\n\n描述\n"
	if err := ValidateSKILLContent(good); err != nil {
		t.Fatalf("good content rejected: %v", err)
	}
	if err := ValidateSKILLContent(""); err == nil {
		t.Fatal("empty content should be rejected")
	}
	noDesc := "---\nname: x\n---\n\n## 步骤\n"
	if err := ValidateSKILLContent(noDesc); err == nil {
		t.Fatal("content without description should be rejected")
	}
}

func TestExtractFieldStripsQuotes(t *testing.T) {
	c := "---\nname: demo\nversion: \"1.0\"\nauthor: 'tester'\n---\n"
	if v := extractField(c, "version"); v != "1.0" {
		t.Fatalf("version = %q, want 1.0", v)
	}
	if v := extractField(c, "author"); v != "tester" {
		t.Fatalf("author = %q, want tester", v)
	}
	if v := extractField(c, "name"); v != "demo" {
		t.Fatalf("name = %q, want demo", v)
	}
}

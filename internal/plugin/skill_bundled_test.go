package plugin

import (
	"path/filepath"
	"testing"
)

// 随发行版分发的 knowledge-base skill 必须能被 LoadSKILL 正确解析。
//
// 这条断言的由来：SKILL.md 里**非白名单的二级标题（## ）会被
// extractToolDefs 当成工具定义**，进而 ValidateSKILLContent 报
// "invalid tool name"。写文档时用中文小标题（"## 前提：…"）就会踩到，
// 报错的提示与真正的原因（标题层级）毫无关联，极难自查。
// 故把「bundled skill 必须可加载且可校验」固化成测试。
func TestBundledSkillsAreLoadable(t *testing.T) {
	root := filepath.Join("..", "..", "assets", "skills")
	dirs, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Skip("没有随发行版分发的 skill")
	}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			sk, err := LoadSKILL(dir)
			if err != nil {
				t.Fatalf("LoadSKILL 失败: %v", err)
			}
			if sk.Name() == "" {
				t.Error("skill 名为空")
			}
			if sk.Description() == "" || sk.Description() == "---" {
				t.Errorf("description 解析失败（frontmatter 未被正确跳过）: %q", sk.Description())
			}
			if err := ValidateSKILLContent(sk.RawContent()); err != nil {
				t.Errorf("ValidateSKILLContent 失败: %v", err)
			}
		})
	}
}

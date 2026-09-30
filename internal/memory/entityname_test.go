package memory

import "testing"

// 纯数字实体（端口/分机/编号类属性值）必须合法。
// 背景（2026-10-01 跑分实测）：旧实现要求"含字母/汉字"导致纯数字值
// 被静默拒写——metrics 端口 8328、分机 4379→4324 全部丢进黑洞，
// 且 commit 返回"成功"，模型永不重试。
func TestValidEntityName_AllowsNumericValues(t *testing.T) {
	for _, name := range []string{"8328", "4379", "8080", "v2", "COPPER-8177", "值班室的分机号"} {
		if !validEntityName(name) {
			t.Errorf("应合法: %q", name)
		}
	}
}

func TestValidEntityName_RejectsJunk(t *testing.T) {
	for _, name := range []string{"", "a", "——！！", "。。。", "   "} {
		if validEntityName(name) {
			t.Errorf("应拒绝: %q", name)
		}
	}
}

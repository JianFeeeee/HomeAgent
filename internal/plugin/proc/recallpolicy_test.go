package proc

import (
	"strings"
	"testing"
)

// 非法 recall_policy 必须报错，而不是静默当成默认（auto）。
//
// 与 context_policy 同理：静默降级会让调用方以为自己声明的“不召回”在生效，
// 而 meta 文本（如中断通知）仍在照常召回，且没有任何报错可循。
func TestValidateRecallPolicy(t *testing.T) {
	ok := []string{"", "none", "auto"}
	for _, policy := range ok {
		if err := validateRecallPolicy("tool.register", policy); err != nil {
			t.Errorf("合法取值 %q 被拒绝: %v", policy, err)
		}
	}

	bad := []string{"auto ", "AUTO", "None", "true", "always", "召回"}
	for _, policy := range bad {
		err := validateRecallPolicy("io.injectText", policy)
		if err == nil {
			t.Errorf("非法取值 %q 应被拒绝", policy)
			continue
		}
		if !strings.Contains(err.Error(), "io.injectText") || !strings.Contains(err.Error(), policy) {
			t.Errorf("错误信息应包含位置与实际值，实际: %v", err)
		}
	}
}

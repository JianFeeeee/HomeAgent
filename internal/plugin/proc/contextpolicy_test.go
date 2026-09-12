package proc

import (
	"strings"
	"testing"
)

// 非法 context_policy 必须报错，而不是静默当成 none。
//
// 为什么这条值得单独测：把拼写错误降级成「不裁剪」不会有任何报错、日志或
// 行为异常——调用方会一直以为自己声明的裁剪在生效，直到某天上下文被撑爆。
// 这类静默降级是本次改造要消掉的东西，所以要钉住。
func TestValidateContextPolicy(t *testing.T) {
	ok := []string{"", "none", "prune"}
	for _, policy := range ok {
		if err := validateContextPolicy("tool.register", policy); err != nil {
			t.Errorf("合法取值 %q 被拒绝: %v", policy, err)
		}
	}

	bad := []string{"prune ", "PRUNE", "True", "None", "always", "裁剪"}
	for _, policy := range bad {
		err := validateContextPolicy("io.injectText", policy)
		if err == nil {
			t.Errorf("非法取值 %q 应被拒绝", policy)
			continue
		}
		// 报错要指出位置与实际值，否则排查时不知道是谁传错了。
		if !strings.Contains(err.Error(), "io.injectText") || !strings.Contains(err.Error(), policy) {
			t.Errorf("错误信息应包含位置与实际值，实际: %v", err)
		}
	}
}

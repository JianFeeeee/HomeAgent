//go:build linux || darwin

package plugins

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// deepsearch 插件的真实调用往返：内核 ExecuteTool → RPC → 插件子进程 → 本地 SearXNG → 结果回传。
//
// 与 TestRealPlugin_ToolInvokeRoundTrip 的区别：那条只断言「链路通（拿到结果或拿到插件侧的错误）」，
// 这条断言**内容形状**——返回里必须有「摘要」与「引擎覆盖度」。这正是旧实现（抓 Bing HTML）
// 拿不到的东西，也是「搜索能力不行」的根因，所以它必须成为回归判据。
//
// 前置：本机 127.0.0.1:8888 上跑着 SearXNG（部署见 /root/searxng-agent）。
// 未启动时 fail 并给出可操作提示，而不是 skip —— 否则这条判据会在环境退化时静默失效。
func TestRealPlugin_DeepSearchInvoke(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installRealPlugin(t, plgDir, "deepsearch")

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}
	if env.pluginReg.Get("deepsearch") == nil {
		t.Fatal("deepsearch 未经 proc 通道加载")
	}

	var toolName string
	for _, def := range env.stageHost.GetToolDefs() {
		if strings.HasPrefix(def.Name, "deepsearch") && strings.HasSuffix(def.Name, "_search") {
			toolName = def.Name
			break
		}
	}
	if toolName == "" {
		t.Fatal("deepsearch 未注册检索工具")
	}
	t.Logf("调用工具 %s", toolName)

	// 用当初失败的那条查询做判据
	res, err := env.stageHost.ExecuteTool(toolName, map[string]interface{}{
		"query": "深度科技 deepin 开发者 被开除",
		"count": float64(3),
	})
	if err != nil {
		if strings.Contains(err.Error(), "not found in any plugin") {
			t.Fatalf("工具未注册到 stageHost: %v", err)
		}
		t.Fatalf("工具调用失败（检查本机 SearXNG 是否在 127.0.0.1:8888 运行）: %v", err)
	}
	if res == nil {
		t.Fatal("工具返回 nil 且无错误")
	}

	text := fmt.Sprintf("%v", res)
	t.Logf("工具返回前 500 字：\n%s", truncRunes(text, 500))

	if !strings.Contains(text, "摘要：") {
		t.Errorf("返回内容缺少摘要——这正是旧实现拿不到的部分：\n%s", truncRunes(text, 800))
	}
	if !strings.Contains(text, "覆盖：") {
		t.Errorf("返回内容缺少引擎覆盖度（模型据此判断可信度）：\n%s", truncRunes(text, 800))
	}
	if !strings.Contains(text, "http") {
		t.Errorf("返回内容缺少结果链接：\n%s", truncRunes(text, 800))
	}
}

// deepsearch_status 也走一遍真实调用：它把「后端是否可用、哪些引擎在出结果」暴露成工具，
// 出问题时 agent 可以先自检，而不是盲目换词重搜。
func TestRealPlugin_DeepSearchStatusInvoke(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installRealPlugin(t, plgDir, "deepsearch")
	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}

	var toolName string
	for _, def := range env.stageHost.GetToolDefs() {
		if strings.HasPrefix(def.Name, "deepsearch") && strings.HasSuffix(def.Name, "_status") {
			toolName = def.Name
			break
		}
	}
	if toolName == "" {
		t.Fatal("deepsearch 未注册自检工具")
	}

	res, err := env.stageHost.ExecuteTool(toolName, map[string]interface{}{"probe": "test"})
	if err != nil {
		t.Fatalf("自检调用失败（检查本机 SearXNG 是否运行）: %v", err)
	}
	text := fmt.Sprintf("%v", res)
	t.Logf("自检返回：%s", truncRunes(text, 400))

	for _, want := range []string{"healthz", "search_ok", "engines_returning_results"} {
		if !strings.Contains(text, want) {
			t.Errorf("自检结果缺少字段 %q：%s", want, truncRunes(text, 500))
		}
	}
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

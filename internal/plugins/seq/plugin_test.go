package seq

import (
	"strings"
	"testing"
)

// 阶段 P4：seq_* 工具与插件装配。
//
// 这一层的判据关注**可观测契约**（给模型看的东西对不对），
// 执行语义已由 P1/P2/P3 的判据覆盖。

// ① 六个工具全部注册，且都带可用的 description 与参数 schema。
//
// 工具是**模型可见面**：description 缺失或为空白 ⇒ 模型不知道何时该用它。
func TestAllSixSeqToolsRegistered(t *testing.T) {
	p := newTestPlugin(t)
	want := []string{
		"seq_create", "seq_list", "seq_delete", "seq_run", "seq_call", "seq_when_call",
	}
	defs := p.toolDefs()
	for _, name := range want {
		def, ok := defs[name]
		if !ok {
			t.Errorf("工具 %s 未注册（已注册：%v）", name, keysOf(defs))
			continue
		}
		if strings.TrimSpace(def.Description) == "" {
			t.Errorf("工具 %s 的 description 为空 —— 模型无从判断何时使用", name)
		}
		if def.Parameters == nil {
			t.Errorf("工具 %s 缺参数 schema", name)
			continue
		}
		if typ, _ := def.Parameters["type"].(string); typ != "object" {
			t.Errorf("工具 %s 的 schema type = %q，期望 object", name, typ)
		}
	}
	if len(defs) != len(want) {
		t.Errorf("应恰好注册 %d 个工具，实际 %d（%v）—— 多余的导出工具会稀释工具面", len(want), len(defs), keysOf(defs))
	}
}

// ② seq_create 的 schema 必须声明 required，否则模型会漏传。
func TestSeqCreateSchemaDeclaresRequired(t *testing.T) {
	p := newTestPlugin(t)
	def := p.toolDefs()["seq_create"]
	if def.Parameters == nil {
		t.Fatal("seq_create 缺 schema")
	}
	req, _ := def.Parameters["required"].([]string)
	if len(req) == 0 {
		t.Fatalf("seq_create 未声明 required：模型会漏传 name/groups/file")
	}
	got := map[string]bool{}
	for _, r := range req {
		got[r] = true
	}
	if !got["name"] {
		t.Error("required 应包含 name")
	}
}

// ③ seq_create 的 description 必须说明 groups 与 file **二选一**。
//
// 这是最容易让模型犯错的地方：两个都传或都不传该怎么���，必须写清。
func TestSeqCreateExplainsMutuallyExclusiveInput(t *testing.T) {
	p := newTestPlugin(t)
	desc := p.toolDefs()["seq_create"].Description
	for _, want := range []string{"groups", "file"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description 未提到 %s：%s", want, desc)
		}
	}
	if !strings.Contains(desc, "二选一") && !strings.Contains(desc, "或") {
		t.Errorf("description 未说明 groups 与 file 是二选一：%s", desc)
	}
}

// ④ 六个工具都**不得**声明并发安全。
//
// seq_run / seq_call 会执行**一串**工具，其中可能含写操作；把它们标成
// 并发安全，会让内核把两条 seq_run 并发跑起来 ⇒ 两个序列的执行顺序
// 交错、变量表互相污染。
func TestSeqToolsAreNotParallelSafe(t *testing.T) {
	p := newTestPlugin(t)
	for name := range p.toolDefs() {
		def := p.toolDefs()[name]
		if def.ParallelSafe {
			t.Errorf("工具 %s 声明了 ParallelSafe —— seq 会执行一串工具，并发会污染执行序列", name)
		}
	}
}

// ⑤ seq_run 的 description 必须说明"按 groups 数组顺序执行"。
//
// 组的执行顺序是**语义**的一部分：条件依赖前面的槽，顺序反了结果就错。
func TestSeqRunExplainsOrdering(t *testing.T) {
	p := newTestPlugin(t)
	desc := p.toolDefs()["seq_run"].Description
	if !strings.Contains(desc, "顺序") {
		t.Errorf("seq_run 未说明组按顺序执行：%s", desc)
	}
}

// ⑥ 黑名单：序列**内部**不得调用这些工具（与子 agent 的黑名单同源，防递归）。
//
// ⚠️ 这里曾有一个我自己的设计矛盾：判据原先把 `seq_call` / `seq_when_call`
// 也要求进黑名单，但「按名调用 group/序列」恰恰是本包的核心能力——
// 若禁掉它，序列就退化成单层脚本，功能归零。
//
// 分层澄清（这也是正确语义）：
//
//	· `seq_call` / `seq_when_call` 作为**模型直接调用**的入口是正常的
//	  （模型可以单独调某个 group），不算黑名单；
//	· 序列**内部**若写 seq_call，那是按名组合，走 runGroup 的专门分支
//	  并受 maxCallDepth 约束（§8.3 的环检测与深度上界），
//	  不靠黑名单防递归。
//	· 真正要禁的是：对外发消息（output_send__）、起子 agent（spawn_child）、
//	  改插件表（plgreload）、以及再次 seq_run 整条序列（会绕过深度计数的语义）。
func TestSeqBlacklistCoversRecursionRisks(t *testing.T) {
	for _, name := range []string{
		"output_send__qq", "output_send__x", "spawn_child", "plgreload", "seq_run",
	} {
		if !blacklisted(name) {
			t.Errorf("黑名单未覆盖 %q —— 序列能调它就是递归/绕过风险", name)
		}
	}
	// seq_call / seq_when_call 必须**不在**黑名单，否则按名调用能力归零
	for _, name := range []string{"seq_call", "seq_when_call"} {
		if blacklisted(name) {
			t.Errorf("黑名单误伤了 %q —— 它是序列组合的核心能力（递归由 maxCallDepth + 环检测负责）", name)
		}
	}
	// 普通工具不应被误伤
	for _, name := range []string{"cmd_run", "knowledge_search", "output_list_channels"} {
		if blacklisted(name) {
			t.Errorf("黑名单误伤了正常工具 %q", name)
		}
	}
}

func keysOf(m map[string]toolDefInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- 测试替身 ----

// newTestPlugin 造一个**不依赖内核**的 Plugin。
//
// 刻意不走 Start（那需要真实 *sdk.PluginSDK）：本组判据只测
// 「工具定义与可观测契约」，不测执行语义（后者由 exec/store 的判据覆盖）。
func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	return &Plugin{name: "seq", store: NewStore(t.TempDir())}
}

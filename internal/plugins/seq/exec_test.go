package seq

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 阶段 P2：执行引擎（组内并行 + 具名槽 + 条件求值）。
//
// 三个核心不变量：
//  1. **组边界即屏障**：同组内工具互相不可见（并行 ⇒ 无确定写序）；
//     变量只在组屏障处按 tools 顺序**确定性合并**。
//  2. **条件求值失败必须报错**，不得降级成「条件为假」——
//     那会让序列安静地少做一步而模型以为跑完了。
//  3. 结果**按 tools 数组顺序**合并，与完成顺序无关 ⇒ 可复现。

// fakeTool 记录一次工具执行，并返回可配置的文本。
type fakeTool struct {
	mu    sync.Mutex
	calls []string
	// results 按工具名给出返回文本；未配置则返回 "ran:<name>"
	results map[string]string
	// errs 按工具名给出错误
	errs map[string]error
	// delay 用于制造"完成顺序 ≠ 声明顺序"
	delay map[string]int
}

func newFakeTool() *fakeTool {
	return &fakeTool{results: map[string]string{}, errs: map[string]error{}, delay: map[string]int{}}
}

func (f *fakeTool) call(name string, _ map[string]interface{}) (string, error) {
	if d := f.delay[name]; d > 0 {
		sleepMS(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if e, ok := f.errs[name]; ok {
		return "", e
	}
	if r, ok := f.results[name]; ok {
		return r, nil
	}
	return "ran:" + name, nil
}

// parallelSafe：默认全部视为可并发（工具级压测通过 toolDefs 控制）。
// 本文件用 fakeTool 的用例关注的是**组内顺序/合并**不变式，
// 并发资格由 TestStress_* 单独检验。
func (f *fakeTool) parallelSafe(string) bool { return true }

func (f *fakeTool) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// ① 具名槽：工具结果按 `as` 写入对应槽。
func TestExecWritesNamedSlots(t *testing.T) {
	ft := newFakeTool()
	g := Group{
		Name:     "g",
		In:       map[string]string{},
		Out:      map[string]string{"summary": "string", "load": "string"},
		When:     "true",
		Parallel: false,
		Tools: []ToolCall{
			{Tool: "uptime", As: "summary"},
			{Tool: "top", As: "load"},
		},
	}
	res, err := execGroup(g, map[string]interface{}{}, ft)
	if err != nil {
		t.Fatalf("execGroup: %v", err)
	}
	if got, _ := res.Slots["summary"].(string); got != "ran:uptime" {
		t.Errorf("summary 槽 = %v", res.Slots["summary"])
	}
	if got, _ := res.Slots["load"].(string); got != "ran:top" {
		t.Errorf("load 槽 = %v", res.Slots["load"])
	}
}

// ② ★ 结果合并必须按 tools 数组顺序，与完成顺序无关。
//
// 并行下完成顺序不确定；若按完成顺序合并，同样的输入会产出不同的槽内容，
// 整条序列**不可复现**。这里让后声明的工具先完成（delay 更短）。
func TestExecMergesSlotsInDeclarationOrder(t *testing.T) {
	ft := newFakeTool()
	ft.delay["first"] = 30 // 先声明但后完成
	ft.delay["second"] = 1
	ft.results["first"] = "FIRST"
	ft.results["second"] = "SECOND"

	g := Group{
		Name: "g", In: map[string]string{},
		Out:  map[string]string{"a": "array", "b": "array"},
		When: "true", Parallel: true,
		Tools: []ToolCall{
			{Tool: "first", As: "a"},
			{Tool: "second", As: "b"},
		},
	}
	res, err := execGroup(g, map[string]interface{}{}, ft)
	if err != nil {
		t.Fatalf("execGroup: %v", err)
	}
	// 各自的槽内容正确
	ra, _ := res.Slots["a"].([]interface{})
	rb, _ := res.Slots["b"].([]interface{})
	if len(ra) != 1 || ra[0] != "FIRST" {
		t.Errorf("槽 a = %v", ra)
	}
	if len(rb) != 1 || rb[0] != "SECOND" {
		t.Errorf("槽 b = %v", rb)
	}
	// 完成顺序确实被打乱（否则本用例测不到并发）
	if got := ft.called(); got[0] != "first" || got[1] != "second" {
		t.Logf("完成顺序 = %v（未打乱，判据可能测不到并发）", got)
	}
}

// ③ array 槽的同名 as：在组屏障按 tools 顺序**确定性追加**。
func TestExecArraySlotAppendsInOrder(t *testing.T) {
	ft := newFakeTool()
	ft.delay["slow"] = 30
	ft.results["a"] = "A"
	ft.results["c"] = "C"
	// "slow" 用默认返回值即可（ran:slow），它只用来制造"最后完成"

	g := Group{
		Name: "g", In: map[string]string{},
		Out:  map[string]string{"xs": "array"},
		When: "true", Parallel: true,
		Tools: []ToolCall{
			{Tool: "a", As: "xs"},
			{Tool: "slow", As: "xs"}, // 声明在中间但最后完成
			{Tool: "c", As: "xs"},
		},
	}
	res, err := execGroup(g, map[string]interface{}{}, ft)
	if err != nil {
		t.Fatalf("execGroup: %v", err)
	}
	xs, _ := res.Slots["xs"].([]interface{})
	if len(xs) != 3 {
		t.Fatalf("xs 应有 3 项，实际 %d（%v）", len(xs), xs)
	}
	// 必须按 tools 声明顺序：a, slow, c ⇒ A, B, C
	// 期望按 tools 声明顺序：a → slow → c
	want := []string{"A", "ran:slow", "C"}
	for i, w := range want {
		if xs[i] != w {
			t.Errorf("xs[%d] = %v，期望 %v（完整 %v）—— 合并顺序依赖了完成顺序", i, xs[i], w, xs)
		}
	}
}

// ④ 条件为假 ⇒ 整组跳过，**槽不赋值**。
func TestExecSkipsGroupWhenConditionFalse(t *testing.T) {
	ft := newFakeTool()
	g := Group{
		Name: "g", In: map[string]string{"flag": "bool"},
		Out:  map[string]string{"x": "string"},
		When: "false", Parallel: false,
		Tools: []ToolCall{{Tool: "uptime", As: "x"}},
	}
	res, err := execGroup(g, map[string]interface{}{"flag": false}, ft)
	if err != nil {
		t.Fatalf("execGroup: %v", err)
	}
	if !res.Skipped {
		t.Error("条件为假时应标记 Skipped")
	}
	if len(ft.called()) != 0 {
		t.Errorf("条件为假却执行了工具: %v", ft.called())
	}
	if _, ok := res.Slots["x"]; ok {
		t.Error("条件为假时不应给槽赋值（后续组会读到不存在的值）")
	}
}

// ⑤ ★ 条件**求值出错**必须报错，不得降级成「条件为假」。
//
// 这是本阶段最关键的一条：把求值失败降级为跳过 = 序列安静地少做一步，
// 而模型以为跑完了 —— 与「静默吞工具」同族。
func TestExecErrorsOnMalformedCondition(t *testing.T) {
	cases := []string{
		"$args.",             // 空键名
		"$args.missing == 1", // 引用了未声明的入参（in 里只有 flag）
		"1 ==",               // 语法不完整
	}
	for _, cond := range cases {
		t.Run(cond, func(t *testing.T) {
			ft := newFakeTool()
			g := Group{
				Name: "g", In: map[string]string{"flag": "bool"},
				Out:  map[string]string{"x": "string"},
				When: cond, Parallel: false,
				Tools: []ToolCall{{Tool: "uptime", As: "x"}},
			}
			_, err := execGroup(g, map[string]interface{}{"flag": false}, ft)
			if err == nil {
				t.Fatalf("畸形条件 %q 未被拒绝（被静默当成假了吗）", cond)
			}
			if !strings.Contains(err.Error(), "条件") {
				t.Errorf("错误信息应提到『条件』，实际: %v", err)
			}
			if len(ft.called()) != 0 {
				t.Errorf("条件求值失败却执行了工具: %v", ft.called())
			}
		})
	}
}

// ⑥ 条件为真时正常执行。
func TestExecRunsWhenConditionTrue(t *testing.T) {
	ft := newFakeTool()
	g := Group{
		Name: "g", In: map[string]string{"flag": "bool"},
		Out:  map[string]string{"x": "string"},
		When: "$args.flag == true", Parallel: false,
		Tools: []ToolCall{{Tool: "uptime", As: "x"}},
	}
	res, err := execGroup(g, map[string]interface{}{"flag": true}, ft)
	if err != nil {
		t.Fatalf("execGroup: %v", err)
	}
	if res.Skipped {
		t.Error("条件为真却跳过了")
	}
	if got, _ := res.Slots["x"].(string); got != "ran:uptime" {
		t.Errorf("x = %v", res.Slots["x"])
	}
}

// ⑦ 工具执行失败：on_error=continue 时继续，abort 时整组失败。
func TestExecOnErrorPolicy(t *testing.T) {
	boom := errors.New("boom")

	t.Run("abort", func(t *testing.T) {
		ft := newFakeTool()
		ft.errs["bad"] = boom
		g := Group{
			Name: "g", In: map[string]string{},
			Out:  map[string]string{"a": "string", "b": "string"},
			When: "true", Parallel: false, OnError: "abort",
			Tools: []ToolCall{{Tool: "bad", As: "a"}, {Tool: "ok", As: "b"}},
		}
		if _, err := execGroup(g, map[string]interface{}{}, ft); err == nil {
			t.Error("on_error=abort 时失败应使整组失败")
		}
	})

	t.Run("continue", func(t *testing.T) {
		ft := newFakeTool()
		ft.errs["bad"] = boom
		g := Group{
			Name: "g", In: map[string]string{},
			Out:  map[string]string{"a": "string", "b": "string"},
			When: "true", Parallel: false, OnError: "continue",
			Tools: []ToolCall{{Tool: "bad", As: "a"}, {Tool: "ok", As: "b"}},
		}
		res, err := execGroup(g, map[string]interface{}{}, ft)
		if err != nil {
			t.Fatalf("on_error=continue 不应整组失败: %v", err)
		}
		if got, _ := res.Slots["b"].(string); got != "ran:ok" {
			t.Errorf("continue 下后续工具应仍执行，b = %v", res.Slots["b"])
		}
		// 失败的槽也要有值（错误文本），否则后续组读到缺失
		if _, ok := res.Slots["a"]; !ok {
			t.Error("continue 下失败的工具也应留下槽（记错误文本）")
		}
	})
}

// ⑧ 变量插值：`$args.x` 被真实值替换，且**整值引用**保留类型
// （数字仍是数字，不是字符串）。
func TestExecSubstitutesArgs(t *testing.T) {
	ft := newFakeTool()
	g := Group{
		Name: "g",
		In:   map[string]string{"host": "string", "count": "integer"},
		Out:  map[string]string{"x": "string"},
		When: "true", Parallel: false,
		Tools: []ToolCall{{
			Tool: "cmd",
			Args: map[string]interface{}{
				"cmd":    "ssh $args.host",
				"amount": "$args.count",
				"whole":  "$args.host",
			},
			As: "x",
		}},
	}
	// 注意：本例只验证 execTool 前的插值由 call 接口承担，
	// 这里通过 fakeTool 捕获实际收到的 args。
	capture := &capturingTool{inner: ft}
	if _, err := execGroup(g, map[string]interface{}{"host": "node-a", "count": 3}, capture); err != nil {
		t.Fatalf("execGroup: %v", err)
	}
	got := capture.lastArgs()
	if got["cmd"] != "ssh node-a" {
		t.Errorf("字符串内插值失败: %v", got["cmd"])
	}
	// 整值引用必须保留原始类型（数字仍是数字）
	if n, ok := got["amount"].(int); !ok || n != 3 {
		t.Errorf("整值引用应保留 int 类型，实际 %#v", got["amount"])
	}
	if got["whole"] != "node-a" {
		t.Errorf("整值引用失败: %#v", got["whole"])
	}
}

// capturingTool 记录最后一次收到的 args。
type capturingTool struct {
	inner *fakeTool
	mu    sync.Mutex
	args  map[string]interface{}
}

func (c *capturingTool) parallelSafe(string) bool { return true }

func (c *capturingTool) call(name string, args map[string]interface{}) (string, error) {
	c.mu.Lock()
	c.args = args
	c.mu.Unlock()
	return c.inner.call(name, args)
}
func (c *capturingTool) lastArgs() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.args
}

// sleepMS 测试用的短延时（毫秒）。
func sleepMS(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

var _ toolRunner = (*fakeTool)(nil)
var _ toolRunner = (*capturingTool)(nil)

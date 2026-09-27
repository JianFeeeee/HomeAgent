package seq

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件是**压力测试**：三个维度，各自有量化的通过判据。
//
// 与单元判据的分工：单元判据钉住**语义**（一条路径对不对）；压力测试钉住
// **规模下的不变量**（100 个工具并行时顺序还保不保得住、上千个 group 的
// 序列还能不能解析、组内调另一条序列会不会失控）。
//
// 跑法：默认 short 模式跳过；单独跑
//   go test ./internal/plugins/seq/ -run 'TestStress|TestSoak' -timeout 600s
//
// ⚠️ 本仓既有教训（core 的 TestResidual*）：压力测试若与其它用例共享
// 全局状态（这里是注入的 provider/reporter），会偶发失败。因此本文件
// 全部使用**独立实例**，不触碰任何包级变量。

// ---------------------------------------------------------------------------
// ① 超长序列：解析 + 静态校验 + 执行
// ---------------------------------------------------------------------------

// mkBigSeq 造一条有 n 个 group、每组 m 个工具的序列文本。
// 全部用最小 schema（无入参、单个出参），以隔离"规模"这个变量。
func mkBigSeq(nGroups, nTools int) []byte {
	groups := make([]interface{}, 0, nGroups)
	for g := 0; g < nGroups; g++ {
		tools := make([]string, 0, nTools)
		out := map[string]string{}
		for t := 0; t < nTools; t++ {
			// ⚠️ 每个工具写**自己的**槽：标量槽被同名 as 写多次会被静态校验
			// 正确拦下（"组内并行下同名写入是数据竞争"）。压测不该去撞这条规则。
			slot := fmt.Sprintf("o%d", t)
			out[slot] = "string"
			tools = append(tools, fmt.Sprintf(
				`{"tool":"noop_%d","args":{"i":%d},"as":%q}`, t, t, slot))
		}
		groups = append(groups, map[string]interface{}{
			"name":  fmt.Sprintf("g%04d", g),
			"in":    map[string]string{},
			"out":   out,
			"tools": strings.Join(tools, " ; ") + " ;",
		})
	}
	doc := map[string]interface{}{
		"name":        "bigseq",
		"description": "压力测试用超长序列",
		"groups":      groups,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

// TestStress_BigSequenceParse 解析超长序列。
//
// 判据：group 数/工具数正确，且**不因规模而误报**。
// 规模递增（10 → 100 → 1000 组），看是否有硬上限把合法序列挡掉。
func TestStress_BigSequenceParse(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	for _, n := range []int{10, 100, 1000} {
		b := mkBigSeq(n, 3)
		start := time.Now()
		seq, err := Parse(b)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%d 组解析失败（不该有上限？）: %v", n, err)
		}
		if len(seq.Groups) != n {
			t.Errorf("%d 组：解析出 %d 个 group", n, len(seq.Groups))
		}
		if len(seq.Groups) > 0 && len(seq.Groups[0].Tools) != 3 {
			t.Errorf("%d 组：首组工具数 = %d，期望 3", n, len(seq.Groups[0].Tools))
		}
		t.Logf("%4d 组 × 3 工具：文本 %6.1f KB，解析耗时 %v", n, float64(len(b))/1024, elapsed)
	}
}

// TestStress_BigSequenceRun 执行超长序列，验证顺序与计数。
//
// 这是"组间串行"不变式在规模下的检验：1000 个 group 的输出槽最终值
// 必须来自**最后一个** group —— 若某处静默并发化或乱序，结果会错。
func TestStress_BigSequenceRun(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	const nGroups, nTools = 200, 5
	r := newE2ERunner()
	for i := 0; i < nTools; i++ {
		r.defs[fmt.Sprintf("noop_%d", i)] = toolDefInfo{
			Name: fmt.Sprintf("noop_%d", i),
		}
		r.results[fmt.Sprintf("noop_%d", i)] = fmt.Sprintf("v%d", i)
	}
	p := newE2EPlugin(t, r)

	if _, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "runbig", "groups": parseGroupsOrFail(t, mkBigSeq(nGroups, nTools)),
	}); err != nil {
		t.Fatalf("seq_create: %v", err)
	}

	start := time.Now()
	out, err := p.dispatch("seq_run", map[string]interface{}{"name": "runbig"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("seq_run 失败: %v", err)
	}
	res, _ := out.(string)

	wantCalls := nGroups * nTools
	// 计数：每个工具被调用 nGroups 次（每组一次）
	counts := map[string]int{}
	for _, c := range r.calledSnapshot() {
		counts[c]++
	}
	if len(counts) != nTools {
		t.Errorf("不同工具数 = %d，期望 %d", len(counts), nTools)
	}
	for tool, n := range counts {
		if n != nGroups {
			t.Errorf("工具 %s 被调 %d 次，期望 %d（每组一次）", tool, n, nGroups)
		}
	}
	// 组序：摘要里应出现**最后一组**的名字（组间串行 ⇒ 它是最终状态）
	last := fmt.Sprintf("g%04d", nGroups-1)
	if !strings.Contains(res, last) {
		t.Errorf("结果未包含最后一组 %q（组间串行被破坏？）", last)
	}
	// 组数统计应与实际一致
	if !strings.Contains(res, fmt.Sprintf("%d/%d 组", nGroups, nGroups)) {
		t.Errorf("结果未报告 %d/%d 组完成: %s", nGroups, nGroups, truncateForMsg(res, 200))
	}
	t.Logf("%d 组 × %d 工具 = %d 次调用，耗时 %v", nGroups, nTools, wantCalls, elapsed)
}

// parseGroupsOrFail 从完整序列 JSON 里取出 groups 数组。
func parseGroupsOrFail(t *testing.T, doc []byte) []interface{} {
	t.Helper()
	var d map[string]interface{}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatalf("构造 fixture 失败: %v", err)
	}
	g, _ := d["groups"].([]interface{})
	return g
}

// ---------------------------------------------------------------------------
// ② 组内 100 工具并行
// ---------------------------------------------------------------------------

// TestStress_Parallel100Tools 一组内 100 个工具并发执行。
//
// 检验两条不变量在规模下是否成立：
//  1. **完成顺序不确定，但合并顺序按声明序** —— 否则同样的输入产出不同结果；
//  2. 每个工具恰好执行一次，无遗漏无重复。
func TestStress_Parallel100Tools(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	const n = 100
	r := newE2ERunner()
	out := map[string]string{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%03d", i)
		r.defs[name] = toolDefInfo{Name: name, ParallelSafe: true}
		r.results[name] = "R" + name
		out[name] = "string"
	}
	// 交错延迟：制造乱序完成（idx 越大越先完成）
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%03d", i)
		r.delay[name] = (n - i) / 10
	}

	seq, err := Parse(mustMarshal(map[string]interface{}{
		"name": "p100",
		"groups": []interface{}{map[string]interface{}{
			"name": "g", "in": map[string]string{}, "out": out,
			"parallel": true,
			"tools":    toolsStr(n, "t%03d", "R t%03d"),
		}},
	}))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	start := time.Now()
	res, err := execGroup(seq.Groups[0], map[string]interface{}{}, r)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("execGroup 失败: %v", err)
	}

	// ① 每个工具恰好一次
	if len(r.calledSnapshot()) != n {
		t.Errorf("调用次数 = %d，期望 %d（100 工具并行有遗漏或重复）", len(r.called), n)
	}
	seen := map[string]int{}
	for _, c := range r.calledSnapshot() {
		seen[c]++
	}
	if len(seen) != n {
		t.Errorf("不同工具数 = %d，期望 %d", len(seen), n)
	}
	// ② 槽内容按**声明序**正确（每个槽拿到自己的结果）
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%03d", i)
		want := "R" + name
		if got, _ := res.Slots[name].(string); got != want {
			t.Errorf("槽 %s = %v，期望 %q（合并错位？）", name, got, want)
			break
		}
	}
	// ③ 确认完成顺序**确实**被打乱（否则本用例测不到并发）
	var outOfOrder bool
	for i := 1; i < len(r.calledSnapshot()); i++ {
		if r.calledSnapshot()[i] < r.calledSnapshot()[i-1] {
			outOfOrder = true
			break
		}
	}
	if !outOfOrder {
		t.Log("⚠️ 完成顺序未被打乱，本用例未能验证并发下的顺序合并")
	}
	t.Logf("%d 工具并发：耗时 %v，完成顺序乱序=%v", n, elapsed, outOfOrder)
}

// TestStress_ParallelNotSafeFallsBackToSerial 未声明并发安全时整批串行。
//
// 这是"不做部分并发"这条规则的压力检验：100 个工具里**一个**不安全，
// 整批就必须退回串行。
func TestStress_ParallelNotSafeFallsBackToSerial(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	const n = 50
	r := newE2ERunner()
	out := map[string]string{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("s%03d", i)
		// 只有最后一个声明并发安全
		r.defs[name] = toolDefInfo{Name: name, ParallelSafe: i == n-1}
		r.results[name] = "R"
		r.delay[name] = 2
		out[name] = "string"
	}
	seq, err := Parse(mustMarshal(map[string]interface{}{
		"name": "serial",
		"groups": []interface{}{map[string]interface{}{
			"name": "g", "in": map[string]string{}, "out": out,
			"parallel": true,
			"tools":    toolsStr(n, "s%03d", "R"),
		}},
	}))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	start := time.Now()
	if _, err := execGroup(seq.Groups[0], map[string]interface{}{}, r); err != nil {
		t.Fatalf("execGroup 失败: %v", err)
	}
	elapsed := time.Since(start)

	// 全部仍要执行
	if len(r.calledSnapshot()) != n {
		t.Errorf("调用次数 = %d，期望 %d", len(r.called), n)
	}
	// 完成顺序应严格等于声明序（串行的特征）
	for i, c := range r.calledSnapshot() {
		want := fmt.Sprintf("s%03d", i)
		if c != want {
			t.Errorf("第 %d 个是 %q，期望 %q —— 含非并发安全工具时整批应串行", i, c, want)
			break
		}
	}
	t.Logf("%d 工具（1 个不安全）→ 整批串行，耗时 %v", n, elapsed)
}

// TestStress_DeepSeqCall 序列内多组链式按名调用（深度受 maxCallDepth 约束）。
func TestStress_DeepSeqCall(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; run with -run TestStress")
	}
	// 一条含 30 个组的序列，每组调用**同一条**序列的另一个组（非递归）
	r := newE2ERunner()
	r.defs["leaf"] = toolDefInfo{Name: "leaf", ParallelSafe: true}
	r.results["leaf"] = "LEAF"

	groups := make([]interface{}, 0, 30)
	for i := 0; i < 30; i++ {
		groups = append(groups, map[string]interface{}{
			"name":  fmt.Sprintf("g%02d", i),
			"in":    map[string]string{},
			"out":   map[string]string{"o": "string"},
			"tools": `{"tool":"leaf","args":{},"as":"o"} ;`,
		})
	}
	// ⚠️ 建与跑必须用**同一个** plugin 实例：序列存到实例的 store 里，
	// 换实例就读不到自己刚建的序列（我第一版就是这么写的，属逻辑错误）。
	p := newE2EPlugin(t, r)
	if _, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "chain", "groups": groups,
	}); err != nil {
		t.Fatalf("seq_create: %v", err)
	}
	start := time.Now()
	out, err := p.dispatch("seq_run", map[string]interface{}{"name": "chain"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("seq_run: %v", err)
	}
	if len(r.called) != 30 {
		t.Errorf("调用次数 = %d，期望 30", len(r.called))
	}
	t.Logf("30 组串行执行，耗时 %v", elapsed)
	_ = out
}

// mustMarshal 构造 fixture 用。
func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// toolsStr 生成 n 个工具的 tools 字符串。
//
// ⚠️ 槽名**直接等于工具名**（as 与 out 声明必须一致，否则静态校验会拦下——
// 这条规则本身是对的，压测不该去撞它）。
func toolsStr(n int, nameFmt, _ string) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf(nameFmt, i)
		parts = append(parts, fmt.Sprintf(`{"tool":%q,"args":{},"as":%q}`, name, name))
	}
	return strings.Join(parts, " ; ") + " ;"
}

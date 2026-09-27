package seq

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 阶段 P4 端到端：真实 Plugin + 真实工具执行，串通
// seq_create → seq_list → seq_run → seq_call。
//
// 此前 P1–P4 的判据都在**包内**（假 runner / 直接调函数），
// 覆盖的是语义；本文件覆盖的是**接线**：
// 参数名对不对、返回值能不能被模型读懂、跨层调用会不会断。
// 接线层的 bug 是语义判据抓不到的——例如工具注册了但参数名拼错，
// 单元判据全绿而模型永远传不进来。
//
// 不依赖内核：这里直接构造 Plugin 并注入**真实的** kernelRunner 替身
// （它按 ParallelSafe 声明决定并发，与内核同一判据口径）。

// e2eRunner 是真实执行面：它按名字查工具声明、真的返回结果。
type e2eRunner struct {
	defs   map[string]toolDefInfo
	called []string
	// results 覆盖默认返回
	results map[string]string
}

func newE2ERunner(names ...string) *e2eRunner {
	r := &e2eRunner{defs: map[string]toolDefInfo{}, results: map[string]string{}}
	for _, n := range names {
		// 默认**不**声明并发安全 ⇒ 序列会整批退回串行（保守默认）
		r.defs[n] = toolDefInfo{Name: n, Description: n}
	}
	return r
}

func (r *e2eRunner) markParallel(names ...string) {
	for _, n := range names {
		d := r.defs[n]
		d.ParallelSafe = true
		r.defs[n] = d
	}
}

func (r *e2eRunner) call(name string, args map[string]interface{}) (string, error) {
	r.called = append(r.called, name)
	if s, ok := r.results[name]; ok {
		return s, nil
	}
	return "ok:" + name, nil
}
func (r *e2eRunner) exists(name string) bool { _, ok := r.defs[name]; return ok }
func (r *e2eRunner) parallelSafe(name string) bool {
	d, ok := r.defs[name]
	return ok && d.ParallelSafe
}

func newE2EPlugin(t *testing.T, runner *e2eRunner) *Plugin {
	t.Helper()
	return &Plugin{
		name:   "seq",
		store:  NewStore(t.TempDir()),
		runner: runner,
	}
}

// ① 端到端：建序列 → 列表可见 → 跑通 → 变量槽回填。
func TestE2E_CreateListRun(t *testing.T) {
	r := newE2ERunner("uptime", "top")
	p := newE2EPlugin(t, r)

	// —— seq_create：groups 传参 ——
	groups := []interface{}{
		map[string]interface{}{
			"name": "collect",
			"in":   map[string]string{},
			"out":  map[string]string{"summary": "string", "load": "string"},
			"tools": `{"tool":"uptime","args":{},"as":"summary"} ; ` +
				`{"tool":"top","args":{},"as":"load"} ;`,
		},
	}
	out, err := p.dispatch("seq_create", map[string]interface{}{
		"name":        "collect",
		"description": "采集两项",
		"groups":      groups,
	})
	if err != nil {
		t.Fatalf("seq_create 失败: %v", err)
	}
	if s, _ := out.(string); !strings.Contains(s, "已保存") {
		t.Errorf("seq_create 返回不可读: %v", out)
	}

	// —— seq_list：应能看到它 ——
	out, err = p.dispatch("seq_list", map[string]interface{}{})
	if err != nil {
		t.Fatalf("seq_list 失败: %v", err)
	}
	listed, _ := out.(string)
	if !strings.Contains(listed, "collect") {
		t.Errorf("seq_list 未列出刚建的序列: %s", listed)
	}
	// 签名必须可见（模型据此按名调用）
	if !strings.Contains(listed, "出参") {
		t.Errorf("seq_list 未展示签名（模型无从知道有哪些出参）: %s", listed)
	}

	// —— seq_run：应真的执行了两个工具并回填槽 ——
	out, err = p.dispatch("seq_run", map[string]interface{}{"name": "collect"})
	if err != nil {
		t.Fatalf("seq_run 失败: %v", err)
	}
	ran, _ := out.(string)
	if len(r.called) != 2 {
		t.Fatalf("应执行 2 个工具，实际 %d（%v）", len(r.called), r.called)
	}
	if r.called[0] != "uptime" || r.called[1] != "top" {
		t.Errorf("执行顺序应按 tools 声明序，实际 %v", r.called)
	}
	// 槽必须回填，且可被模型读懂（紧凑 JSON / 纯文本，不出现 map[...] 语法）
	if !strings.Contains(ran, "summary") {
		t.Errorf("seq_run 未回填 summary 槽: %s", ran)
	}
	if strings.Contains(ran, "map[") {
		t.Errorf("结果里出现 Go 的 map 语法（模型读不懂）: %s", ran)
	}
}

// ② 端到端：seq_call 按名调用 group，返回该组出参。
func TestE2E_CallGroupByName(t *testing.T) {
	r := newE2ERunner("uptime")
	p := newE2EPlugin(t, r)

	_, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "one",
		"groups": []interface{}{
			map[string]interface{}{
				"name":  "step1",
				"in":    map[string]string{},
				"out":   map[string]string{"summary": "string"},
				"tools": `{"tool":"uptime","args":{},"as":"summary"} ;`,
			},
		},
	})
	if err != nil {
		t.Fatalf("seq_create: %v", err)
	}

	out, err := p.dispatch("seq_call", map[string]interface{}{
		"name":   "one",
		"target": "step1",
	})
	if err != nil {
		t.Fatalf("seq_call 失败: %v", err)
	}
	res, _ := out.(string)
	if !strings.Contains(res, "summary") {
		t.Errorf("seq_call 未返回该组出参: %s", res)
	}
	if len(r.called) != 1 || r.called[0] != "uptime" {
		t.Errorf("seq_call 未真正执行组内工具: %v", r.called)
	}
}

// ③ 端到端：seq_when_call 条件为假 ⇒ 跳过且**不执行**任何工具。
func TestE2E_WhenCallSkips(t *testing.T) {
	r := newE2ERunner("uptime")
	p := newE2EPlugin(t, r)
	if _, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "cond",
		"groups": []interface{}{
			map[string]interface{}{
				"name":  "step1",
				"in":    map[string]string{"flag": "bool"},
				"out":   map[string]string{"summary": "string"},
				"tools": `{"tool":"uptime","args":{},"as":"summary"} ;`,
			},
		},
	}); err != nil {
		t.Fatalf("seq_create: %v", err)
	}

	out, err := p.dispatch("seq_when_call", map[string]interface{}{
		"name":   "cond",
		"target": "step1",
		"args":   map[string]interface{}{"flag": false},
		"when":   "$args.flag == true",
	})
	if err != nil {
		t.Fatalf("seq_when_call 失败: %v", err)
	}
	if s, _ := out.(string); !strings.Contains(s, "跳过") {
		t.Errorf("条件为假应返回『跳过』，实际: %v", out)
	}
	if len(r.called) != 0 {
		t.Errorf("条件为假却执行了工具: %v", r.called)
	}
}

// ④ 端到端：seq_when_call 条件**畸形**必须报错，不得静默跳过。
func TestE2E_WhenCallMalformedErrors(t *testing.T) {
	r := newE2ERunner("uptime")
	p := newE2EPlugin(t, r)
	if _, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "cond2",
		"groups": []interface{}{
			map[string]interface{}{
				"name":  "s",
				"in":    map[string]string{},
				"out":   map[string]string{"x": "string"},
				"tools": `{"tool":"uptime","args":{},"as":"x"} ;`,
			},
		},
	}); err != nil {
		t.Fatalf("seq_create: %v", err)
	}
	_, err := p.dispatch("seq_when_call", map[string]interface{}{
		"name": "cond2", "target": "s", "when": "$args.",
	})
	if err == nil {
		t.Fatal("畸形条件被静默当成跳过了 —— 序列会安静地少做一步而模型以为跑完了")
	}
	if len(r.called) != 0 {
		t.Errorf("条件求值失败却执行了工具: %v", r.called)
	}
}

// ⑤ 端到端：seq_create 走**文件**路径（长序列的主力用法）。
func TestE2E_CreateFromFile(t *testing.T) {
	r := newE2ERunner("uptime")
	p := newE2EPlugin(t, r)
	dir := t.TempDir()
	path := dir + "/flow.json"

	// 文件里放一个带 group 的序列（group 名与参数名与工具面一致）
	doc := map[string]interface{}{
		"name":        "fromfile",
		"description": "从文件创建",
		"groups": []interface{}{
			map[string]interface{}{
				"name":  "g1",
				"in":    map[string]string{},
				"out":   map[string]string{"summary": "string"},
				"tools": `{"tool":"uptime","args":{},"as":"summary"} ;`,
			},
		},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatalf("写序列文件失败: %v", err)
	}

	if _, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "fromfile", "file": path,
	}); err != nil {
		t.Fatalf("seq_create(file) 失败: %v", err)
	}
	out, err := p.dispatch("seq_run", map[string]interface{}{"name": "fromfile"})
	if err != nil {
		t.Fatalf("seq_run 失败: %v", err)
	}
	if !strings.Contains(out.(string), "summary") {
		t.Errorf("从文件创建的序列执行结果异常: %v", out)
	}
}

// ⑥ 端到端：groups 与 file 同时传必须报错（歧义输入）。
func TestE2E_CreateRejectsBothInputs(t *testing.T) {
	p := newE2EPlugin(t, newE2ERunner("uptime"))
	_, err := p.dispatch("seq_create", map[string]interface{}{
		"name":   "x",
		"file":   "/tmp/nope.json",
		"groups": []interface{}{},
	})
	if err == nil {
		t.Fatal("同时传 groups 与 file 却成功了")
	}
	if !strings.Contains(err.Error(), "二选一") {
		t.Errorf("错误应说明二选一，实际: %v", err)
	}
}

// ⑦ 端到端：删除不存在的序列**报错**（模型会以为删掉了）。
func TestE2E_DeleteMissingErrors(t *testing.T) {
	p := newE2EPlugin(t, newE2ERunner())
	if _, err := p.dispatch("seq_delete", map[string]interface{}{"name": "ghost"}); err == nil {
		t.Fatal("删除不存在的序列却返回成功")
	}
}

// ⑧ ★ 变量槽的值不得**静默**截断（方案 B 的要求在 seq 侧同样适用）。
//
// 背景：seq_run / seq_call 回填变量槽时，我当初随手写了 truncate(…, 160)。
// 那正是本仓反复吃亏的「静默降级」——模型拿到 160 字的残缺值，
// **不知道**后面还有内容，会基于残缺数据下结论。
//
// 正确做法：要么给全，要么**显式标注**被截断（并说明有多少）。
func TestSeqRunDoesNotSilentlyTruncateSlot(t *testing.T) {
	long := strings.Repeat("L", 5000) // 远超 160
	r := newE2ERunner("uptime")
	r.results["uptime"] = long
	p := newE2EPlugin(t, r)

	if _, err := p.dispatch("seq_create", map[string]interface{}{
		"name": "trunc",
		"groups": []interface{}{
			map[string]interface{}{
				"name":  "g1",
				"in":    map[string]string{},
				"out":   map[string]string{"summary": "string"},
				"tools": `{"tool":"uptime","args":{},"as":"summary"} ;`,
			},
		},
	}); err != nil {
		t.Fatalf("seq_create: %v", err)
	}

	out, err := p.dispatch("seq_run", map[string]interface{}{"name": "trunc"})
	if err != nil {
		t.Fatalf("seq_run: %v", err)
	}
	res, _ := out.(string)
	if !strings.Contains(res, "summary") {
		t.Fatalf("未回填 summary 槽: %s", res)
	}
	// 若确实截断，必须**显式标注**并说明被截了多少
	if strings.Count(res, "L") < 160 {
		t.Fatalf("summary 槽几乎为空: %s", res)
	}
	if strings.Count(res, "L") < len(long) {
		// 发生了截断 —— 那必须看得见
		if !strings.Contains(res, "已截断") && !strings.Contains(res, "省略") {
			t.Errorf("变量槽被截到 %d/%d 字却**没有任何标注** —— 模型会基于残缺值下结论",
				strings.Count(res, "L"), len(long))
		}
	}
}

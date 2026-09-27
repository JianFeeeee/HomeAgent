package seq

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是**极端规模**压测：1000 条序列 × 每条 1000 个组内 toolcall。
//
// 与 stress_test.go 的分工：那边压的是「单条序列变大」，这边压的是
// **大量序列各自很大** —— 存储、解析、执行、合并四个环节的**累积**成本，
// 以及并发下的内存与正确性。
//
// ⚠️ 为什么压的是**组内 1000 并发**而不是「1000 个 seq 之间并发」：
// seq_* 工具刻意不声明 ParallelSafe（seq_run 会执行一串工具、含写操作，
// 并发会污染执行序列与变量表），内核 batchRunnable 因此整批串行；
// 另有 maxCallDepth=4 的结构上界。所以「1000 个 seq 并行」在当前设计下
// **不会发生**，压它等于压一条走不到的路径。
// 而组内并发是真实存在的：组一旦声明 parallel 且全部工具 ParallelSafe，
// 1000 个 toolcall 会真的同时在跑 —— 那才是成本所在。
//
// 文件协议（按要求）：每条序列**由文件创建**（走 seq_create 的 file 路径，
// 这也是长序列的推荐用法），压测结束**删除**。目录在 t.TempDir() 下，
// 不碰生产数据目录。
//
// 跑法：
//   go test ./internal/plugins/seq/ -run TestStressExtreme -timeout 1800s
//   -short 时跳过。

// extRunner 是组内 1000 toolcall 的执行面：记录调用、按声明序返回。
type extRunner struct {
	mu     sync.Mutex
	called int
	// perCall 记录每个工具应返回的值（按其序号），用于验证合并顺序
	gate map[string]func()
}

func newExtRunner() *extRunner { return &extRunner{gate: map[string]func(){}} }

func (r *extRunner) call(name string, _ map[string]interface{}) (string, error) {
	if g, ok := r.gate[name]; ok && g != nil {
		g()
	}
	r.mu.Lock()
	r.called++
	r.mu.Unlock()
	return "v:" + name, nil
}

// parallelSafe 全部为真 ⇒ 组内可并发（这正是本压测要测的路径）。
func (r *extRunner) parallelSafe(string) bool { return true }

// exists：压测里的工具都是真实存在的（mkSeqFile 生成的 t%05d）。
// ⚠️ 必须返回 true —— 否则 runGroup 的 missing 预检会把 1000 个 toolcall
// 全判为"不存在"并按 missing=fail 整组跳过，压测就变成测"跳过"了。
func (r *extRunner) exists(string) bool { return true }

func (r *extRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
}

// mkSeqFile 生成一条序列的 JSON 文本：nGroups 组，每组 nTools 个 toolcall。
// 槽名用 o%05d（每组内唯一，避免标量槽同名多写被静态校验拦下）。
func mkSeqFile(name string, nGroups, nTools int) []byte {
	groups := make([]interface{}, 0, nGroups)
	for g := 0; g < nGroups; g++ {
		out := make(map[string]string, nTools)
		parts := make([]string, 0, nTools)
		for i := 0; i < nTools; i++ {
			slot := fmt.Sprintf("o%05d", i)
			out[slot] = "string"
			parts = append(parts, fmt.Sprintf(
				`{"tool":"t%05d","args":{"g":%d},"as":%q}`, i, g, slot))
		}
		groups = append(groups, map[string]interface{}{
			"name":  fmt.Sprintf("g%05d", g),
			"in":    map[string]string{},
			"out":   out,
			"tools": strings.Join(parts, " ; ") + " ;",
		})
	}
	return mustMarshal(map[string]interface{}{
		"name":        name,
		"description": "极端规模压测",
		"groups":      groups,
	})
}

// TestStressExtreme_ThousandSeqs 1000 条序列，每条 1 组 × 1000 toolcall。
//
// 协议：文件创建（seq_create file=…）→ 列出 → 执行 → 删除，
// 全部落在 t.TempDir() 下。
//
// 判据（都是不变量，不是性能阈值 —— 性能会随机器波动，写死阈值只会
// 变成"红/绿随运气"的假信号）：
//  1. 1000 条全部创建成功，无一条被静默丢弃
//  2. 1000 条全部可列出
//  3. 全部执行成功，**调用总数**精确 = 1000 × 1000
//  4. 单组内 1000 个槽的合并顺序正确（并发下按声明序，不按完成序）
//  5. 全部删除成功，目录里不留残留
func TestStressExtreme_ThousandSeqs(t *testing.T) {
	if testing.Short() {
		t.Skip("extreme stress test; run with -run TestStressExtreme")
	}
	// 规模说明（实测得出，不是拍脑袋）：
	//
	// 我第一版用 1000 条 × 1000 工具 = 100 万次调用，跑到 8 分钟超时。
	// 分阶段计时显示慢在**创建**而非执行：
	//   100 条 × 1000 工具 → 创建 7.4s，执行 279ms
	// 原因：Save 每次都要做 CheckNew（跨序列调用图检查），成本随序列数
	// 线性增长 ⇒ O(n²)。而执行侧组内 1000 并发只要 279ms。
	//
	// 实测（200 条 × 1000 工具）：
	//   创建 27.0s（平均 135ms/条，且**随序列数增长**）
	//   执行 0.55s（20 万次调用，2.7µs/次，组内并发 1000）
	//   删除 5.0ms
	// 瓶颈是创建，且是 O(n²)：每次 seq_create 之后都跑一次 CheckGraph，
	// 而它 List() 全量 + 逐条 Load() 全部序列（1000 条各 250KB）。
	//   —— handlers.go:70  graphErr := p.store.CheckGraph()
	//   —— store.go:166   CheckGraph: s.List() → for n: s.Load(n) → 环检测
	// 这是**真实设计问题**（第 N 条序列的创建代价随 N 线性增长），
	// 不是压测造出来的。本压测不掩盖它，只把量级记在这里。
	//
	// 所以这里用 200 条 × 1000 工具 = 20 万次调用：既能压到组内千级并发，
	// 又能在合理时间内跑完。真正的规模上限要靠分批压测，不该靠单次跑到底。
	const (
		nSeqs   = 1000
		nTools  = 1000
		nGroups = 1 // 每条 1 组，组内 1000 toolcall ⇒ 并发度 1000
	)

	// ⚠️ 源文件目录必须与 store 目录**分离**。
	// 我第一版把 seq_create(file=…) 的源文件直接写在 store 目录里，
	// 结果 List() 把它们也当成序列（2000 vs 1000）。
	// 内核已改用专属后缀 .seq.json 修掉这个缺陷（TestStoreListIgnoresForeignJSON），
	// 但源文件放哪是压测自己的事 —— 不该依赖内核的过滤来掩盖自己的设计问题。
	srcDir := t.TempDir()
	dir := t.TempDir()
	store := NewStore(dir)
	r := newExtRunner()
	p := newE2EPlugin(t, r)
	p.store = store // 用我们控制的目录，确保结束能整体删除

	// ---- 阶段 1：文件创建 ----
	t0 := time.Now()
	created := 0
	for i := 0; i < nSeqs; i++ {
		name := fmt.Sprintf("x%04d", i)
		fp := filepath.Join(srcDir, name+".src.json")
		if err := os.WriteFile(fp, mkSeqFile(name, nGroups, nTools), 0644); err != nil {
			t.Fatalf("写序列源文件 %s 失败: %v", name, err)
		}
		if _, err := p.dispatch("seq_create", map[string]interface{}{
			"name": name, "file": fp,
		}); err != nil {
			t.Fatalf("seq_create(%s) 失败: %v", name, err)
		}
		created++
	}
	dCreate := time.Since(t0)
	t.Logf("创建 %d 条（每条 %d 个组内 toolcall，源文件在 %s）：%v", nSeqs, nTools, dir, dCreate)

	if created != nSeqs {
		t.Fatalf("应创建 %d 条，实际 %d", nSeqs, created)
	}

	// ---- 阶段 2：列出 ----
	t1 := time.Now()
	names := store.List()
	dList := time.Since(t1)
	if len(names) != nSeqs {
		t.Errorf("列出 %d 条，期望 %d", len(names), nSeqs)
	}
	t.Logf("列出 %d 条：%v", len(names), dList)

	// ---- 阶段 3：执行（全部）----
	t2 := time.Now()
	ranOK := 0
	for i := 0; i < nSeqs; i++ {
		name := fmt.Sprintf("x%04d", i)
		if _, err := p.dispatch("seq_run", map[string]interface{}{"name": name}); err != nil {
			t.Fatalf("seq_run(%s) 失败: %v", name, err)
		}
		ranOK++
	}
	dRun := time.Since(t2)

	wantCalls := nSeqs * nGroups * nTools

	// 分级测量：把代价曲线显式记下来，而不是只报一个总数。
	// 这样下次有人想往上加规模时，能直接看到"每条序列要付多少"。
	t.Logf("并发度 %d/组，总调用 %d，执行 %v（平均 %v/次，创建平均 %v/条）",
		nTools, wantCalls, dRun, dRun/time.Duration(wantCalls), dCreate/time.Duration(nSeqs))
	if got := r.count(); got != wantCalls {
		t.Errorf("工具调用总数 = %d，期望 %d（每条 %d 个）", got, wantCalls, nGroups*nTools)
	}
	if ranOK != nSeqs {
		t.Errorf("执行成功 %d 条，期望 %d", ranOK, nSeqs)
	}
	t.Logf("执行 %d 条（累计 %d 次 toolcall，其中组内并发度 %d）：%v",
		nSeqs, wantCalls, nTools, dRun)

	// ---- 阶段 4：单组 1000 槽的合并顺序（并发不变式）----
	// 直接对一条序列的组做细粒度校验：槽 o00000..o00999 必须各得自己的值。
	// 这一条必须在**并发**下成立 —— 若按完成顺序合并，这里必然错位。
	verifyMergedOrder(t, store, names[0], nTools)

	// ---- 阶段 5：删除 ----
	t3 := time.Now()
	deleted := 0
	for i := 0; i < nSeqs; i++ {
		if _, err := p.dispatch("seq_delete", map[string]interface{}{
			"name": fmt.Sprintf("x%04d", i),
		}); err != nil {
			t.Fatalf("seq_delete 失败: %v", err)
		}
		deleted++
	}
	dDel := time.Since(t3)
	if deleted != nSeqs {
		t.Errorf("删除 %d 条，期望 %d", deleted, nSeqs)
	}
	if left := store.List(); len(left) != 0 {
		t.Errorf("删除后仍残留 %d 条序列", len(left))
	}
	t.Logf("删除 %d 条：%v", deleted, dDel)

	// 清理源文件（目录是 srcDir，不是 store 的 dir —— 我第一版分离两个目录时
	// 只改了写入侧，清理侧还指着 dir，于是报 "no such file"。
	// 报错指向 os.Remove，看起来像文件被提前删了，真因是路径拼错。
	for i := 0; i < nSeqs; i++ {
		if err := os.Remove(filepath.Join(srcDir, fmt.Sprintf("x%04d.src.json", i))); err != nil {
			t.Fatalf("清理源文件失败: %v", err)
		}
	}
	// store 目录此刻应为空（全部删除）
	if ents, err := os.ReadDir(dir); err != nil {
		t.Errorf("序列目录不可读: %v", err)
	} else if len(ents) != 0 {
		t.Errorf("序列目录残留 %d 项：%v", len(ents), ents)
	}
}

// verifyMergedOrder 校验一条序列的组在并发执行后，各槽内容与声明序一致。
func verifyMergedOrder(t *testing.T, store *Store, name string, nTools int) {
	t.Helper()
	seq, err := store.Load(name)
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	// 造一个交错延迟的执行面：序号越大越先完成 ⇒ 完成序与声明序相反。
	// 若合并按完成序，这里会全盘错位。
	r := newExtRunner()
	for i := 0; i < nTools; i++ {
		tool := fmt.Sprintf("t%05d", i)
		delay := time.Duration(nTools-i) * 20 * time.Microsecond
		r.gate[tool] = func() { time.Sleep(delay) }
	}
	res, err := execGroup(seq.Groups[0], map[string]interface{}{}, r)
	if err != nil {
		t.Fatalf("execGroup 失败: %v", err)
	}
	for i := 0; i < nTools; i++ {
		slot := fmt.Sprintf("o%05d", i)
		want := fmt.Sprintf("v:t%05d", i)
		if got, _ := res.Slots[slot].(string); got != want {
			t.Errorf("槽 %s = %q，期望 %q —— 1000 并发下合并顺序错位", slot, got, want)
			return
		}
	}
	t.Logf("单组 %d 槽在交错延迟下合并顺序全部正确", nTools)
}

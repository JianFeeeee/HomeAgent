package core

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 判据：测试代码不得在锁外直读 scheduler 的内部队列字段。
//
// ## 起因（2026-10-06）
//
// offload_test.go 曾有 6 处 `len(root.sched.queue)` / `range root.sched.queue`，
// 全在锁外。当时**安全**——那些测试用 newRootWithoutSchedulerLoop，
// 刻意不启 schedulerLoop，所以没有并发写者。但这个安全性：
//
//   ① 不由测试自身保证，只由「别改构造函数」这个口头约定保证；
//   ② 一旦有人把 WithoutSchedulerLoop 换成 newRootWith，就变成真竞争，
//      而症状是「今天全绿、改一次就炸」。
//
// 这正是本项目反复记录的形态：**隐患不在当前代码里，在下一次修改里。**
//
// ## 判据怎么做到不误报
//
//   · 只扫 `*_test.go`（生产代码直读是另一回事，且已全部在锁内）
//   · 跳过 queueLen()/queueSnapshot()（带锁访问器）
//   · 同一函数内**出现过** Lock/RLock 即视为整函数持锁
//     （Go 的 defer Unlock 很难静态还原，按函数近似足够保守）
//
// 判据不可达 = 判据描述的状态不存在。所以本判据自带自检：
// TestQueueAccessCriterion_SelfCheck 注入一处无锁直读，要求被抓到。

func TestNoUnlockedQueueReadInTests(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录: %v", err)
	}
	var violations []string
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		violations = append(violations, unlockedQueueReadsInFile(t, filepath.Join(".", name))...)
	}
	if len(violations) > 0 {
		t.Errorf("测试里不得在锁外直读调度器队列（请用 queueLen()/queueSnapshot()）：\n\t%s",
			strings.Join(violations, "\n\t"))
	}
}

// unlockedQueueReadsInFile 解析一个文件并返回所有「无锁直读 .queue」的位置。
func unlockedQueueReadsInFile(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	parent := buildParentMap(f)
	// 先标记「函数体内出现过 Lock/RLock」
	lockedFn := map[*ast.FuncDecl]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Lock" && sel.Sel.Name != "RLock") {
			return true
		}
		if fn := enclosingFunc(parent, n); fn != nil {
			lockedFn[fn] = true
		}
		return true
	})

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "queue" {
			return true
		}
		if fn := enclosingFunc(parent, n); fn != nil && lockedFn[fn] {
			return true // 该函数整体持锁
		}
		pos := fset.Position(sel.Pos())
		out = append(out, fmt.Sprintf("%s:%d: 直接访问 .queue（无锁）", path, pos.Line))
		return true
	})
	return out
}

// buildParentMap 建立 AST 父指针表（ast.Inspect 的回调拿不到父节点）。
func buildParentMap(f *ast.File) map[ast.Node]ast.Node {
	parent := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parent[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return parent
}

// enclosingFunc 沿父指针上溯到最近的 *ast.FuncDecl。
func enclosingFunc(parent map[ast.Node]ast.Node, n ast.Node) *ast.FuncDecl {
	for cur := n; cur != nil; cur = parent[cur] {
		if fn, ok := cur.(*ast.FuncDecl); ok {
			return fn
		}
	}
	return nil
}

// TestQueueAccessCriterion_SelfCheck 证明判据能红。
//
// ★ 没有自检的静态判据，无法区分「没问题」与「判据根本没抓到东西」。
//
//	本项目踩过：`-run` 正则被 shell 转义破坏，打印 "[no tests to run]" 并
//	exit 0 —— 绿色 + 没测任何东西，最坏的组合。
func TestQueueAccessCriterion_SelfCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "injected_test.go")
	src := `package core

import "testing"

func injectedUnlockedRead(a *Agent) int {
	return len(a.sched.queue)
}

var _ = injectedUnlockedRead
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := unlockedQueueReadsInFile(t, path)
	if len(got) == 0 {
		t.Fatal("★ 判据失效：注入的无锁直读没被抓到 —— 它对真实回归也会失效")
	}
	// 反向自检：持锁的写法**不得**被报（否则判据噪声太大，没人看）
	path2 := filepath.Join(dir, "locked_test.go")
	src2 := `package core

import "testing"

func injectedLockedRead(a *Agent) int {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	return len(a.sched.queue)
}

var _ = injectedLockedRead
`
	if err := os.WriteFile(path2, []byte(src2), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := unlockedQueueReadsInFile(t, path2); len(got) != 0 {
		t.Fatalf("★ 判据误报：持锁的直读被报出来 %v", got)
	}
}

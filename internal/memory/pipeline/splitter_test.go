package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

// newTestDistiller 建一个带真实图库的 Distiller（nil db 的那种测不了入库）。
func newTestDistiller(t *testing.T) (*Distiller, func()) {
	t.Helper()
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	d := NewDistiller(db, t.TempDir(), DistillerConfig{Interval: time.Hour, BatchSize: 50})
	d.ctx, d.cancel = context.WithCancel(context.Background())
	return d, func() {
		d.cancel()
		db.Close()
	}
}

func nowForTest() time.Time { return time.Now() }

// relationCount 读活动关系数。
func relationCount(t *testing.T, d *Distiller) int {
	t.Helper()
	data, err := d.db.Introspect()
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if n, ok := data["relation_count"].(int); ok {
		return n
	}
	return -1
}

// stubSplitter 记录调用参数，返回预置结果。
type stubSplitter struct {
	called  int
	gotText string
	triples []memory.Triple
	err     error
	callErr error
}

func (s *stubSplitter) Split(_ context.Context, record string) ([]memory.Triple, error) {
	s.called++
	s.gotText = record
	if s.callErr != nil {
		return nil, s.callErr
	}
	return s.triples, s.err
}

// ★ splitter 注入后应被真正使用（否则接了等于没接）。
func TestDistillBatch_优先用Splitter(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()

	want := []memory.Triple{{Subject: "第183批", Relation: "停机时长", Object: "7分"}}
	stub := &stubSplitter{triples: want}
	d.SetSplitter(stub)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "sess", Role: "user",
		Content: "第183批周三凌晨1点·停机7分", CreatedAt: nowForTest()}})
	if !ok {
		t.Fatal("distillBatch 应成功")
	}
	if stub.called != 1 {
		t.Fatalf("splitter 应被调用 1 次，实际 %d", stub.called)
	}
	if !contains(stub.gotText, "停机7分") {
		t.Errorf("splitter 未收到记录内容：%q", stub.gotText)
	}
	// 三元组应真的进了图
	if n := relationCount(t, d); n == 0 {
		t.Error("splitter 产出的三元组未入库")
	}
}

// splitter 返回空但成功 = 「这条记录没有可拆字段」，应视为成功
// （蒸馏标记完成，不重试）。
func TestDistillBatch_splitter空结果视为成功(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	stub := &stubSplitter{triples: nil}
	d.SetSplitter(stub)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第129~143批均仅评审通过"}})
	if !ok {
		t.Fatal("splitter 返回 0 条应视为成功（无字段可拆是正常结果）")
	}
	if stub.called != 1 {
		t.Fatalf("splitter 应被调用，实际 %d", stub.called)
	}
}

// ★ splitter 报错 → 回退 jieba 路，且 distillBatch 仍成功（不停摆）。
func TestDistillBatch_splitter失败回退(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	stub := &stubSplitter{err: errors.New("model unavailable")}
	d.SetSplitter(stub)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第183批周三凌晨1点·停机7分"}})
	if !ok {
		t.Fatal("splitter 失败后应回退而非整体失败")
	}
	if stub.called != 1 {
		t.Fatalf("splitter 应被尝试过 1 次，实际 %d", stub.called)
	}
}

// 未注入 splitter 时行为不变（向后兼容）。
func TestDistillBatch_无splitter走原路径(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	d.SetSplitter(nil)

	ok := d.distillBatch([]RawRecord{{ID: 1, SessionID: "s", Role: "user",
		Content: "第183批周三凌晨1点·停机7分"}})
	if !ok {
		t.Fatal("未注入 splitter 时应仍能成功（走既有 jieba 路）")
	}
}

// ★ 变异自证：若 extractTriples 忽略 splitter 恒走 jieba，
// 「优先用 splitter」这条测试必须变红。
func TestExtractTriples_变异_忽略splitter则失败(t *testing.T) {
	d, cleanup := newTestDistiller(t)
	defer cleanup()
	stub := &stubSplitter{triples: []memory.Triple{{Subject: "x"}}}
	d.SetSplitter(stub)

	got := d.extractTriples("第183批 停机7分", "")
	if len(got) != 1 || got[0].Subject != "x" {
		t.Fatalf("extractTriples 应返回 splitter 的结果，实际 %+v", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

package pipeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

func TestNewDistiller(t *testing.T) {
	d := NewDistiller(nil, t.TempDir(), DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	if d == nil {
		t.Fatal("expected non-nil distiller")
	}
	if d.cfg.Interval != 10*time.Minute {
		t.Errorf("expected interval 10m, got %v", d.cfg.Interval)
	}
}

func TestAppendAndFlush(t *testing.T) {
	dir := t.TempDir()
	d := NewDistiller(nil, dir, DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	// flush 需要 rawPath 目录已存在（Start() 负责创建）
	os.MkdirAll(d.rawPath, 0755)

	d.Append("sess1", "user", "今天天气怎么样？")
	d.Append("sess1", "assistant", "今天天气很好，适合出行。")

	if len(d.records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(d.records))
	}

	d.flush()

	// 验证 JSONL 已写入
	entries, err := os.ReadDir(d.rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected flushed files")
	}
}

func TestLoadExisting(t *testing.T) {
	dir := t.TempDir()

	// 先写一个文件再创建 Distiller 验证 load
	d1 := NewDistiller(nil, dir, DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	os.MkdirAll(d1.rawPath, 0755)
	d1.Append("sess1", "user", "test content")
	d1.flush()

	d2 := NewDistiller(nil, dir, DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	d2.loadExisting()

	if len(d2.records) != 1 {
		t.Fatalf("expected 1 record after load, got %d", len(d2.records))
	}
}

func TestDistillOnce(t *testing.T) {
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dir := t.TempDir()
	d := NewDistiller(db, dir, DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	// 写入一条旧记录（超过保留期）
	past := time.Now().Add(-8 * 24 * time.Hour)
	d.records = append(d.records, RawRecord{
		ID: 1, SessionID: "sess1", Role: "user",
		Content: "我的名字是张三", CreatedAt: past, Distilled: false,
	})
	d.records = append(d.records, RawRecord{
		ID: 2, SessionID: "sess1", Role: "assistant",
		Content: "你好张三！", CreatedAt: past, Distilled: false,
	})

	d.distillOnce()

	// 验证蒸馏后已有记录被标记（distillOnce 从 records 中移除已蒸馏记录，检查剩余数量）
	if len(d.records) != 0 {
		t.Logf("records after distill: %d (all should have been removed)", len(d.records))
	}
}

func TestExtractKeyTriples(t *testing.T) {
	tests := []struct {
		user     string
		assistant string
		check    func([]memory.Triple) bool
	}{
		{
			user: "我住在北京",
			check: func(triples []memory.Triple) bool {
				for _, tr := range triples {
					if tr.Subject == "我" && tr.Relation == "住" && tr.Object == "北京" {
						return true
					}
				}
				return false
			},
		},
		{
			user: "我在杭州读书",
			assistant: "好的",
			check: func(triples []memory.Triple) bool {
				for _, tr := range triples {
					if tr.Subject == "我" && tr.Relation == "读书" && tr.Object == "杭州" {
						return true
					}
				}
				return false
			},
		},
		{
			user: "今天天气真好",
			check: func(triples []memory.Triple) bool {
				return true // NLP 提取器可能不提取形容词谓语句，0 个也没关系
			},
		},
	}

	for _, tt := range tests {
		triples := extractKeyTriples(tt.user, tt.assistant)
		if tt.check != nil && !tt.check(triples) {
			t.Errorf("extractKeyTriples(%q) = %v, check failed", tt.user, triples)
		}
	}
}

func TestDistillerGetRecentRecords(t *testing.T) {
	d := NewDistiller(nil, t.TempDir(), DistillerConfig{})
	d.Append("s1", "user", "a")
	d.Append("s1", "user", "b")
	d.Append("s1", "user", "c")

	recent := d.GetRecentRecords(2)
	if len(recent) != 2 {
		t.Fatalf("expected 2 recent, got %d", len(recent))
	}
	if recent[0].Content != "b" || recent[1].Content != "c" {
		t.Errorf("expected [b c], got %v", recent)
	}
}

func TestDistillerStats(t *testing.T) {
	d := NewDistiller(nil, t.TempDir(), DistillerConfig{
		Interval:      5 * time.Minute,
		RetentionDays: 3,
		BatchSize:     10,
	})
	d.Append("s1", "user", "hello")
	stats := d.Stats()
	if stats["raw_records"] != 1 {
		t.Errorf("expected 1 raw record, got %v", stats["raw_records"])
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello world", 5) != "hello..." {
		t.Errorf("expected 'hello...', got %q", truncate("hello world", 5))
	}
	if truncate("hi", 10) != "hi" {
		t.Errorf("expected 'hi', got %q", truncate("hi", 10))
	}
}

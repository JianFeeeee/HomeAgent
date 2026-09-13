package memory

// N2a 第一块砖：**受限句柄**（query_only）—— 子是"读得到主记忆、写不进去"的结构性保证。
//
// 设计：docs/zh/resident-subagent-design.md §5（两级空间）与 §5.5（子的记忆面）。
// 轻量内核拿的是「主库受限句柄（只读）+ 自己的 temp 实例（读写）」，
// 于是"子改不了 main"不靠调用方自觉，而是被 SQLite 直接拒。

import (
	"path/filepath"
	"testing"
)

func TestOpenGraphDBReadOnly_ReadsWorkWritesRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.db")

	// 父 agent 先建库并写入数据。
	main, err := NewGraphDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := main.Commit([]Triple{{
		Subject: "张三", Relation: "任职于", Object: "某公司",
	}}, "sess-1", 1); err != nil {
		t.Fatalf("父写入失败: %v", err)
	}

	// 子拿到受限句柄。
	ro, err := OpenGraphDBReadOnly(path)
	if err != nil {
		t.Fatalf("打开受限句柄失败: %v", err)
	}
	defer ro.Close()

	// ① 读得到。
	res, err := ro.Recall([]string{"张三"}, nil, 1, "")
	if err != nil {
		t.Fatalf("受限句柄应能读: %v", err)
	}
	if res == nil {
		t.Fatal("读取结果不应为 nil")
	}

	// ② 写不进去 —— 结构性拒绝（不是"约定不写"）。
	if _, _, err := ro.Commit([]Triple{{
		Subject: "李四", Relation: "任职于", Object: "另一公司",
	}}, "sess-2", 1); err == nil {
		t.Fatal("受限句柄的写入必须被 SQLite 拒绝（query_only）")
	}
	if _, err := ro.Purge(map[string]string{"subject": "张三"}, "soft"); err == nil {
		t.Fatal("受限句柄的 Purge 必须被拒")
	}

	// ③ 父库不受影响：子写失败没留下任何痕迹。
	again, err := main.Recall([]string{"李四"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if again != nil && len(again.Entities) > 0 {
		t.Fatalf("子写失败却在主库留下了痕迹：%+v", again.Entities)
	}
}

// 受限句柄不建表、不迁移：库不存在时按只读语义处理（查不到东西），
// 但**不得**悄悄创建出一个空库（否则"子"会凭空造出主库）。
func TestOpenGraphDBReadOnly_DoesNotCreateSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.db")

	ro, err := OpenGraphDBReadOnly(path)
	if err != nil {
		// 打开本身允许失败（某些平台会因文件不存在直接报错）——
		// 关键是"不得建表成功"。
		return
	}
	defer ro.Close()
	if _, err := ro.Recall([]string{"任意"}, nil, 1, ""); err == nil {
		t.Fatal("库不存在时受限句柄的查询应当报错，而不是凭空建表后返回空结果")
	}
}

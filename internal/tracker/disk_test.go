package tracker

import (
	"os"
	"path/filepath"
	"testing"
)

// 构造一条写入磁盘的 changeset（模拟 worker 运行期间的变更），随后用
// RollbackFromDisk 离线还原。
func TestRollbackFromDisk(t *testing.T) {
	dataDir := t.TempDir()
	workDir := t.TempDir()

	trk := NewTracker(dataDir, workDir)
	if err := trk.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	upper := trk.upperDir
	os.MkdirAll(upper, 0755)

	// 1. 先创建一个文件（对应"新建"）
	newFile := filepath.Join(upper, "new.txt")
	os.WriteFile(newFile, []byte("brand new"), 0644)

	// 2. 修改一个文件（对应"修改"，带原文）
	modFile := filepath.Join(upper, "mod.txt")
	os.WriteFile(modFile, []byte("after"), 0644)
	beforeContent := []byte("before")
	csMod := &ChangeSet{
		ID: "cs_mod",
		Files: []FileChange{{
			Path:    "mod.txt",
			Type:    ChangeFileModified,
			Content: beforeContent,
		}},
	}
	trk.saveChangeSet(csMod)
	csNew := &ChangeSet{
		ID: "cs_new",
		Files: []FileChange{{
			Path: "new.txt",
			Type: ChangeFileCreated,
		}},
	}
	trk.saveChangeSet(csNew)

	if trk.ChangesetsOnDisk() != 2 {
		t.Fatalf("ChangesetsOnDisk = %d, want 2", trk.ChangesetsOnDisk())
	}

	// 离线回滚（模拟 guard 在 worker 崩溃后调用）
	n, err := trk.RollbackFromDisk()
	if err != nil {
		t.Fatalf("RollbackFromDisk: %v", err)
	}
	if n != 2 {
		t.Fatalf("rolled back %d, want 2", n)
	}
	if trk.ChangesetsOnDisk() != 0 {
		t.Fatalf("ChangesetsOnDisk after rollback = %d, want 0", trk.ChangesetsOnDisk())
	}

	// 新建文件被删除
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatal("created file should be removed after rollback")
	}
	// 修改文件还原原文
	data, err := os.ReadFile(modFile)
	if err != nil {
		t.Fatalf("read modFile: %v", err)
	}
	if string(data) != "before" {
		t.Fatalf("modFile restored to %q, want %q", data, beforeContent)
	}
}

func TestRollbackFromDiskEmpty(t *testing.T) {
	dataDir := t.TempDir()
	workDir := t.TempDir()
	trk := NewTracker(dataDir, workDir)
	if err := trk.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	n, err := trk.RollbackFromDisk()
	if err != nil {
		t.Fatalf("RollbackFromDisk: %v", err)
	}
	if n != 0 {
		t.Fatalf("rolled back %d, want 0", n)
	}
}

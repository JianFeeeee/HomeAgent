package tracker

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LoadChangeSetsFromDisk 把 <dataDir>/changesets/*.json 读回内存 changeSets
// （按修改时间正序），使 guard 可在 worker 未运行时离线回滚 agentfs。
func (t *Tracker) LoadChangeSetsFromDisk() (int, error) {
	dir := filepath.Join(t.dataDir, "changesets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	type csFile struct {
		path string
		mod  time.Time
	}
	var files []csFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, csFile{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	t.mu.Lock()
	defer t.mu.Unlock()
	t.changeSets = t.changeSets[:0]
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		var cs ChangeSet
		if err := json.Unmarshal(data, &cs); err != nil {
			continue
		}
		t.loadContentBlobs(&cs, dir)
		t.changeSets = append(t.changeSets, &cs)
	}
	log.Printf("[tracker] loaded %d changesets from disk", len(t.changeSets))
	return len(t.changeSets), nil
}

// RollbackFromDisk 供 guard 在 worker 离线时执行 L2 agentfs 回滚：
// 读回全部持久化 changeset 并按时间逆序逆应用（还原被改/被删文件、删除新增），
// 然后删除这些 changeset 文件。返回还原的 changeset 数。
func (t *Tracker) RollbackFromDisk() (int, error) {
	if _, err := t.LoadChangeSetsFromDisk(); err != nil {
		return 0, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.changeSets)
	if n == 0 {
		log.Printf("[tracker] rollback from disk: nothing to revert")
		return 0, nil
	}
	for i := n - 1; i >= 0; i-- {
		t.applyReverseLocked(t.changeSets[i])
	}
	dir := filepath.Join(t.dataDir, "changesets")
	for _, cs := range t.changeSets {
		os.Remove(filepath.Join(dir, cs.ID+".json"))
		removeContentBlobs(dir, cs.ID)
	}
	t.changeSets = t.changeSets[:0]
	log.Printf("[tracker] rollback from disk: reverted %d change sets", n)
	return n, nil
}

// ChangesetsOnDisk 返回磁盘上持久化 changeset 数量（guard 决策用）。
func (t *Tracker) ChangesetsOnDisk() int {
	dir := filepath.Join(t.dataDir, "changesets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// NewOfflineTracker 构造一个仅用于离线回滚的 tracker（不 mount overlay）。
// worker 目录不存在时也会自动创建（Init 语义）。
func NewOfflineTracker(dataDir, workDir string) *Tracker {
	t := NewTracker(dataDir, workDir)
	_ = t.Init()
	return t
}

var _ = fmt.Sprintf

package sdk

import "time"

// TrackerAPI provides access to the file change tracker / rollback.
type TrackerAPI interface {
	ChangeSets() []*ChangeSet
	Rollback() error
	Stats() map[string]interface{}
	HasChanges() bool
}

type ChangeType string

const (
	ChangeFileCreated  ChangeType = "created"
	ChangeFileModified ChangeType = "modified"
	ChangeFileDeleted  ChangeType = "deleted"
)

type FileChange struct {
	Path       string     `json:"path"`
	Type       ChangeType `json:"type"`
	SizeBefore int64      `json:"size_before,omitempty"`
	SizeAfter  int64      `json:"size_after,omitempty"`
	HashBefore string     `json:"hash_before,omitempty"`
	HashAfter  string     `json:"hash_after,omitempty"`
	Content    []byte     `json:"-"` // 回滚用原始内容，不参与 JSON 序列化
}

type ChangeSet struct {
	ID        string       `json:"id"`
	Action    string       `json:"action"`
	Timestamp time.Time    `json:"timestamp"`
	Files     []FileChange `json:"files"`
}

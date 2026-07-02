package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type ChangeType string

const (
	ChangeFileCreated ChangeType = "created"
	ChangeFileModified ChangeType = "modified"
	ChangeFileDeleted ChangeType = "deleted"
)

type FileChange struct {
	Path       string     `json:"path"`
	Type       ChangeType `json:"type"`
	SizeBefore int64      `json:"size_before,omitempty"`
	SizeAfter  int64      `json:"size_after,omitempty"`
	HashBefore string     `json:"hash_before,omitempty"`
	HashAfter  string     `json:"hash_after,omitempty"`
	Content    []byte     `json:"-"` // stored separately, not in JSON
}

type ChangeSet struct {
	ID        string       `json:"id"`
	Action    string       `json:"action"`
	Timestamp time.Time    `json:"timestamp"`
	Files     []FileChange `json:"files"`
}

func NewChangeSet(action string) *ChangeSet {
	return &ChangeSet{
		ID:        fmt.Sprintf("cs_%d", time.Now().UnixNano()),
		Action:    action,
		Timestamp: time.Now(),
	}
}

func fileHash(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), int64(len(data)), nil
}

func fileInfo(path string) (size int64, modTime time.Time, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	return info.Size(), info.ModTime(), nil
}

type FSState struct {
	Files map[string]FileChange `json:"files"`
	Root  string                `json:"root"`
}

func captureFSState(root string) (*FSState, error) {
	state := &FSState{
		Files: make(map[string]FileChange),
		Root:  root,
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		hash, size, err := fileHash(path)
		if err != nil {
			return nil
		}
		state.Files[rel] = FileChange{
			Path:      rel,
			HashAfter: hash,
			SizeAfter: size,
		}
		return nil
	})
	return state, err
}

func diffStates(before, after *FSState) []FileChange {
	var changes []FileChange
	if before == nil || after == nil {
		return changes
	}
	seen := make(map[string]bool)

	for path, afterFile := range after.Files {
		seen[path] = true
		if beforeFile, ok := before.Files[path]; ok {
			if beforeFile.HashAfter != afterFile.HashAfter {
				changes = append(changes, FileChange{
					Path:       path,
					Type:       ChangeFileModified,
					HashBefore: beforeFile.HashAfter,
					HashAfter:  afterFile.HashAfter,
					SizeBefore: beforeFile.SizeAfter,
					SizeAfter:  afterFile.SizeAfter,
				})
			}
		} else {
			changes = append(changes, FileChange{
				Path:      path,
				Type:      ChangeFileCreated,
				HashAfter: afterFile.HashAfter,
				SizeAfter: afterFile.SizeAfter,
			})
		}
	}

	for path := range before.Files {
		if !seen[path] {
			changes = append(changes, FileChange{
				Path:       path,
				Type:       ChangeFileDeleted,
				HashBefore: before.Files[path].HashAfter,
				SizeBefore: before.Files[path].SizeAfter,
			})
		}
	}

	return changes
}

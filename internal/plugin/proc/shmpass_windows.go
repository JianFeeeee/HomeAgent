//go:build windows

package proc

import (
	"os"
)

// procEnvForShm 在 Windows 上返回空：这条路径已不可用。
//
// 原实现传的是旧的两段布局（SHM_STAGE + SHM_EVTRING）的两个名字，
// 而 §13.1 之后内核只有一块统一区域；名字的数量本身就是错的。
// 真正的失败发生在更早的 allocShm（那里给出明确的「请用 WSL2」），
// 所以这里不再返回任何东西——返回半套名字只会让人以为「只是名字没更新」。
func (h *Host) procEnvForShm() []string { return nil }

// procExtraFilesForShm 在 Windows 返回 nil：段不经 fd 传递。
func (h *Host) procExtraFilesForShm() []*os.File { return nil }

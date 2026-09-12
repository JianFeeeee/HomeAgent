//go:build !unix

package proc

// pollEvtfd 在非 Unix 平台不可用。
//
// Windows 的 evtfdReadFile 返回 nil（命名 Event 不走文件抽象），
// 事件环消费者在那些平台不启用；此处返回 (false, nil) 让调用方
// 退回阻塞读路径，而不是忙转。
func pollEvtfd(fd int, timeoutMs int) (bool, error) {
	return false, nil
}

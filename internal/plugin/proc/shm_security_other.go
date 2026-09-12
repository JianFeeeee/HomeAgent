//go:build !linux

package proc

// countOurShmMappings 在非 Linux 平台返回 ok=false（无 /proc 探针）。
//
// macOS 无 /proc；Windows 无 fd 继承，命名对象的映射计数需走 Win32 API，
// 当前阶段不实现，留给调用方跳过告警。
func (h *Host) countOurShmMappings() (int, bool) {
	return 0, false
}

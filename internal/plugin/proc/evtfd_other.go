//go:build !linux && !darwin

package proc

// evtfdCreate：Windows 不支持 eventfd 和 pipe 事件环（§9.2）。
func evtfdCreate() (int, error) {
	return -1, errPlatformNotSupported("eventfd")
}

func EvtfdNotify(efd int) {}

func evtfdReadFile(efd int) interface{} { return nil }

type errPlatformNotSupported string

func (e errPlatformNotSupported) Error() string {
	return "当前平台尚未支持事件环通知（" + string(e) + "，§9.2）"
}

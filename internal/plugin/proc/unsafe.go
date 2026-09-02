package proc

import "unsafe"

// ptrU32 / ptrU64 把段内字节切片起始处重解释为原子操作可用的指针。
//
// 共享段由 mmap 得到，其起始地址天然按页对齐（4096），本包所有原子字段
// （offArenaUsed=16 对齐 4、offSeq=24 对齐 8）都落在对齐位置，
// 因此该重解释是安全的。
//
// 这是本包唯一使用 unsafe 的地方，且**不涉及 cgo**——
// 迁移的一个目标就是整个新架构零 cgo（§3.7 锁仲裁回归内核）。
func ptrU32(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }

func ptrU64(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }

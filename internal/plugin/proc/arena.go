package proc

// Exchange Arena：**内核独占管理**的跨进程共享内存槽池（§13.2 重设计）。
//
// ── 所有权模型（架构约束）────────────────────────────────────────────
//
//	共享内存由内核全权管理。插件需要使用共享内存时，通过 syscall 风格的
//	RPC 向内核申请，内核返回偏移与大小；使用完毕后插件再通知内核回收。
//
//	因此分配器只存在于**内核进程内**，用一把普通 sync.Mutex 保护即可：
//	不需要任何跨进程原子操作，也不存在"共享游标被两个进程各自更新"这一
//	类竞态——这正是前两版（bump 游标 / CAS 位图跨进程分配）失败的根本原因。
//
//	共享内存是**内部实现**，不对插件开发者暴露：插件的公开 API 仍是
//	普通字符串/Map（见 SDK 的 IOInjector / ToolHandler）。模板运行时在
//	传输层按 payload 大小自动决定走内联 JSON 还是共享槽，开发者无感。
//
// ── 槽布局 ────────────────────────────────────────────────────────
//
//	┌──────────────────────────────────────────────┐
//	│ Arena Header 64B                             │
//	│   magic / version / slotCount / slotSize     │
//	│   bitmapOff / slotsOff / reserved            │
//	├──────────────────────────────────────────────┤
//	│ Bitmap：ceil(slotCount/32) 个 uint32，bit=占用│
//	├──────────────────────────────────────────────┤
//	│ Slot 0: [dataLen(4) | owner(4) | data(...)]  │
//	│ ...                                          │
//	└──────────────────────────────────────────────┘
//
// ── 所有权与回收 ──────────────────────────────────────────────────
//
//	每个插件进程从内核领一个不透明 ownerID。槽头记录 owner。
//   - 内核 → 插件：内核直接 Alloc，把 ref 放进 RPC 参数（插件不分配）
//   - 插件 → 内核：插件 RPC arena.alloc 申请，用完 RPC arena.free 归还
//   - 插件退出：内核 ReclaimOwner 回收其残留槽，避免崩溃泄漏耗尽池
//
//	插件归还时校验 owner，防止一个插件释放另一个插件的槽。

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	arenaMagic   uint32 = 0x41524E41 // "ARNA"
	arenaVersion uint32 = 1

	arenaHeaderSize = 64

	// Arena Header 字段偏移（相对 arena 起始）。
	arOffMagic     = 0
	arOffVersion   = 4
	arOffSlotCount = 8
	arOffSlotSize  = 12
	arOffBitmapOff = 16
	arOffSlotsOff  = 20
	arOffReserved  = 24

	// 槽头：数据长度 + 所有者。数据紧随其后。
	slotHeaderSize = 8
	slotOffDataLen = 0
	slotOffOwner   = 4

	// OwnerHost 标记由内核分配的槽。插件用 Host.NextOwnerID 分配的值。
	OwnerHost uint32 = 0

	// 槽池默认规格：32 槽 × 16KB ≈ 512KB。
	arenaDefaultSlotCount = 32
	arenaDefaultSlotSize  = 16 * 1024
)

// arenaRequiredSize 返回给定规格的槽池所需字节数（含头、位图对齐与槽数组）。
func arenaRequiredSize(slotCount, slotSize uint32) uint32 {
	bitmapBytes := ((slotCount+31)/32)*4 + 7
	bitmapBytes &^= 7 // 位图按 8 字节对齐
	slotsOff := (arenaHeaderSize + bitmapBytes + 7) &^ 7
	return slotsOff + slotCount*slotSize
}

// arenaPublishSize 是统一区域里预留给槽池的字节数。
var arenaPublishSize = arenaRequiredSize(arenaDefaultSlotCount, arenaDefaultSlotSize)

// arenaRegion 是槽池视图。
//
// region 始终是**完整**统一区域 mmap：SharedRef.Offset 是相对区域起始的
// 绝对偏移，因此读写都直接落在 region 上，不需要再换算。
//
// mu 只在**内核进程内**使用——分配器完全由内核持有（见文件头所有权模型）。
type arenaRegion struct {
	mu sync.Mutex

	region []byte
	base   uint32 // arena 在 region 内的起始偏移
	cap    uint32 // arena 可用字节数

	slots    uint32
	slotSize uint32
	bmOff    uint32 // 位图在 arena 内的偏移
	slotsOff uint32 // 槽数组在 arena 内的偏移
}

// initArena 在统一区域的 arena 段上初始化槽池并返回句柄。
func initArena(region []byte, base, size uint32, slotCount, slotSize uint32) (*arenaRegion, error) {
	if slotCount == 0 || slotSize <= slotHeaderSize {
		return nil, fmt.Errorf("arena: 非法规格（slots=%d slotSize=%d）", slotCount, slotSize)
	}
	need := arenaRequiredSize(slotCount, slotSize)
	if need > size {
		return nil, fmt.Errorf("arena: 段太小（需要 %d，实际 %d）", need, size)
	}
	if uint64(base)+uint64(need) > uint64(len(region)) {
		return nil, fmt.Errorf("arena: 越出映射（base=%d need=%d total=%d）", base, need, len(region))
	}

	bitmapBytes := ((slotCount+31)/32)*4 + 7
	bitmapBytes &^= 7
	slotsOff := (arenaHeaderSize + bitmapBytes + 7) &^ 7

	ap := region[base : base+need]
	putU32(ap[arOffMagic:], arenaMagic)
	putU32(ap[arOffVersion:], arenaVersion)
	putU32(ap[arOffSlotCount:], slotCount)
	putU32(ap[arOffSlotSize:], slotSize)
	putU32(ap[arOffBitmapOff:], arenaHeaderSize)
	putU32(ap[arOffSlotsOff:], slotsOff)

	// 显式清零位图与槽头，保证复用已存在区域时状态干净。
	for i := uint32(0); i < bitmapBytes; i++ {
		ap[arenaHeaderSize+i] = 0
	}
	for i := uint32(0); i < slotCount; i++ {
		sb := slotsOff + i*slotSize
		putU32(ap[sb+slotOffDataLen:], 0)
		putU32(ap[sb+slotOffOwner:], 0)
	}

	return &arenaRegion{
		region:   region,
		base:     base,
		cap:      need,
		slots:    slotCount,
		slotSize: slotSize,
		bmOff:    arenaHeaderSize,
		slotsOff: slotsOff,
	}, nil
}

// attachArena 从已初始化的区域解析槽池（诊断用；模板不再需要解析位图）。
func attachArena(region []byte, base, size uint32) (*arenaRegion, error) {
	if size < arenaHeaderSize {
		return nil, fmt.Errorf("arena: 段太小（%d）", size)
	}
	if uint64(base)+uint64(arenaHeaderSize) > uint64(len(region)) {
		return nil, fmt.Errorf("arena: 头部越界（base=%d total=%d）", base, len(region))
	}
	ap := region[base : base+size]

	if got := getU32(ap[arOffMagic:]); got != arenaMagic {
		return nil, fmt.Errorf("arena: 魔数不匹配（0x%x）", got)
	}
	if got := getU32(ap[arOffVersion:]); got != arenaVersion {
		return nil, fmt.Errorf("arena: 版本不匹配（%d，期望 %d）", got, arenaVersion)
	}
	slotCount := getU32(ap[arOffSlotCount:])
	slotSize := getU32(ap[arOffSlotSize:])
	bmOff := getU32(ap[arOffBitmapOff:])
	slotsOff := getU32(ap[arOffSlotsOff:])

	if slotCount == 0 || slotSize <= slotHeaderSize {
		return nil, fmt.Errorf("arena: 非法规格（slots=%d slotSize=%d）", slotCount, slotSize)
	}
	if uint64(slotsOff)+uint64(slotCount)*uint64(slotSize) > uint64(size) {
		return nil, fmt.Errorf("arena: 槽数组越出段（slotsOff=%d slots=%d slotSize=%d size=%d）",
			slotsOff, slotCount, slotSize, size)
	}
	if uint64(bmOff)+uint64(((slotCount+31)/32)*4) > uint64(size) {
		return nil, fmt.Errorf("arena: 位图越出段（bmOff=%d slots=%d）", bmOff, slotCount)
	}

	return &arenaRegion{
		region:   region,
		base:     base,
		cap:      size,
		slots:    slotCount,
		slotSize: slotSize,
		bmOff:    bmOff,
		slotsOff: slotsOff,
	}, nil
}

// SlotCount 返回槽总数。
func (a *arenaRegion) SlotCount() uint32 { return a.slots }

// SlotPayloadCap 返回单个槽可承载的最大 payload 字节数。
func (a *arenaRegion) SlotPayloadCap() int { return int(a.slotSize) - slotHeaderSize }

func (a *arenaRegion) bitmapWord(i uint32) *uint32 {
	off := a.base + a.bmOff + i*4
	return (*uint32)(unsafe.Pointer(&a.region[off]))
}

func (a *arenaRegion) slotBase(slot uint32) uint32 {
	return a.base + a.slotsOff + slot*a.slotSize
}

// dataOffset 返回槽数据区的绝对偏移（SharedRef.Offset 即此值）。
func (a *arenaRegion) dataOffset(slot uint32) uint32 {
	return a.slotBase(slot) + slotHeaderSize
}

// Alloc 预留一个槽并写入 owner，返回共享引用（Flags 即槽号，Length 为 n）。
//
// 必须由内核调用：分配器由内核独占（见文件头所有权模型）。
func (a *arenaRegion) Alloc(owner uint32, n int) (SharedRef, error) {
	if n < 0 || n > a.SlotPayloadCap() {
		return SharedRef{}, fmt.Errorf("arena: payload %d 超出槽容量 %d", n, a.SlotPayloadCap())
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	for slot := uint32(0); slot < a.slots; slot++ {
		if a.testAndSetLocked(slot) {
			base := a.slotBase(slot)
			putU32(a.region[base+slotOffDataLen:], uint32(n))
			putU32(a.region[base+slotOffOwner:], owner)
			return SharedRef{
				Offset: a.dataOffset(slot),
				Length: uint32(n),
				Flags:  slot,
			}, nil
		}
	}
	return SharedRef{}, fmt.Errorf("arena: 槽池已满（%d 槽全部占用）", a.slots)
}

// Put 分配并写入 payload，返回共享引用（内核方向发送用）。
func (a *arenaRegion) Put(owner uint32, payload []byte, gen uint64) (SharedRef, error) {
	ref, err := a.Alloc(owner, len(payload))
	if err != nil {
		return SharedRef{}, err
	}
	copy(a.region[ref.Offset:ref.Offset+ref.Length], payload)
	ref.Generation = uint32(gen)
	return ref, nil
}

// MakeRef 为已分配槽构造引用（Length 为 n）。
func (a *arenaRegion) MakeRef(slot uint32, n int, gen uint64) SharedRef {
	return SharedRef{
		Offset:     a.dataOffset(slot),
		Length:     uint32(n),
		Generation: uint32(gen),
		Flags:      slot,
	}
}

// SlotOf 从 SharedRef 解出槽号，并校验它与 Offset 自洽。
func (a *arenaRegion) SlotOf(ref SharedRef) (uint32, bool) {
	slot := ref.Flags
	if slot >= a.slots {
		return 0, false
	}
	if uint32(ref.Offset) != a.dataOffset(slot) {
		return 0, false
	}
	if int(ref.Length) > a.SlotPayloadCap() {
		return 0, false
	}
	return slot, true
}

// Read 按 SharedRef 读取槽数据。
//
// 校验四件事，任一不满足都返回 error（而不是像早期版本那样返回 nil，
// 让调用方分不清"空数据"和"非法引用"）：
//  1. generation 与当前区域一致（remap 后旧引用失效）
//  2. 槽号在范围内，且 Offset 与槽数据区自洽
//  3. Length 不超过槽容量
//  4. 槽当前处于占用状态（已被释放的槽不可再读）
func (a *arenaRegion) Read(ref SharedRef, gen uint64) ([]byte, error) {
	if ref.IsZero() {
		return nil, nil
	}
	if ref.Generation != uint32(gen) {
		return nil, fmt.Errorf("arena: 引用 generation 过期（ref=%d now=%d）", ref.Generation, gen)
	}
	slot, ok := a.SlotOf(ref)
	if !ok {
		return nil, fmt.Errorf("arena: 非法引用（offset=%d len=%d slot=%d）", ref.Offset, ref.Length, ref.Flags)
	}
	if !a.IsBusy(slot) {
		return nil, fmt.Errorf("arena: 槽 %d 已被释放，引用失效", slot)
	}
	start := a.dataOffset(slot)
	return a.region[start : start+ref.Length], nil
}

// Free 释放 owner 名下的槽。
//
// 校验 owner 是关键安全边界：一个插件不能释放另一个插件（或内核）的槽。
func (a *arenaRegion) Free(owner uint32, slot uint32) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if slot >= a.slots {
		return fmt.Errorf("arena: 槽号越界 %d", slot)
	}
	if !a.isBusyLocked(slot) {
		return fmt.Errorf("arena: 槽 %d 未分配（重复释放？）", slot)
	}
	base := a.slotBase(slot)
	got := getU32(a.region[base+slotOffOwner:])
	if got != owner {
		return fmt.Errorf("arena: 槽 %d 不属于调用者（owner=%d caller=%d）", slot, got, owner)
	}
	a.clearLocked(slot)
	return nil
}

// ReclaimOwner 释放 owner 名下所有槽，返回回收数量。
//
// 用于插件进程退出：崩溃的插件无法归还自己申请的槽，若不管会把池慢慢
// 耗尽，最终让所有走共享内存的调用退化成内联 RPC。
func (a *arenaRegion) ReclaimOwner(owner uint32) int {
	if owner == OwnerHost {
		return 0 // 内核自己的槽由正常路径释放，不在此回收
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	reclaimed := 0
	for slot := uint32(0); slot < a.slots; slot++ {
		if !a.isBusyLocked(slot) {
			continue
		}
		base := a.slotBase(slot)
		if getU32(a.region[base+slotOffOwner:]) != owner {
			continue
		}
		a.clearLocked(slot)
		reclaimed++
	}
	return reclaimed
}

// Stats 返回 (已用槽数, 总槽数)，供诊断与测试断言。
func (a *arenaRegion) Stats() (used, total uint32) {
	for slot := uint32(0); slot < a.slots; slot++ {
		if a.IsBusy(slot) {
			used++
		}
	}
	return used, a.slots
}

// OwnerOf 返回槽当前的 owner（诊断用，槽空闲时返回 OwnerHost）。
func (a *arenaRegion) OwnerOf(slot uint32) uint32 {
	if slot >= a.slots {
		return OwnerHost
	}
	return getU32(a.region[a.slotBase(slot)+slotOffOwner:])
}

// IsBusy 报告槽当前是否被占用。
func (a *arenaRegion) IsBusy(slot uint32) bool {
	if slot >= a.slots {
		return false
	}
	return atomic.LoadUint32(a.bitmapWord(slot/32))&(1<<(slot%32)) != 0
}

// ---- 位图内部操作（调用方必须持有 a.mu）----

func (a *arenaRegion) isBusyLocked(slot uint32) bool {
	return atomic.LoadUint32(a.bitmapWord(slot/32))&(1<<(slot%32)) != 0
}

// testAndSetLocked 尝试占用 slot；已占用返回 false。
func (a *arenaRegion) testAndSetLocked(slot uint32) bool {
	w := a.bitmapWord(slot / 32)
	bit := uint32(1) << (slot % 32)
	for {
		cur := atomic.LoadUint32(w)
		if cur&bit != 0 {
			return false
		}
		if atomic.CompareAndSwapUint32(w, cur, cur|bit) {
			return true
		}
	}
}

func (a *arenaRegion) clearLocked(slot uint32) {
	w := a.bitmapWord(slot / 32)
	bit := uint32(1) << (slot % 32)
	for {
		cur := atomic.LoadUint32(w)
		if cur&bit == 0 {
			return
		}
		if atomic.CompareAndSwapUint32(w, cur, cur&^bit) {
			base := a.slotBase(slot)
			putU32(a.region[base+slotOffDataLen:], 0)
			putU32(a.region[base+slotOffOwner:], 0)
			return
		}
	}
}

package proc

import (
	"bytes"
	"sync"
	"testing"
)

// newTestArena 构造一个独立槽池（不经 Host），保持单测快速。
func newTestArena(t *testing.T, slots, slotSize uint32) *arenaRegion {
	t.Helper()
	const base = 64
	need := arenaRequiredSize(slots, slotSize)
	region := make([]byte, base+need)
	a, err := initArena(region, base, need, slots, slotSize)
	if err != nil {
		t.Fatalf("initArena: %v", err)
	}
	return a
}

func TestArena_AllocFreeRoundTrip(t *testing.T) {
	a := newTestArena(t, 4, 64)
	if got, total := a.Stats(); got != 0 || total != 4 {
		t.Fatalf("初始 Stats: got (%d,%d), want (0,4)", got, total)
	}

	ref, err := a.Put(OwnerHost, []byte("hello"), 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if used, _ := a.Stats(); used != 1 {
		t.Fatalf("Put 后 used=%d, want 1", used)
	}
	got, err := a.Read(ref, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Read=%q, want hello", got)
	}

	if err := a.Free(OwnerHost, ref.Flags); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if used, _ := a.Stats(); used != 0 {
		t.Fatalf("Free 后 used=%d, want 0", used)
	}
	// 归还后再读必须失败（槽已回收）
	if _, err := a.Read(ref, 0); err == nil {
		t.Fatal("已释放槽的引用应读取失败")
	}
}

func TestArena_ConcurrentAllocUnique(t *testing.T) {
	const slots = 32
	a := newTestArena(t, slots, 64)

	refs := make(chan SharedRef, slots)
	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := a.Alloc(OwnerHost, 8)
			if err != nil {
				t.Errorf("Alloc: %v", err)
				return
			}
			refs <- ref
		}()
	}
	wg.Wait()
	close(refs)

	seen := make(map[uint32]bool, slots)
	for ref := range refs {
		if seen[ref.Flags] {
			t.Fatalf("并发分配拿到重复槽 %d", ref.Flags)
		}
		seen[ref.Flags] = true
	}
	if len(seen) != slots {
		t.Fatalf("唯一槽数=%d, want %d", len(seen), slots)
	}
	if used, _ := a.Stats(); used != slots {
		t.Fatalf("used=%d, want %d", used, slots)
	}
}

func TestArena_ExhaustionReturnsError(t *testing.T) {
	a := newTestArena(t, 2, 64)
	for i := 0; i < 2; i++ {
		if _, err := a.Alloc(OwnerHost, 8); err != nil {
			t.Fatalf("第 %d 次 Alloc: %v", i, err)
		}
	}
	if _, err := a.Alloc(OwnerHost, 8); err == nil {
		t.Fatal("槽池耗尽时 Alloc 应返回错误")
	}
}

func TestArena_RejectsOversizePayload(t *testing.T) {
	a := newTestArena(t, 2, 64)
	if _, err := a.Alloc(OwnerHost, a.SlotPayloadCap()+1); err == nil {
		t.Fatal("超出槽容量的 payload 应被拒绝")
	}
}

func TestArena_FreeEnforcesOwnership(t *testing.T) {
	a := newTestArena(t, 4, 64)
	const ownerA, ownerB = 7, 9

	refA, err := a.Alloc(ownerA, 8)
	if err != nil {
		t.Fatal(err)
	}
	// 另一个插件不能释放 A 的槽
	if err := a.Free(ownerB, refA.Flags); err == nil {
		t.Fatal("跨 owner 释放应被拒绝")
	}
	if used, _ := a.Stats(); used != 1 {
		t.Fatalf("拒绝释放后 used=%d, want 1", used)
	}
	if a.OwnerOf(refA.Flags) != ownerA {
		t.Fatalf("槽 %d 的 owner 应为 %d", refA.Flags, ownerA)
	}
	// 正确的 owner 可以释放
	if err := a.Free(ownerA, refA.Flags); err != nil {
		t.Fatalf("同 owner 释放: %v", err)
	}
}

func TestArena_ReclaimOwnerReleasesOnlyItsSlots(t *testing.T) {
	a := newTestArena(t, 8, 64)
	const ownerA, ownerB = 7, 9

	for i := 0; i < 3; i++ {
		if _, err := a.Alloc(ownerA, 8); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := a.Alloc(ownerB, 8); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.ReclaimOwner(ownerA); n != 3 {
		t.Fatalf("ReclaimOwner(A)=%d, want 3", n)
	}
	if used, _ := a.Stats(); used != 2 {
		t.Fatalf("回收 A 后 used=%d, want 2（B 的槽必须保留）", used)
	}
	// 内核自己的槽不参与回收
	if n := a.ReclaimOwner(OwnerHost); n != 0 {
		t.Fatalf("ReclaimOwner(host)=%d, want 0", n)
	}
}

func TestArena_ReclaimHostIsNoop(t *testing.T) {
	a := newTestArena(t, 4, 64)
	if _, err := a.Alloc(OwnerHost, 8); err != nil {
		t.Fatal(err)
	}
	if n := a.ReclaimOwner(OwnerHost); n != 0 {
		t.Fatalf("内核槽不应被 ReclaimOwner 回收，实际回收 %d", n)
	}
	if used, _ := a.Stats(); used != 1 {
		t.Fatalf("used=%d, want 1", used)
	}
}

func TestArena_ReadRejectsInvalidRefs(t *testing.T) {
	a := newTestArena(t, 4, 256)
	ref, err := a.Put(OwnerHost, []byte("payload"), 0)
	if err != nil {
		t.Fatal(err)
	}

	// generation 过期
	stale := ref
	stale.Generation = 99
	if _, err := a.Read(stale, 0); err == nil {
		t.Fatal("generation 不匹配应被拒绝")
	}

	// offset 与槽不自洽
	badOffset := ref
	badOffset.Offset++
	if _, err := a.Read(badOffset, 0); err == nil {
		t.Fatal("offset 与槽不自洽应被拒绝")
	}

	// 槽号越界
	badSlot := ref
	badSlot.Flags = 999
	if _, err := a.Read(badSlot, 0); err == nil {
		t.Fatal("槽号越界应被拒绝")
	}

	// Length 超容量
	tooLong := ref
	tooLong.Length = uint32(a.SlotPayloadCap() + 1)
	if _, err := a.Read(tooLong, 0); err == nil {
		t.Fatal("Length 超容量应被拒绝")
	}
}

func TestArena_PutReadLargePayload(t *testing.T) {
	a := newTestArena(t, 4, 4096)
	payload := bytes.Repeat([]byte("abcdefgh"), 400) // 3200 字节
	ref, err := a.Put(OwnerHost, payload, 3)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := a.Read(ref, 3)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回数据不一致：len(got)=%d len(want)=%d", len(got), len(payload))
	}
}

func TestArena_AttachRejectsCorruptLayout(t *testing.T) {
	a := newTestArena(t, 4, 256)

	// 魔数被改
	putU32(a.region[a.base+arOffMagic:], 0xDEADBEEF)
	if _, err := attachArena(a.region, a.base, a.cap); err == nil {
		t.Fatal("魔数错误应被拒绝")
	}
	putU32(a.region[a.base+arOffMagic:], arenaMagic)

	// 版本被改
	putU32(a.region[a.base+arOffVersion:], 99)
	if _, err := attachArena(a.region, a.base, a.cap); err == nil {
		t.Fatal("版本不匹配应被拒绝")
	}
	putU32(a.region[a.base+arOffVersion:], arenaVersion)

	// 槽数组越界
	putU32(a.region[a.base+arOffSlotCount:], 1<<20)
	if _, err := attachArena(a.region, a.base, a.cap); err == nil {
		t.Fatal("槽数组越界应被拒绝")
	}
}

func TestArenaRequiredSizeAlignment(t *testing.T) {
	// 位图按 8 字节对齐、槽数组 8 字节对齐，保证 4 字节原子操作不跨页/不越界。
	for _, tc := range []struct{ slots, slotSize uint32 }{
		{1, 16}, {8, 64}, {32, 16384}, {33, 1024}, {64, 512}, {100, 256},
	} {
		need := arenaRequiredSize(tc.slots, tc.slotSize)
		if need%8 != 0 {
			t.Errorf("slots=%d slotSize=%d: arenaRequiredSize=%d 不是 8 的倍数", tc.slots, tc.slotSize, need)
		}
		region := make([]byte, 64+need)
		if _, err := initArena(region, 64, need, tc.slots, tc.slotSize); err != nil {
			t.Errorf("slots=%d slotSize=%d: initArena 失败: %v", tc.slots, tc.slotSize, err)
		}
	}
}

package proc

import (
	"bytes"
	"sync"
	"testing"
)

// newTestArena 构造一个独立块分配器（不经 Host），保持单测快速。
func newTestArena(t *testing.T, size uint32) *arenaRegion {
	t.Helper()
	const base = 64 // 8 字节对齐
	region := make([]byte, base+size)
	a, err := initArena(region, base, size)
	if err != nil {
		t.Fatalf("initArena: %v", err)
	}
	return a
}

func TestArena_AllocFreeRoundTrip(t *testing.T) {
	a := newTestArena(t, 8*1024)
	if used, total := a.Stats(); used != 0 || total != 8*1024 {
		t.Fatalf("初始 Stats: got (%d,%d), want (0,%d)", used, total, 8*1024)
	}

	ref, err := a.Put(OwnerHost, []byte("hello"), 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if used, _ := a.Stats(); used == 0 {
		t.Fatal("Put 后 used 应大于 0")
	}
	got, err := a.Read(ref, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Read=%q, want hello", got)
	}

	if err := a.Free(OwnerHost, ref); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if used, _ := a.Stats(); used != 0 {
		t.Fatalf("Free 后 used=%d, want 0", used)
	}
	// 归还后再读必须失败（块已回收）
	if _, err := a.Read(ref, 0); err == nil {
		t.Fatal("已释放块的引用应读取失败")
	}
}

func TestArena_ConcurrentAllocUnique(t *testing.T) {
	const workers = 64
	a := newTestArena(t, 256*1024)

	refs := make(chan SharedRef, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := a.Alloc(OwnerHost, 1024, 0)
			if err != nil {
				t.Errorf("Alloc: %v", err)
				return
			}
			refs <- ref
		}()
	}
	wg.Wait()
	close(refs)

	seen := make(map[uint32]bool, workers)
	for ref := range refs {
		if seen[ref.Offset] {
			t.Fatalf("并发分配拿到重复 offset=%d", ref.Offset)
		}
		seen[ref.Offset] = true
	}
	if len(seen) != workers {
		t.Fatalf("唯一块数=%d, want %d", len(seen), workers)
	}
}

func TestArena_ExhaustionReturnsError(t *testing.T) {
	a := newTestArena(t, 4*1024)
	// 第一次分配吃掉几乎整块
	if _, err := a.Alloc(OwnerHost, 3*1024, 0); err != nil {
		t.Fatalf("首次 Alloc: %v", err)
	}
	// 再申请一大块必然失败
	if _, err := a.Alloc(OwnerHost, 3*1024, 0); err == nil {
		t.Fatal("空间不足时 Alloc 应返回错误")
	}
}

func TestArena_RejectsOversizePayload(t *testing.T) {
	a := newTestArena(t, 4*1024)
	if _, err := a.Alloc(OwnerHost, a.MaxPayload()+1, 0); err == nil {
		t.Fatal("超出 arena 容量的请求应被拒绝")
	}
}

func TestArena_FreeEnforcesOwnership(t *testing.T) {
	a := newTestArena(t, 8*1024)
	const ownerA, ownerB = 7, 9

	refA, err := a.Alloc(ownerA, 128, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 另一个插件不能释放 A 的块
	if err := a.Free(ownerB, refA); err == nil {
		t.Fatal("跨 owner 归还应被拒绝")
	}
	if used, _ := a.Stats(); used == 0 {
		t.Fatal("拒绝归还后块应仍然占用")
	}
	// 正确的 owner 可以归还
	if err := a.Free(ownerA, refA); err != nil {
		t.Fatalf("同 owner 归还: %v", err)
	}
}

func TestArena_FreeRejectsForgedOffset(t *testing.T) {
	a := newTestArena(t, 8*1024)
	ref, err := a.Alloc(OwnerHost, 256, 0)
	if err != nil {
		t.Fatal(err)
	}

	// 未对齐到块数据起点的 offset 必须被拒绝——否则会直接破坏块链。
	forged := ref
	forged.Offset += 8
	if err := a.Free(OwnerHost, forged); err == nil {
		t.Fatal("伪造 offset 应被拒绝")
	}
	// 合法引用仍能正常归还（分配器状态未被破坏）
	if err := a.Free(OwnerHost, ref); err != nil {
		t.Fatalf("合法引用不应受影响: %v", err)
	}
	if used, _ := a.Stats(); used != 0 {
		t.Fatalf("归还后 used=%d, want 0", used)
	}
}

func TestArena_ReclaimOwnerReleasesOnlyItsBlocks(t *testing.T) {
	a := newTestArena(t, 64*1024)
	const ownerA, ownerB = 7, 9

	for i := 0; i < 3; i++ {
		if _, err := a.Alloc(ownerA, 1024, 0); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := a.Alloc(ownerB, 1024, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.ReclaimOwner(ownerA); n != 3 {
		t.Fatalf("ReclaimOwner(A)=%d, want 3", n)
	}
	// B 的两块必须保留：仍能读回
	if n := a.ReclaimOwner(ownerB); n != 2 {
		t.Fatalf("ReclaimOwner(B)=%d, want 2（A 的回收不能误伤 B）", n)
	}
	if used, _ := a.Stats(); used != 0 {
		t.Fatalf("全部回收后 used=%d, want 0", used)
	}
}

func TestArena_ReclaimHostIsNoop(t *testing.T) {
	a := newTestArena(t, 4*1024)
	if _, err := a.Alloc(OwnerHost, 128, 0); err != nil {
		t.Fatal(err)
	}
	if n := a.ReclaimOwner(OwnerHost); n != 0 {
		t.Fatalf("内核块不应被 ReclaimOwner 回收，实际回收 %d", n)
	}
	if used, _ := a.Stats(); used == 0 {
		t.Fatal("used 应仍大于 0")
	}
}

func TestArena_ReadRejectsInvalidRefs(t *testing.T) {
	a := newTestArena(t, 8*1024)
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

	// offset 不是块数据起点
	badOffset := ref
	badOffset.Offset += 8
	if _, err := a.Read(badOffset, 0); err == nil {
		t.Fatal("offset 不是块数据起点应被拒绝")
	}

	// Length 超出块容量
	tooLong := ref
	tooLong.Length = 1 << 20
	if _, err := a.Read(tooLong, 0); err == nil {
		t.Fatal("Length 超容量应被拒绝")
	}
}

func TestArena_PutReadLargePayload(t *testing.T) {
	a := newTestArena(t, 512*1024)
	payload := bytes.Repeat([]byte("abcdefgh"), 8192) // 64KB
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
	// 大 payload 不能被 16KB 定长槽时代的容量假设卡住
	if len(got) <= 16*1024 {
		t.Fatalf("本用例应验证超过旧 16KB 槽容量的 payload，实际 %d", len(got))
	}
}

func TestArena_FreeCoalescesAdjacentBlocks(t *testing.T) {
	a := newTestArena(t, 64*1024)

	refs := make([]SharedRef, 0, 8)
	for i := 0; i < 8; i++ {
		r, err := a.Alloc(OwnerHost, 4*1024, 0)
		if err != nil {
			t.Fatalf("第 %d 次 Alloc: %v", i, err)
		}
		refs = append(refs, r)
	}
	for _, r := range refs {
		if err := a.Free(OwnerHost, r); err != nil {
			t.Fatalf("Free: %v", err)
		}
	}

	// 全部归还并合并后，应能再分配一个接近整块的大块。
	big, err := a.Alloc(OwnerHost, 48*1024, 0)
	if err != nil {
		t.Fatalf("合并后应能分配大块: %v", err)
	}
	if err := a.Free(OwnerHost, big); err != nil {
		t.Fatal(err)
	}
	if n := a.BlockCount(); n != 1 {
		t.Fatalf("全部归还并合并后应只剩 1 块，实际 %d", n)
	}
}

func TestArena_AttachRejectsCorruptLayout(t *testing.T) {
	a := newTestArena(t, 4*1024)

	putU32(a.region[a.base+arOffMagic:], 0xDEADBEEF)
	if _, err := attachArena(a.region, a.base, a.cap); err == nil {
		t.Fatal("魔数错误应被拒绝")
	}
	putU32(a.region[a.base+arOffMagic:], arenaMagic)

	putU32(a.region[a.base+arOffVersion:], 99)
	if _, err := attachArena(a.region, a.base, a.cap); err == nil {
		t.Fatal("版本不匹配应被拒绝")
	}
	putU32(a.region[a.base+arOffVersion:], arenaVersion)

	putU32(a.region[a.base+arOffCapacity:], 1<<30)
	if _, err := attachArena(a.region, a.base, a.cap); err == nil {
		t.Fatal("capacity 不匹配应被拒绝")
	}
}

func TestArena_InitRejectsTooSmall(t *testing.T) {
	const base = 64
	region := make([]byte, base+arenaMinSize()-8)
	if _, err := initArena(region, base, arenaMinSize()-8); err == nil {
		t.Fatal("过小的段应被拒绝")
	}
}

func TestArena_DataOffsetsAligned(t *testing.T) {
	a := newTestArena(t, 64*1024)
	for i, size := range []uint32{1, 7, 8, 9, 100, 4096} {
		ref, err := a.Alloc(OwnerHost, int(size), 0)
		if err != nil {
			t.Fatalf("第 %d 次 Alloc(size=%d): %v", i, size, err)
		}
		if ref.Offset%8 != 0 {
			t.Fatalf("size=%d 的数据起点 %d 未 8 字节对齐", size, ref.Offset)
		}
	}
}

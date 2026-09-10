package proc

import (
	"sync"
	"testing"
	"unsafe"
)

func TestToolCallRing_InitAndReserve(t *testing.T) {
	arenaSize := 256 * 1024
	size := superBlockSize + int(shmDefaultSize) + int(evtTotalSize) + arenaSize + int(trlOffFrameBase+toolRingCap*toolFrameSize)
	data := make([]byte, size)
	putU32(data[0:], unifiedMagic)
	putU32(data[4:], unifiedVersion)
	putU32(data[16:], uint32(size))

	ringData := data[size-int(trlOffFrameBase+toolRingCap*toolFrameSize) : size]
	if err := InitToolRing(ringData); err != nil {
		t.Fatalf("InitToolRing: %v", err)
	}

	ring, err := AttachToolRing(ringData)
	if err != nil {
		t.Fatalf("AttachToolRing: %v", err)
	}
	if ring.cap != toolRingCap {
		t.Fatalf("cap: got %d, want %d", ring.cap, toolRingCap)
	}

	// Reserve 帧应成功
	idx, err := ring.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if idx != 0 {
		t.Fatalf("首帧 index: got %d, want 0", idx)
	}

	// SetCalling -> GetCallingFrame -> SetReady -> GetReadyFrame -> ReleaseFrame
	inputRef := SharedRef{Offset: 1024, Length: 100, Generation: 0, Flags: 1}
	ring.SetCalling(idx, 42, "demo_upper", inputRef)

	name, gotInput := ring.GetCallingFrame(idx)
	if name != "demo_upper" {
		t.Fatalf("GetCallingFrame name: got %q, want %q", name, "demo_upper")
	}
	if gotInput.Offset != 1024 || gotInput.Length != 100 {
		t.Fatalf("GetCallingFrame input: got %+v", gotInput)
	}

	ring.SetReading(idx)

	outputRef := SharedRef{Offset: 2048, Length: 50, Generation: 0, Flags: 0}
	ring.SetReady(idx, outputRef)

	reqID, gotOutput := ring.GetReadyFrame(idx)
	if reqID != 42 {
		t.Fatalf("GetReadyFrame reqID: got %d, want 42", reqID)
	}
	if gotOutput.Offset != 2048 || gotOutput.Length != 50 {
		t.Fatalf("GetReadyFrame output: got %+v", gotOutput)
	}

	ring.ReleaseFrame(idx)

	// 释放后应能重用
	idx2, err := ring.Reserve()
	if err != nil {
		t.Fatalf("Reserve after release: %v", err)
	}
	if idx2 != 1 {
		t.Fatalf("第二帧 index: got %d, want 1", idx2)
	}
}

func TestToolCallRing_FullBackpressure(t *testing.T) {
	size := int(toolRingCap)*int(toolFrameSize) + 64*1024
	data := make([]byte, size)
	if err := InitToolRing(data); err != nil {
		t.Fatalf("InitToolRing: %v", err)
	}
	ring, err := AttachToolRing(data)
	if err != nil {
		t.Fatalf("AttachToolRing: %v", err)
	}

	// Reserve 所有帧
	for i := uint32(0); i < toolRingCap; i++ {
		idx, err := ring.Reserve()
		if err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
		ring.SetCalling(idx, uint64(i+1), "tool", SharedRef{})
		ring.SetReading(idx)
		// 不释放
	}

	// 再 Reserve 应失败（背压）
	_, err = ring.Reserve()
	if err != ErrToolCallRingFull {
		t.Fatalf("满帧后 Reserve 应返回 ErrToolCallRingFull，实际: %v", err)
	}

	// 释放一帧后应恢复
	ring.ReleaseFrame(0)
	idx, err := ring.Reserve()
	if err != nil {
		t.Fatalf("释放后 Reserve: %v", err)
	}
	_ = idx
}

func TestToolCallRing_FindFrameByReqID(t *testing.T) {
	size := int(toolRingCap)*int(toolFrameSize) + 64*1024
	data := make([]byte, size)
	if err := InitToolRing(data); err != nil {
		t.Fatal(err)
	}
	ring, _ := AttachToolRing(data)

	// 没有帧时应找不到
	_, err := ring.FindFrameByReqID(999)
	if err != ErrToolCallNotFound {
		t.Fatalf("空 ring 应返回 ErrToolCallNotFound，实际: %v", err)
	}

	// Reserve + SetCalling 后应能找到
	idx, _ := ring.Reserve()
	ring.SetCalling(idx, 100, "tool_a", SharedRef{})
	ring.SetReading(idx)

	found, err := ring.FindFrameByReqID(100)
	if err != nil {
		t.Fatalf("FindFrameByReqID: %v", err)
	}
	if found != idx {
		t.Fatalf("FindFrameByReqID: got %d, want %d", found, idx)
	}

	// Release 后应找不到
	ring.ReleaseFrame(idx)
	_, err = ring.FindFrameByReqID(100)
	if err != ErrToolCallNotFound {
		t.Fatalf("释放后应找不到，实际: %v", err)
	}
}

func TestSharedRef_PackUnpack(t *testing.T) {
	b := make([]byte, sharedRefSize)
	ref := SharedRef{Offset: 1024, Length: 256, Generation: 7, Flags: 3}
	packSharedRef(b, ref)
	got := unpackSharedRef(b)
	if got.Offset != ref.Offset || got.Length != ref.Length || got.Generation != ref.Generation || got.Flags != ref.Flags {
		t.Fatalf("pack/unpack: got %+v, want %+v", got, ref)
	}
}

func TestToolCallRing_ConcurrentReserveUnique(t *testing.T) {
	data := make([]byte, int(trlOffFrameBase+toolRingCap*toolFrameSize))
	if err := InitToolRing(data); err != nil {
		t.Fatal(err)
	}
	ring, err := AttachToolRing(data)
	if err != nil {
		t.Fatal(err)
	}

	indices := make(chan uint32, toolRingCap)
	var wg sync.WaitGroup
	for i := uint32(0); i < toolRingCap; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, reserveErr := ring.Reserve()
			if reserveErr != nil {
				t.Errorf("Reserve: %v", reserveErr)
				return
			}
			indices <- idx
		}()
	}
	wg.Wait()
	close(indices)

	seen := make(map[uint32]bool, toolRingCap)
	for idx := range indices {
		if seen[idx] {
			t.Fatalf("并发 Reserve 重复分配帧 %d", idx)
		}
		seen[idx] = true
	}
	if len(seen) != int(toolRingCap) {
		t.Fatalf("唯一帧数=%d，期望 %d", len(seen), toolRingCap)
	}
}

func TestAttachToolRingRejectsInvalidLayout(t *testing.T) {
	data := make([]byte, trlOffFrameBase)
	putU32(data[trlOffMagic:], toolRingMagic)
	putU32(data[trlOffVersion:], toolRingVersion)
	putU32(data[trlOffCap:], toolRingCap)
	putU32(data[trlOffFrameSize:], toolFrameSize)
	if _, err := AttachToolRing(data); err == nil {
		t.Fatal("越界布局应被拒绝")
	}
}

func TestToolFrameLayoutAligned(t *testing.T) {
	// 确保帧布局字段偏移与内存布局一致（安全断言）
	var frame toolFrame
	off := func(ptr *uint32) uint32 {
		return uint32(uintptr(unsafe.Pointer(ptr)) - uintptr(unsafe.Pointer(&frame)))
	}
	if off(&frame.state) != toolFrameOffState {
		t.Errorf("state offset: got %d, want %d", off(&frame.state), toolFrameOffState)
	}
}

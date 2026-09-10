package proc

// ToolCall lane 实现（§13.3）。
//
// 采用操作系统函数调用栈帧模型：每个工具调用 = 一个固定帧，帧满背压，完成即回收。
// 不是 arena 堆分配，是 ring buffer 上的 push/pop。
//
// +--------------------------------------------+
// | ToolCall Ring Header 32B                   |
// |   magic / cap / frameSize / writeIdx       |
// +--------------------------------------------+
// | Frame 0: state(4) + reqID(8) + name(64) + |
// |          inputRef(16) + outputRef(16)      |
// | Frame 1: ...                               |
// | Frame N-1: ...                             |
// +--------------------------------------------+
// | Dynamic Arena (input/output JSON data)     |
// +--------------------------------------------+
//
// 状态机：
//   FREE -> CALLING (内核写 inputRef，eventfd 通知插件)
//        -> READING (插件读 input，执行，写 outputRef，通知内核)
//        -> FREE (内核读 output，帧回收)
//
// 背压：writeIdx 追上最后的 busy 帧时返回 ErrToolCallRingFull。
// 零竞争：插件只操作自己 requestID 对应的帧。

import (
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

const (
	toolRingMagic    uint32 = 0x54524C47 // "TRLG" — TooL Ring
	toolRingVersion  uint32 = 1
	toolRingCap      uint32 = 64 // 最大并发工具调用
	toolFrameNameLen uint32 = 64 // 工具名最大长度

	// 帧偏移（相对帧起始）
	toolFrameOffState  = 0   // uint32: 帧状态
	toolFrameOffReqID  = 8   // uint64: 请求 ID
	toolFrameOffName   = 16  // [16, 80): 工具名（64B）
	toolFrameOffInput  = 80  // SharedRef(16): input 描述符
	toolFrameOffOutput = 96  // SharedRef(16): output 描述符
	toolFrameSize      = 112 // 单帧总大小

	// 帧状态
	toolFrameFree     uint32 = 0
	toolFrameReserved uint32 = 1 // 内核已占帧，正在写入元数据
	toolFrameCalling  uint32 = 2 // 内核写完 input，插件待读
	toolFrameReading  uint32 = 3 // 插件正在执行
	toolFrameReady    uint32 = 4 // 插件写完 output，内核待读
)

// ToolRing 头偏移（相对区域起始）
const (
	trlOffMagic     uint32 = 0
	trlOffVersion   uint32 = 4
	trlOffCap       uint32 = 8
	trlOffFrameSize uint32 = 12
	trlOffFrameBase uint32 = 16
)

var (
	ErrToolCallRingFull = errors.New("proc: tool call ring full")
	ErrToolCallNotFound = errors.New("proc: tool call frame not found")
)

// toolFrame 是单个帧的内存布局。
type toolFrame struct {
	state  uint32 // atomic
	reqID  uint64
	name   [toolFrameNameLen]byte
	input  SharedRef
	output SharedRef
}

// ToolCallRing 内核侧的 ToolCall ring 视图。
type ToolCallRing struct {
	data       []byte
	cap        uint32
	frameSize  uint32
	framesBase uint32
	writeIdx   atomic.Uint64 // 下一个可写帧 index
	gen        *uint32       // 统一区域 generation（变化则 snapshot 失效）
}

// InitToolRing 在统一区域的 ToolCall 子区域上初始化 ring header。
func InitToolRing(data []byte) error {
	if uint32(len(data)) < trlOffFrameBase+toolRingCap*toolFrameSize {
		return fmt.Errorf("tool ring: 区域过小（%d 字节，至少需要 %d）",
			len(data), trlOffFrameBase+toolRingCap*toolFrameSize)
	}

	putU32(data[trlOffMagic:], toolRingMagic)
	putU32(data[trlOffVersion:], toolRingVersion)
	putU32(data[trlOffCap:], toolRingCap)
	putU32(data[trlOffFrameSize:], toolFrameSize)

	// 初始化所有帧为 FREE
	for i := uint32(0); i < toolRingCap; i++ {
		frameOff := trlOffFrameBase + i*toolFrameSize
		putU32(data[frameOff+toolFrameOffState:], toolFrameFree)
	}

	return nil
}

// AttachToolRing 挂载已初始化的 ToolCall ring（插件侧调用）。
func AttachToolRing(data []byte) (*ToolCallRing, error) {
	if uint32(len(data)) < trlOffFrameBase {
		return nil, fmt.Errorf("tool ring: 区域过小")
	}
	magic := getU32(data[trlOffMagic:])
	if magic != toolRingMagic {
		return nil, fmt.Errorf("tool ring: 魔数不匹配（0x%x）", magic)
	}
	cap := getU32(data[trlOffCap:])
	frameSize := getU32(data[trlOffFrameSize:])
	version := getU32(data[trlOffVersion:])
	if version != toolRingVersion {
		return nil, fmt.Errorf("tool ring: 版本不匹配（%d，期望 %d）", version, toolRingVersion)
	}
	if cap == 0 || frameSize != toolFrameSize || uint64(trlOffFrameBase)+uint64(cap)*uint64(frameSize) > uint64(len(data)) {
		return nil, fmt.Errorf("tool ring: 布局非法（cap=%d frameSize=%d total=%d）", cap, frameSize, len(data))
	}
	return &ToolCallRing{
		data:       data,
		cap:        cap,
		frameSize:  frameSize,
		framesBase: trlOffFrameBase,
	}, nil
}

// Reserve 为一次工具调用预留帧（内核调用）。
//
// 环形扫描：从 writeIdx 开始，绕环一圈找 FREE 帧。全部忙时背压。
func (r *ToolCallRing) Reserve() (frameIdx uint32, err error) {
	start := r.writeIdx.Add(1) - 1
	for i := uint64(0); i < uint64(r.cap); i++ {
		idx := start + i
		fi := uint32(idx % uint64(r.cap))
		if atomic.CompareAndSwapUint32(r.statePtr(fi), toolFrameFree, toolFrameReserved) {
			return fi, nil
		}
	}
	return 0, ErrToolCallRingFull
}

// SetCalling 将帧状态设为 CALLING 并写入 requestID 和工具名（内核调用）。
func (r *ToolCallRing) SetCalling(frameIdx uint32, reqID uint64, toolName string, inputRef SharedRef) {
	off := r.frameOff(frameIdx)
	// 写 requestID
	putU64(r.data[off+toolFrameOffReqID:], reqID)
	// 写工具名（截断到 64B）
	nameBytes := []byte(toolName)
	if len(nameBytes) > int(toolFrameNameLen) {
		nameBytes = nameBytes[:toolFrameNameLen]
	}
	for i := range r.data[off+toolFrameOffName : off+toolFrameOffName+toolFrameNameLen] {
		r.data[off+toolFrameOffName+uint32(i)] = 0
	}
	copy(r.data[off+toolFrameOffName:], nameBytes)
	// 写 input SharedRef
	packSharedRef(r.data[off+toolFrameOffInput:], inputRef)
	// 原子设置状态为 CALLING
	atomic.StoreUint32(r.statePtr(frameIdx), toolFrameCalling)
}

// FindFrameByReqID 查找对应 requestID 的帧（插件调用）。
// 返回帧索引和帧内偏移数据，或 ErrToolCallNotFound。
func (r *ToolCallRing) FindFrameByReqID(reqID uint64) (uint32, error) {
	for i := uint32(0); i < r.cap; i++ {
		off := r.frameOff(i)
		state := atomic.LoadUint32(r.statePtr(i))
		if state == toolFrameCalling || state == toolFrameReading {
			fid := getU64(r.data[off+toolFrameOffReqID:])
			if fid == reqID {
				return i, nil
			}
		}
	}
	return 0, ErrToolCallNotFound
}

// GetCallingFrame 读取帧的工具名和 input 描述符（插件侧，正在 CALLING 的帧）。
func (r *ToolCallRing) GetCallingFrame(frameIdx uint32) (name string, inputRef SharedRef) {
	off := r.frameOff(frameIdx)
	nameBytes := r.data[off+toolFrameOffName : off+toolFrameOffName+toolFrameNameLen]
	// 去尾零
	n := len(nameBytes)
	for n > 0 && nameBytes[n-1] == 0 {
		n--
	}
	name = string(nameBytes[:n])
	inputRef = unpackSharedRef(r.data[off+toolFrameOffInput:])
	return
}

// SetReading 将 CALLING 帧原子转为 READING（插件开始执行）。
func (r *ToolCallRing) SetReading(frameIdx uint32) bool {
	return atomic.CompareAndSwapUint32(r.statePtr(frameIdx), toolFrameCalling, toolFrameReading)
}

// SetReady 将 READING 帧设为 READY 并写入 output 描述符（插件执行完毕）。
func (r *ToolCallRing) SetReady(frameIdx uint32, outputRef SharedRef) bool {
	if atomic.LoadUint32(r.statePtr(frameIdx)) != toolFrameReading {
		return false
	}
	off := r.frameOff(frameIdx)
	packSharedRef(r.data[off+toolFrameOffOutput:], outputRef)
	atomic.StoreUint32(r.statePtr(frameIdx), toolFrameReady)
	return true
}

// GetReadyFrame 读取帧的 requestID 和 output 描述符（内核消费 READY 帧）。
func (r *ToolCallRing) GetReadyFrame(frameIdx uint32) (reqID uint64, outputRef SharedRef) {
	off := r.frameOff(frameIdx)
	reqID = getU64(r.data[off+toolFrameOffReqID:])
	outputRef = unpackSharedRef(r.data[off+toolFrameOffOutput:])
	return
}

// ReleaseFrame 回收帧为 FREE（内核消费完毕后调用）。
func (r *ToolCallRing) ReleaseFrame(frameIdx uint32) {
	off := r.frameOff(frameIdx)
	// state 必须最后发布 FREE；否则并发 Reserve 可能在其余字段尚未清零时复用帧。
	for i := uint32(4); i < r.frameSize; i++ {
		r.data[off+i] = 0
	}
	atomic.StoreUint32(r.statePtr(frameIdx), toolFrameFree)
}

func (r *ToolCallRing) frameOff(idx uint32) uint32 {
	return r.framesBase + idx*r.frameSize
}

func (r *ToolCallRing) statePtr(idx uint32) *uint32 {
	off := r.framesBase + idx*r.frameSize + toolFrameOffState
	return (*uint32)(unsafe.Pointer(
		uintptr(unsafe.Pointer(&r.data[0])) + uintptr(off),
	))
}

func packSharedRef(b []byte, ref SharedRef) {
	putU32(b[0:], ref.Offset)
	putU32(b[4:], ref.Length)
	putU32(b[8:], ref.Generation)
	putU32(b[12:], ref.Flags)
}

func unpackSharedRef(b []byte) SharedRef {
	return SharedRef{
		Offset:     getU32(b[0:]),
		Length:     getU32(b[4:]),
		Generation: getU32(b[8:]),
		Flags:      getU32(b[12:]),
	}
}

package proc

import (
	"fmt"
	"log"
	"os"
	"sync"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	"golang.org/x/sys/unix"
)

// Host 持有**被全部子进程插件共享的一块 StageContext 段**，是共享内存数据面的
// 所有权中心（§3.3/§3.4）。
//
// ❗ 为什么必须共享一块段（这是一个容易走错的关键点）：
// 若每个插件各持一块段，则「内核 ctx → 段 → 插件改 → 回读 ctx」在多插件下退化成
// 副本模型——两个插件各写各的段、各自回读，最后回读者覆盖前者，
// lost update 原样复现（§8.4 实测 35.8~36.8%）。
// 实验 8 的做法是 5 个 worker 进程 mmap **同一个 memfd**，本实现与之一致。
//
// 生命周期：Host 由 registry 创建一次，随内核存活；每个插件 spawn 时经
// ExtraFiles 拿到同一 memfd（fd 3），mmap 后即看到同一份物理页。
type Host struct {
	memfd   *os.File
	data    []byte
	seg     *Segment
	shmSize int

	// locks 被全部插件的 coreHandler 共享——同阶段并发扇出的插件在此排队，
	// 语义等价于内置插件共享 *StageContext 的 sync.RWMutex（§0.2 第 1 条）。
	locks *lockRegistry

	// stageMu 串行化「整次 stage 执行」对共享段的独占。
	//
	// 必要性：内核可能在不同路径并发触发 RunStage（如 emitResponse 的
	// before_output 与主循环的其他阶段）。段只有一份，两次 stage 交叠会互相污染。
	// 由首个进入的插件加锁、最后离开的插件解锁；RunStage 的 wg.Wait() 保证
	// 每个 handler 的 defer 必然执行，故 inflight 必然归零，不会死锁。
	stageMu sync.Mutex

	coordMu sync.Mutex
	coord   *stageCoordinator
}

// NewHost 创建共享段（memfd + mmap + 布局初始化）。
//
// 用 memfd 而非 /dev/shm 文件：无需文件名、不残留（进程退出即回收）、
// 可经 ExtraFiles 传给子进程。实验 2 已验证父子 mmap 到不同虚拟地址时
// 相对偏移仍正确解引用。
func NewHost() (*Host, error) {
	fd, err := unix.MemfdCreate("hastagectx", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("proc: 创建共享段 memfd: %w", err)
	}
	if err := unix.Ftruncate(fd, int64(shmDefaultSize)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("proc: 共享段 ftruncate: %w", err)
	}
	data, err := unix.Mmap(fd, 0, shmDefaultSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("proc: 共享段 mmap: %w", err)
	}
	seg, err := NewSegment(data)
	if err != nil {
		unix.Munmap(data)
		unix.Close(fd)
		return nil, err
	}

	return &Host{
		memfd:   os.NewFile(uintptr(fd), "hastagectx"),
		data:    data,
		seg:     seg,
		shmSize: shmDefaultSize,
		locks:   &lockRegistry{},
	}, nil
}

// shmDefaultSize 是共享 StageContext 段的大小。
//
// 取 256KB：StageContext 全字段 JSON 化后典型 < 4KB（工具结果中位 93B，§2.5），
// append-only 中间垃圾由 stage 结束时 Compact 回收，256KB 给足余量。
// 全部插件共享一块，总开销恒定，不随插件数增长。
const shmDefaultSize = 256 * 1024

// Close 释放共享段。
func (h *Host) Close() error {
	if h.data != nil {
		unix.Munmap(h.data)
		h.data = nil
	}
	if h.memfd != nil {
		err := h.memfd.Close()
		h.memfd = nil
		return err
	}
	return nil
}

// beginStage 由插件 handler 进入时调用。
//
// 首个进入者：获取 stageMu（独占共享段）→ 把内核 StageContext 写入段。
// 后续进入者：仅递增 inflight。
func (h *Host) beginStage(sc *pubsdk.StageContext) (*stageCoordinator, error) {
	h.coordMu.Lock()
	first := h.coord == nil
	if first {
		// 独占共享段直到本次 stage 全部插件离开
		h.coordMu.Unlock()
		h.stageMu.Lock()
		h.coordMu.Lock()
		// 双检：等锁期间可能已有其他插件建好协调器（它们会先拿到 stageMu）
		if h.coord != nil {
			first = false
			h.stageMu.Unlock()
		} else {
			h.coord = newStageCoordinator(h.seg)
		}
	}
	coord := h.coord
	h.coordMu.Unlock()

	if err := coord.enter(sc, first); err != nil {
		if first {
			h.coordMu.Lock()
			h.coord = nil
			h.coordMu.Unlock()
			h.stageMu.Unlock()
		}
		return nil, err
	}
	h.locks.bind(coord.lock)
	return coord, nil
}

// endStage 由插件 handler 返回时调用。
// 最后离开者：把共享段结果读回内核 StageContext → 压实 arena → 释放 stageMu。
func (h *Host) endStage(coord *stageCoordinator) error {
	last, err := coord.leave()
	if !last {
		return err
	}
	h.coordMu.Lock()
	h.coord = nil
	h.coordMu.Unlock()
	h.stageMu.Unlock()
	return err
}

// ForceReleaseLock 在插件进程崩溃时释放其可能持有的 stage 锁（实验 9 自愈机制）。
func (h *Host) ForceReleaseLock(plugin string) bool {
	return h.locks.forceRelease(plugin)
}

// Segment 暴露共享段（供诊断与测试）。
func (h *Host) Segment() *Segment { return h.seg }

// stageCoordinator 跟踪一次 stage 执行中参与插件的进出。
type stageCoordinator struct {
	seg  *Segment
	lock *stageLock

	mu       sync.Mutex
	inflight int
	written  bool
	ctxRef   *pubsdk.StageContext
}

func newStageCoordinator(seg *Segment) *stageCoordinator {
	return &stageCoordinator{seg: seg, lock: newStageLock()}
}

// enter 登记一个插件进入本次 stage；first 为真时把内核状态写入共享段。
func (c *stageCoordinator) enter(sc *pubsdk.StageContext, first bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight++
	if !first || c.written {
		return nil
	}
	c.ctxRef = sc
	if err := c.seg.WriteAll(sc); err != nil {
		c.inflight--
		return fmt.Errorf("写入共享段: %w", err)
	}
	c.written = true
	return nil
}

// leave 登记一个插件离开；返回是否为最后一个离开者。
//
// 最后离开者负责把共享段结果读回内核 StageContext，并压实 arena
// （此时无插件持锁，满足 §3.3 的压实前提）。
func (c *stageCoordinator) leave() (last bool, err error) {
	c.mu.Lock()
	c.inflight--
	last = c.inflight == 0
	sc := c.ctxRef
	written := c.written
	c.mu.Unlock()

	if !last || !written || sc == nil {
		return last, nil
	}
	if rErr := c.seg.ReadInto(sc); rErr != nil {
		return last, fmt.Errorf("回读共享段: %w", rErr)
	}
	if reclaimed := c.seg.Compact(); reclaimed > 0 {
		log.Printf("[proc] stage 结束，arena 压实回收 %d 字节", reclaimed)
	}
	return last, nil
}

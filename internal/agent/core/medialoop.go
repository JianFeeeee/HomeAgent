package core

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
)

// 媒体与记忆块的生命周期辅助。
//
// 媒体不单独做生命周期管理（没有 GC、没有引用计数）：blob 是记忆块的内容，
// 块的创建/迁移/删除由记忆系统本身决定，块被永久删除时内容随之删除。
// 图片不靠文本描述索引——它只按自己的统一空间向量被检索。

// heldMediaDigests 汇总三层记忆当前持有的媒体 digest 集合。
//
// CAS 是全库字节存储，它的检索结果不等于「记忆里的媒体」——
// 召回前用它把已无处可归的内容过滤掉。
func (a *Agent) heldMediaDigests() map[string]bool {
	held := map[string]bool{}
	collect := func(blocks []memory.MemoryBlock) {
		for _, b := range blocks {
			if b.PayloadDigest != "" {
				held[b.PayloadDigest] = true
			}
		}
	}
	if a.context != nil {
		collect(a.context.Blocks())
	}
	if a.docStore != nil {
		collect(a.docStore.Blocks())
	}
	if a.memory != nil {
		if blocks, err := a.memory.MemoryBlocks(); err == nil {
			collect(blocks)
		}
	}
	return held
}

// payloadHeld 报告某个 digest 是否仍被三层记忆中的一等块持有。
// 这是删除前的一次活查询（不是持久化账本）：同一份字节可能同时被多个块共享。
func (a *Agent) payloadHeld(digest string) bool {
	if digest == "" {
		return false
	}
	if a.context != nil {
		for _, b := range a.context.Blocks() {
			if b.PayloadDigest == digest {
				return true
			}
		}
	}
	if a.docStore != nil {
		for _, b := range a.docStore.Blocks() {
			if b.PayloadDigest == digest {
				return true
			}
		}
	}
	if a.memory != nil {
		if blocks, err := a.memory.MemoryBlocks(); err == nil {
			for _, b := range blocks {
				if b.PayloadDigest == digest {
					return true
				}
			}
		}
	}
	return false
}

// forgetPayloads 在记忆块被永久删除后删除它们的内容。
//
// 与文本块一致：删除块即删除内容。只有确认没有任何存活块仍共享该 digest
// 时才删字节（同一张图可能被多个块引用）。
func (a *Agent) forgetPayloads(digests []string) {
	if a.mediaStore == nil {
		return
	}
	for _, d := range digests {
		if d == "" || a.payloadHeld(d) {
			continue
		}
		if err := a.mediaStore.Delete(d); err != nil {
			log.Printf("[media] 删除内容失败 %s: %v", shortDigest(d), err)
		}
	}
}

// reembedStaleMedia 在启动时批量迁移历史媒体向量到当前向量空间。
//
// 触发场景（任一变化都会导致旧向量无法参与查询）：
//   - 切换模型（模型 A→模型 B，fp 变了）
//   - 切换向量维度（ONNX→HTTP dim 512→1024）
//   - 首次部署嵌入服务（历史无向量的媒体补算）
//   - 嵌入服务离线后重新上线（失败条目 vec_model 仍为空）
//
// 并发策略：启动时用 worker pool 并行迁移，避免上千张图片串行耗时过长。
// 并发数在 ONNX 内嵌路径下不超 CPU 核心数（避免 ONNX 并发限流），
// 外部 API 路径下不超 8（避免打爆外部服务）。
func (a *Agent) reembedStaleMedia() {
	if a.multimodalSpace == nil || a.mediaStore == nil {
		return
	}
	fp := a.multimodalSpace.Fingerprint()
	digests, err := a.mediaStore.StaleVecDigestsAll(fp)
	if err != nil {
		log.Printf("[media] 查询需重算向量的媒体失败: %v", err)
		return
	}
	if len(digests) == 0 {
		log.Printf("[media] 无需迁移向量（所有媒体已与当前空间对齐 fp=%s）", shortFP(fp))
		return
	}

	// 并发度：ONNX 内嵌不超过 4，外部 API 不超过 8（由配置或实际环境动态定）
	workers := 4
	if fp[:min(4, len(fp))] == "http:" {
		workers = 8
	}
	log.Printf("[media] 启动向量迁移: %d 条 → 新空间 fp=%s dim=%d workers=%d",
		len(digests), shortFP(fp), a.multimodalSpace.Dim(), workers)

	jobs := make(chan string, workers*2)
	var done, failed, unsupported int64
	var failedMu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				switch err := a.reembedOne(d, fp); {
				case err == nil:
					atomic.AddInt64(&done, 1)
				case errors.Is(err, vector.ErrModalityUnsupported):
					// 该模态不在本空间内（如音频）：不重试、不计失败，
					// 也不拿另一个模型的向量顶替。
					atomic.AddInt64(&unsupported, 1)
				default:
					failedMu.Lock()
					failed++
					failedMu.Unlock()
				}
			}
		}()
	}

	for i, d := range digests {
		jobs <- d
		// 每迁移 20 条输出进度日志，让用户看到迁移在推进
		if (i+1)%20 == 0 {
			log.Printf("[media] 向量迁移进度: %d/%d (done=%d failed=%d)", i+1, len(digests), atomic.LoadInt64(&done), failed)
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("[media] 向量迁移完成: 成功=%d 失败=%d 不在本空间=%d 总计=%d fp=%s",
		done, failed, unsupported, len(digests), shortFP(fp))
}

// reembedOne 为单条媒体重新计算向量并写入（stat/get 失败时跳过该条目）。
//
// 模态不在本空间覆盖范围时返回 ErrModalityUnsupported，调用方据此区分
// 「永久无向量」与「本次失败重试」。
func (a *Agent) reembedOne(digest, fp string) error {
	it, err := a.mediaStore.Stat(digest)
	if err != nil {
		return err
	}
	data, err := a.mediaStore.Get(digest)
	if err != nil {
		return err
	}
	mime := it.MIME
	if mime == "" {
		mime = "image/png"
	}
	vec, err := a.multimodalSpace.EmbedImageDense(data, mime)
	if err != nil {
		return err
	}
	return a.mediaStore.SetVec(digest, vec, fp)
}

// shortFP 截断 fingerprint 为可读日志格式。
func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

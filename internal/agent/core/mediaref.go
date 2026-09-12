package core

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// blockSeq 保证块 ID 全局唯一（Graph memory_blocks 以 id 为主键）。
var blockSeq int64

func newBlockID() string {
	return fmt.Sprintf("blk_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&blockSeq, 1))
}

// blockModalityOf 把 CAS 媒体大类映射为一等记忆块模态。
func blockModalityOf(k media.Kind) memory.BlockModality {
	switch k {
	case media.KindImage:
		return memory.BlockImage
	case media.KindVideo:
		return memory.BlockVideo
	case media.KindAudio:
		return memory.BlockAudio
	default:
		return memory.BlockText
	}
}

// blockFromDigest 把一份已入库媒体变成一等记忆块。
// 块携带 digest/向量/fingerprint；CAS 只提供字节与元数据，不参与生命周期。
func (a *Agent) blockFromDigest(digest string) (memory.MemoryBlock, bool) {
	if a.mediaStore == nil || digest == "" {
		return memory.MemoryBlock{}, false
	}
	it, err := a.mediaStore.Stat(digest)
	if err != nil || it == nil {
		return memory.MemoryBlock{}, false
	}
	return memory.MemoryBlock{
		ID:            newBlockID(),
		Modality:      blockModalityOf(it.Kind),
		PayloadDigest: it.Digest,
		MIME:          it.MIME,
		Size:          it.Size,
		Width:         it.Width,
		Height:        it.Height,
		Vector:        it.Vec,
		Fingerprint:   it.VecModel,
		Tool:          it.Tool,
		CreatedAt:     it.FirstSeen,
	}, true
}

// 媒体记忆接线：把对话里出现的图片/音频落进内容寻址存储（CAS），
// 并让 L0 的 ContextEvent 直接持有一等记忆块。
//
// 媒体进入对话有两条路：用户直接发图（ContentBlock data URL）、插件注入
// （SetToolBlocks）。两条都在这里收口：从 data URL 取出字节存进 CAS，
// 用其向量构造一等记忆块挂到当轮 ContextEvent 上；事件被 Prune 时
// 块随之迁移到 L2 文档。
//
// 不再生成任何描述文本，也不再往正文写 media marker：图片只按自己的
// 统一空间向量被检索，描述式索引是将就方案。

// captureBlockMedia 把 blocks 里的 data URL 媒体落进 CAS，返回 digest 列表。
//
// 只处理 data URL：http(s) URL 拿不到字节就无法做内容寻址，
// 而"下载它再存"会把一次对话变成一次网络请求（超时、鉴权、SSRF 全来了），
// 不在本层解决。
func (a *Agent) captureBlockMedia(blocks []agentAPI.ContentBlock, tool string) []string {
	if a.mediaStore == nil || len(blocks) == 0 {
		return nil
	}

	var digests []string
	for _, b := range blocks {
		var url string
		switch {
		case b.ImageURL != nil && b.ImageURL.URL != "":
			url = b.ImageURL.URL
		case b.AudioURL != nil && b.AudioURL.URL != "":
			url = b.AudioURL.URL
		default:
			continue
		}

		mime, data, ok := media.ParseDataURL(url)
		if !ok {
			continue // http(s) URL 或格式不认，跳过
		}

		d, err := a.mediaStore.Put(data, media.Item{
			MIME: mime,
			Tool: tool,
		})
		if err != nil {
			// 媒体存不进去不该让对话失败——它是记忆增强，不是对话必需品
			log.Printf("[media] 落盘失败 (tool=%s mime=%s): %v", tool, mime, err)
			continue
		}

		// 入库即算一次多模态坐标并缓存（多模态空间可用时）。
		// 之后 doc_query / memory_recall / 内部召回直接复用 SetVec 的缓存坐标，
		// 不重复跑 ONNX；模型切换由启动时的 reembedStaleMedia 补算。
		a.embedMediaOnIngest(d, mime, data)
		digests = append(digests, d)
	}
	return digests
}

// embedMediaOnIngest 给刚入库的图片立即计算多模态坐标并缓存。
// 只在 多模态空间可用且为图像时执行；音频/未配置时静默跳过（保持既有行为）。
func (a *Agent) embedMediaOnIngest(digest, mime string, data []byte) {
	if a.multimodalSpace == nil || !a.multimodalSpace.Loaded() {
		return
	}
	if !strings.HasPrefix(mime, "image/") {
		return
	}
	vec, err := a.multimodalSpace.EmbedImageDense(data, mime)
	if err != nil {
		log.Printf("[media] 入库嵌入失败 %s: %v", shortDigest(digest), err)
		return
	}
	if err := a.mediaStore.SetVec(digest, vec, a.multimodalSpace.Fingerprint()); err != nil {
		log.Printf("[media] 入库写向量失败 %s: %v", shortDigest(digest), err)
	}
}

// stageMediaDigests 累积本轮捕获的 digest，等 ContextEvent 建好后一起挂上。
//
// 为何要缓存而不是当场建块：媒体在 process() 执行期间被捕获，而承载它的
// ContextEvent 要等 process() 返回后才 Append——此刻还没有 owner_id。
// 与既有的 a.pendingMedia 同一手法（都在 a.mu 保护下）。
func (a *Agent) stageMediaDigests(digests ...string) {
	if len(digests) == 0 {
		return
	}
	a.pendingMediaDigests = append(a.pendingMediaDigests, digests...)
}

// drainMediaDigests 取出并清空本轮累积的 digest。
func (a *Agent) drainMediaDigests() []string {
	if len(a.pendingMediaDigests) == 0 {
		return nil
	}
	out := a.pendingMediaDigests
	a.pendingMediaDigests = nil
	return out
}

// bindEventMedia 把本轮捕获的媒体变成一等记忆块，直接挂到 ContextEvent 上。
//
// 块存储在事件自身（随 context.json 持久化），不再写 media_refs：
// 存活与否由“三层记忆块是否持有这个 digest”决定，不维护引用账本。
func (a *Agent) bindEventMedia(evt *ContextEvent, digests []string) {
	if a.mediaStore == nil || evt == nil || len(digests) == 0 {
		return
	}
	if evt.ID == "" {
		evt.ID = newEventID()
	}
	for _, d := range digests {
		b, ok := a.blockFromDigest(d)
		if !ok {
			log.Printf("[media] 块构造失败 (%s)", shortDigest(d))
			continue
		}
		evt.Blocks = append(evt.Blocks, b)
	}
}

// mediaLabel 渲染一行媒体标签，供提示词告知"这条记忆带着哪份媒体"。
//
// 不再包含任何生成的描述文本：图片只按自己的向量被检索，标签仅提供
// MIME 与短 digest，让模型知道有这份媒体、可据 digest 取回字节。
// 查不到返回空串：内容可能已被删除，不该造出一条指向虚无的标签。
func mediaLabel(it *media.Item) string {
	if it == nil {
		return ""
	}
	label := string(it.Kind)
	if it.MIME != "" {
		label = it.MIME
	}
	return fmt.Sprintf("[%s %s]", label, shortDigest(it.Digest))
}

// newEventID 生成 ContextEvent 的稳定标识。
//
// 沿用 document.Store 的 doc_<unixnano> 手法（同一份代码库里保持一致，
// 也避免为此引入 uuid 依赖）。纳秒精度足够：同一 Agent 的事件由
// a.mu 串行化 Append，不存在同纳秒两条。
func newEventID() string {
	return fmt.Sprintf("evt_%d", time.Now().UnixNano())
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

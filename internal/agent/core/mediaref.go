package core

import (
	"fmt"
	"log"
	"strings"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// 媒体记忆接线：把对话里出现的图片/音频落进内容寻址存储（CAS），
// 并让 L0 的 ContextEvent 记住它们的 digest。
//
// 为何需要这一层：媒体进入对话有两条路，两条都只把**文字**留给记忆——
//
//  1. 用户直接发图 → processInput/resolveInput → mediaToBlocks
//     ContextEvent.Input 只存 alt 文本（"[从 qq 收到了 image]"），
//     base64 随 message 数组发给模型后就丢了。
//  2. 插件注入 → SetToolBlocks → process.go 的 mediaMsg
//     ToolResultItem.Output 只存那句 "[已将图片注入后续对话] /tmp/x.png"。
//
// 于是下一轮对话起，模型能看到的只有一句路径或一句 alt。那个文件被删、
// 被覆盖，或者本来就是 /tmp 下的临时产物，连线索都断了。
//
// 现在两条路都在同一处收口：从 ContentBlock 的 data URL 取出字节存进 CAS，
// digest 挂到当轮 ContextEvent 上；事件被 Prune 归档进 L2 时引用随之转移。

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
// 为何要缓存而不是当场 AddRef：媒体在 process() 执行期间被捕获，而承载它的
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

// bindEventMedia 把 digest 列表登记到某个 ContextEvent 上。
//
// 双向落地：evt.Media 让事件自己记得引了哪些媒体（随 context.json 持久化），
// media_refs 表让 CAS 侧知道谁在引用（GC 据此判断能不能清）。
// 两边都写才闭环——只写一边的话，要么 GC 会误删仍被记忆引用的内容，
// 要么孤儿永远清不掉。
func (a *Agent) bindEventMedia(evt *ContextEvent, digests []string) {
	if a.mediaStore == nil || evt == nil || len(digests) == 0 {
		return
	}
	if evt.ID == "" {
		evt.ID = newEventID()
	}
	for _, d := range digests {
		if err := a.mediaStore.AddRef(d, media.OwnerContext, evt.ID); err != nil {
			log.Printf("[media] AddRef 失败 (%s → %s): %v", shortDigest(d), evt.ID, err)
			continue
		}
		evt.Media = append(evt.Media, d)
	}
}

// mediaSummaryForEvent 给已有描述的媒体生成一行文字，供写进 ContextEvent.Input。
//
// 这是方案 C 的落点：**描述文本才是持久语义记忆，blob 只是缓存**。
// blob 可能被容量 GC 淘汰，但描述会一直留在 L0/L2/L3 的文本里，
// 让"那张紫蓝红三色带图"在几个月后仍然可被检索到。
func (a *Agent) mediaSummaryForEvent(digests []string) string {
	if a.mediaStore == nil || len(digests) == 0 {
		return ""
	}
	var lines []string
	for _, d := range digests {
		if line := a.mediaMarkerLine(d); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "媒体内容：\n" + strings.Join(lines, "\n")
}

// mediaMarkerLine 为一份媒体生成一行标记文本 `[<mime> <短digest>] <描述>`。
//
// 这是媒体标记格式的唯一生成处。此前 mediaSummaryForEvent 与
// mediaContextForSentences 各拼一份，改动截断长度或分隔符时只改一处，
// 另一处写出的标记就再也解析不回来——而解析失败是静默的（引用挂不上）。
//
// 查不到返回空串：媒体可能已被容量 GC 淘汰，此时不该造出一条指向虚无的标记。
func (a *Agent) mediaMarkerLine(digest string) string {
	if a.mediaStore == nil {
		return ""
	}
	it, err := a.mediaStore.Stat(digest)
	if err != nil || it == nil {
		return ""
	}
	label := string(it.Kind)
	if it.MIME != "" {
		label = it.MIME
	}
	desc := it.Description
	if desc == "" {
		// 「已入库但还没描述」与「压根没有媒体」必须可区分：描述由后台循环
		// 异步补齐，占位符保证补齐前这份媒体也不会从文本里消失。
		desc = "(未描述)"
	}
	return fmt.Sprintf("[%s %s] %s", label, shortDigest(digest), desc)
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

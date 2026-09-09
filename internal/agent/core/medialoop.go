package core

import (
	"log"
	"runtime/debug"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
)

// 媒体记忆的两条后台循环。
//
// mediaGCLoop     清理无人引用的 blob，让容量上限真正生效。
// mediaDescribeLoop 给未描述的媒体生成文字描述（方案 C 的另一半）。
//
// 为何描述要走后台而不是入库时同步做：视觉模型一次调用在生产实测 9.6s
// （see_video 6 帧批量 23s）。放在对话路径上会让每张图都给回复加十几秒，
// 而描述的价值是**几个月后还能检索到这张图**，不是这一轮对话——
// 这一轮模型本来就直接看着图。

const (
	// mediaDescribeBatch 是单轮描述的媒体条数上限。
	//
	// 取 4：既有回退链的 modalFallbackMaxBlocks 是 6（一次请求最多带 6 个媒体），
	// 这里留出余量，且每条单独请求以便逐条落库——批量描述拿回来一整段文字
	// 无法可靠切分回各自的 digest。
	mediaDescribeBatch = 4

	// mediaDescribeMinInterval 是两轮描述之间的最小间隔。
	//
	// 描述是纯后台的锦上添花，不该跟对话抢视觉模型配额。取 30s 让它
	// 慢慢消化积压，而不是一上线就把几百条历史媒体全打过去。
	mediaDescribeMinInterval = 30 * time.Second
)

// mediaGCLoop 周期清理无引用的媒体内容。
//
// 不做这件事的后果：容量上限形同虚设。CAS 的 GC 只在被显式调用时执行，
// 而 Put 路径不触发它——一次 see_video 抽 10 帧，帧本身没人引用（工具
// 结果被 Prune 掉之后），若无人清理就会一直堆在磁盘上。
func (a *Agent) mediaGCLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] mediaGCLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.mediaGCLoop()
		}
	}()
	if a.mediaStore == nil || a.mediaGCInterval <= 0 {
		return
	}

	ticker := time.NewTicker(a.mediaGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			removed, freed, err := a.mediaStore.GC(a.mediaGCMinAge)
			if err != nil {
				log.Printf("[media] GC 失败: %v", err)
				continue
			}
			if removed > 0 {
				st := a.mediaStore.Stats()
				log.Printf("[media] GC 清理 %d 条（释放 %d 字节），剩余 %v 条 / %v 字节",
					removed, freed, st["count"], st["total_bytes"])
			}
		case <-a.ctx.Done():
			return
		}
	}
}

// mediaDescribeLoop 给未描述的媒体补文字描述。
//
// 描述文本才是持久语义记忆：blob 会被容量 GC 淘汰，而描述留在 media 表里，
// 并经 mediaSummaryForEvent 写进 L0 事件、随归档进 L2 文档、经蒸馏进 L3 图库。
// 于是「那张紫蓝红三色带图」在原始字节早已被清掉之后仍然可被检索到。
func (a *Agent) mediaDescribeLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] mediaDescribeLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.mediaDescribeLoop()
		}
	}()
	if a.mediaStore == nil || !a.mediaDescribe {
		return
	}

	ticker := time.NewTicker(mediaDescribeMinInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.describePendingMedia()
		case <-a.ctx.Done():
			return
		}
	}
}

// describePendingMedia 取一批未描述的媒体逐条描述。
//
// 逐条而非批量：批量拿回来是一整段文字，无法可靠切分回各自的 digest
// （模型未必按序号输出，也可能把两张图合并成一句）。宁可多几次往返
// 也要保证「描述 ↔ digest」的对应关系是确定的。
func (a *Agent) describePendingMedia() {
	pending, err := a.mediaStore.Pending(mediaDescribeBatch)
	if err != nil {
		log.Printf("[media] 取待描述项失败: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	for _, it := range pending {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		kind := "image"
		if it.Kind == media.KindAudio {
			kind = "audio"
		} else if it.Kind != media.KindImage {
			// 视频帧以 image 入库；其余大类没有可用的描述通道，
			// 标记成"不可描述"以免每轮都被 Pending 取出来重试。
			if err := a.mediaStore.Describe(it.Digest, "", "unsupported"); err != nil {
				log.Printf("[media] 标记不可描述失败 %s: %v", shortDigest(it.Digest), err)
			}
			continue
		}

		p, srcName := a.resolveModalFallback(kind)
		if p == nil {
			// 没有声明该模态能力的源——这一轮整体跳过，不逐条重试。
			// 配置好之后自然会被下一轮捡起来。
			log.Printf("[media] 无可用的 %s 描述源，跳过本轮（%d 条待描述）", kind, len(pending))
			return
		}

		data, err := a.mediaStore.Get(it.Digest)
		if err != nil {
			// blob 已被 GC 清掉但元数据还在（GC 会同删，此处属异常路径）：
			// 标记一下避免死循环。
			log.Printf("[media] 读内容失败 %s: %v", shortDigest(it.Digest), err)
			if e := a.mediaStore.Describe(it.Digest, "", "content-missing"); e != nil {
				log.Printf("[media] 标记内容缺失失败 %s: %v", shortDigest(it.Digest), e)
			}
			continue
		}

		mime := it.MIME
		if mime == "" {
			mime = "image/png"
		}
		url := media.DataURL(mime, data)

		desc, err := a.chatModalFallbackBatch(p, kind, []string{url}, []string{"high"})
		if err != nil {
			// 失败不标记：可能是网络抖动或配额，下一轮该重试。
			log.Printf("[media] 描述失败 %s (源=%s): %v", shortDigest(it.Digest), srcName, err)
			continue
		}
		if desc == "" {
			// 空回复通常意味着上游把媒体剥离了——与 modalfallback 里的判断
			// 同一个道理，视作失败而非"没什么可说的"。
			log.Printf("[media] 描述为空 %s (源=%s)，视作失败", shortDigest(it.Digest), srcName)
			continue
		}

		if err := a.mediaStore.Describe(it.Digest, desc, srcName); err != nil {
			log.Printf("[media] 写描述失败 %s: %v", shortDigest(it.Digest), err)
			continue
		}
		log.Printf("[media] 已描述 %s (%s, %d 字, 源=%s)", shortDigest(it.Digest), kind, len([]rune(desc)), srcName)

		// 描述成功后，若 CLIP 嵌入器可用且是图片，计算视觉向量。
		// 这是"描述 + 向量"两步同步完成的路径；对于历史已有描述但无向量的媒体，
		// 由启动时的 reembedStaleMedia 补算。
		if a.clipEmb != nil && kind == "image" && it.Kind == media.KindImage {
			vec, err := a.clipEmb.EmbedImageDense(data, mime)
			if err != nil {
				log.Printf("[media] 视觉嵌入失败 %s: %v", shortDigest(it.Digest), err)
			} else if err := a.mediaStore.SetVec(it.Digest, vec, a.clipEmb.Fingerprint()); err != nil {
				log.Printf("[media] 写向量失败 %s: %v", shortDigest(it.Digest), err)
			} else {
				log.Printf("[media] 已嵌入 %s (dim=%d)", shortDigest(it.Digest), len(vec))
			}
		}
	}
}

// reembedStaleMedia 在启动时为历史已有描述但无 CLIP 向量的图片补算视觉向量。
// 避免安装 CLIP 后，旧图片永远只有描述文本、没有视觉向量，直到下次 Describe 才能写入。
func (a *Agent) reembedStaleMedia() {
	if a.clipEmb == nil || a.mediaStore == nil {
		return
	}
	fp := a.clipEmb.Fingerprint()
	digests, err := a.mediaStore.StaleVecDigests(fp)
	if err != nil {
		log.Printf("[media] 查询需重算向量的媒体失败: %v", err)
		return
	}
	if len(digests) == 0 {
		log.Printf("[media] 无历史媒体需要补算视觉向量")
		return
	}
	log.Printf("[media] 启动补算视觉向量: %d 条 (fp=%s...)", len(digests), fp[:min(12, len(fp))])
	done := 0
	for _, d := range digests {
		it, err := a.mediaStore.Stat(d)
		if err != nil {
			continue
		}
		data, err := a.mediaStore.Get(d)
		if err != nil {
			continue
		}
		mime := it.MIME
		if mime == "" {
			mime = "image/png"
		}
		vec, err := a.clipEmb.EmbedImageDense(data, mime)
		if err != nil {
			log.Printf("[media] 启动补算失败 %s: %v", shortDigest(d), err)
			continue
		}
		if err := a.mediaStore.SetVec(d, vec, fp); err != nil {
			log.Printf("[media] 启动写入向量失败 %s: %v", shortDigest(d), err)
			continue
		}
		done++
	}
	log.Printf("[media] 启动补算完成: %d/%d", done, len(digests))
}

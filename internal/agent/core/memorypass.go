package core

import (
	"log"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
)

// sceneKeysFor 推导本轮输入的**当前场景**。
//
// 场景是“这场面正在发生”的机器可读描述，用于把带条件的记忆（规则/约定）
// 取回来。优先级：
//  1. 注入点显式声明（payload.scene）——插件最清楚自己在什么场面里
//  2. 通道（evt.Source → chan:qq）
//  3. 工具（tool:qq_get_message）——工具输出触发的召回只知道这一步
//
// 多个场景是**并列命中**（取回任一场景的记忆），不是交集：
// 「在 QQ 上」与「刚取回消息正文」是两个都能独立成立的触发条件。
func sceneKeysFor(evt *agentIO.InputEvent, toolName string) []string {
	var keys []string
	seen := make(map[string]bool)
	add := func(k string) {
		// 显式声明的场景键来自插件，大小写/空白/标点都不可控；归一化后再去重，
		// 否则「chan:QQ」与「chan:qq」会变成两个场景，各自只召回一半记忆。
		k = memory.NormalizeSceneKey(k)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}

	if evt != nil && evt.Payload != nil {
		switch v := evt.Payload["scene"].(type) {
		case string:
			add(v)
		case []string:
			for _, s := range v {
				add(s)
			}
		case []interface{}:
			for _, item := range v {
				if s, ok := item.(string); ok {
					add(s)
				}
			}
		}
	}

	if evt != nil {
		add(memory.ChannelScene(evt.Source))
	}
	if toolName != "" {
		add(memory.ToolScene(toolName))
	}
	return keys
}

// memoryPassOut 是一次记忆操作（取进来 / 踢出去）的结果。
type memoryPassOut struct {
	// Archived 是被归档进文档记忆的低相关 L0 事件数（prune 的输出）。
	Archived int
	// RecallText 是可注入 prompt 的记忆索引文本（recall 的输出，空串表示无）。
	RecallText string
}

// memoryPass 是「取进来（召回）」与「踢出去（裁剪）」的**唯一入口**。
//
// prune 与 recall 是两根正交的声明轴（默认值刻意相反：裁剪是破坏性的、
// 默认关；召回是只读增量、默认开），但两者都建立在**同一份清洗后的 query**
// 之上。调用点只负责解析声明，这里统一做三件各写一遍就会写歪的事：
//
//  1. 同一 query：prune 与 recall 用同一个查询向量来源，避免「裁错事件、
//     召回错记忆」——原始内容里的 ANSI/base64/JSON 噪声会把相关性打分带偏。
//  2. 同一次预算：召回文本按 token 预算截断只做一次（见 recallText）。
//  3. 同一条审计：谁（trigger）据什么触发了哪种操作都落一条日志，
//     否则又是一个「幕后发生、查不出是谁」的机制（对齐 prune 的设计初衷）。
//
// 边界：prune 与 recall 目前仍走**各自的相关性空间**（prune 用 L0 事件的
// 稠密/词向量给已有事件打分，recall 用图 + TF-IDF 实体索引）。真正的
// 「一次打分」要先统一打分空间（后续步骤）；这里统一的是**入口、query、
// 预算与审计**——这已是「一个过程」的可审计外壳，剩下的差在打分空间。
func (a *Agent) memoryPass(query, trigger string, prune, recall bool, scenes []string) memoryPassOut {
	var out memoryPassOut
	if a == nil || (!prune && !recall) {
		return out
	}
	if prune {
		out.Archived = a.pruneByQuery(query)
	}
	if recall && query != "" {
		out.RecallText = a.recallTextFor(query, trigger, scenes)
	}
	if out.Archived > 0 || out.RecallText != "" {
		log.Printf("[agent] memory pass (%s): archived=%d recalled=%d chars",
			trigger, out.Archived, len(out.RecallText))
	}
	return out
}

// pruneByQuery 按相关性把低相关 L0 事件归档进文档记忆，返回归档数。
//
// **不做声明判定**——声明已由调用方（pruneOnInput / stepToolAfter）解析，
// 这里只负责执行。放在 memoryPass 内部是为了让裁剪与召回共享入口。
func (a *Agent) pruneByQuery(query string) int {
	if a == nil || a.context == nil {
		return 0
	}
	// **动态上下文**是父 agent 专属能力：轻量内核（驻留子）用传统上下文，
	// 不做按相关度的裁剪与向 doc 记忆的归档（子也没有 doc 记忆）。
	if a.isLightKernel() {
		return 0
	}
	topK := a.maxContextSize - 1
	if topK < 1 {
		topK = 1
	}
	return a.context.Prune(query, topK, a.docStore)
}

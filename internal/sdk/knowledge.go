package sdk

import (
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// KnowledgeAPI 是内置插件使用的全量知识库接口。
//
// 为什么不把多模态加进 pubsdk.KnowledgeAPI：那会改动
// third_party/homeagent-sdk/sdk/ 的公开契约，而发布纪律要求
// `git diff main -- third_party/homeagent-sdk/sdk/` 恒为 0
// （改动它等于一次 major bump + 完整的外部发版流程）。
// internal/sdk 明确不受此约束（"内部实现自由"），且这里本来就是
// "内核侧接口 = 公共接口 + 内置插件额外能力" 的既有范式
// （见 PluginSDK 遮蔽访问器：Settings/Memory/TextMemory/DocMemory…）。
// 结果是：内置插件（含子进程插件的 core handler）拿到多模态能力，
// 而外部 SDK 契约保持逐字不变。
type KnowledgeAPI interface {
	pubsdk.KnowledgeAPI
	// Stats 返回知识库的运行统计。
	Stats() map[string]interface{}
	// Remove 按名称删除一条知识。
	Remove(name string) error

	// SearchIn 在某个分类子树内检索（category 为空 = 全库）。
	SearchIn(query, category string, topK int) ([]*Knowledge, error)
	// AddWithMedia 写入带媒体的知识。媒体是一等节点：其向量会与正文向量
	// 在多模态统一空间内融合，使该条目能按图本身被召回。
	//
	// 未接入多模态空间时与 Add 等价（媒体仍被记录，只是不参与召回）。
	AddWithMedia(name, content string, media []KnowledgeMediaRef) error
	// AttachMedia 给已有知识追加媒体，并当场重算其稠密向量。
	AttachMedia(name string, media ...KnowledgeMediaRef) error
	// ReindexDense 重建稠密向量（模型/维度变化后调用），返回新建与跳过条数。
	ReindexDense() (built, skipped int)
	// DenseStats 报告稠密路的接线与覆盖情况。
	DenseStats() map[string]interface{}
}

// KnowledgeMediaRef 是媒体在知识条目中的一等引用。
//
// 与内核 knowledge.KnowledgeMediaRef 是**类型别名**而非新类型：别名
// 才能穿过 C ABI / JSON 边界；若是两种结构，core handler 还得再做一次
// 手工转换，漏一处就是「媒体被静默丢弃」。
type KnowledgeMediaRef = knowledge.KnowledgeMediaRef

// Knowledge 沿用公共 SDK 的类型，保证内外两侧对同一批知识条目的
// 字段理解一致（跨 ABI 传递时按此结构序列化）。
type Knowledge = pubsdk.Knowledge

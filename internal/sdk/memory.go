package sdk

import pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"

// MemoryAPI 是内置插件使用的全量图记忆接口。
type MemoryAPI interface {
	pubsdk.MemoryAPI
	// GraphData 返回整个知识图谱的完整快照。
	GraphData() (map[string]interface{}, error)
}

type Entity = pubsdk.Entity
type Relation = pubsdk.Relation
type Triple = pubsdk.Triple

// TextMemoryAPI 是内置插件使用的全量文本记忆接口。
type TextMemoryAPI interface {
	pubsdk.TextMemoryAPI
	// RecentEvents 返回最近 n 条记忆事件。
	RecentEvents(n int) ([]TextEvent, error)
	// Stats 返回文本记忆的运行统计。
	Stats() map[string]interface{}
}

type TextEvent = pubsdk.TextEvent

// MediaAttachment 是记忆附件（媒体）在插件边界上的表示。
// 与公共 SDK 同一类型，内置插件与外部插件用同一套字段。
type MediaAttachment = pubsdk.MediaAttachment

type DocMemoryAPI = pubsdk.DocMemoryAPI
type Doc = pubsdk.Doc

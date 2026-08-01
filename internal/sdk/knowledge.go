package sdk

import pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

// KnowledgeAPI 是内置插件使用的全量知识库接口。
type KnowledgeAPI interface {
	pubsdk.KnowledgeAPI
	// Stats 返回知识库的运行统计。
	Stats() map[string]interface{}
	// Remove 按名称删除一条知识。
	Remove(name string) error
}

type Knowledge = pubsdk.Knowledge

package sdk

import "gitcode.com/JianFeeeee/HomeAgent/internal/events"

// 事件类型/事件类型别名：插件经 SDK 订阅/发布内核事件，
// 无需直接 import internal/events（内核事件总线仅通过 SDK 暴露）。

type EventType = events.EventType
type Event = events.Event

const (
	EventRawInput       = events.EventRawInput
	EventAgentOutput    = events.EventAgentOutput
	EventAgentLLMChain  = events.EventAgentLLMChain
	EventToolCall       = events.EventToolCall
	EventReasoning      = events.EventReasoning
	EventStage          = events.EventStage
	EventSystem         = events.EventSystem
	EventTerminalOutput = events.EventTerminalOutput
	EventAll            = events.EventAll
)

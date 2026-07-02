// Package onebot 实现 OneBot V11 标准协议（Reverse WebSocket 通信）
// 参考: https://github.com/botuniverse/onebot-11
package onebot

import "fmt"

// Action 是 OneBot 标准 API 请求
type Action struct {
	Action string                 `json:"action"`
	Params map[string]interface{} `json:"params,omitempty"`
	Echo   string                 `json:"echo,omitempty"`
}

// ActionResponse 是 OneBot 标准 API 响应
type ActionResponse struct {
	Status  string      `json:"status"`
	RetCode int         `json:"retcode"`
	Data    interface{} `json:"data"`
	Echo    string      `json:"echo,omitempty"`
}

// Event 是 OneBot 推送的事件
type Event struct {
	Time       int64  `json:"time"`
	SelfID     int64  `json:"self_id"`
	PostType   string `json:"post_type"` // message, notice, request, meta_event
	DetailType string `json:"-"`

	// 消息事件字段
	MessageType string      `json:"message_type,omitempty"` // private, group
	SubType     string      `json:"sub_type,omitempty"`
	MessageID   int64       `json:"message_id,omitempty"`
	UserID      int64       `json:"user_id,omitempty"`
	GroupID     int64       `json:"group_id,omitempty"`
	Message     interface{} `json:"message,omitempty"`   // string 或 []MessageSegment
	RawMessage  string      `json:"raw_message,omitempty"`
	Font        int         `json:"font,omitempty"`
	Sender      *Sender     `json:"sender,omitempty"`

	// 通知事件字段
	NoticeType string `json:"notice_type,omitempty"`

	// 请求事件字段
	RequestType string `json:"request_type,omitempty"`
	Flag        string `json:"flag,omitempty"`
	Comment     string `json:"comment,omitempty"`

	// 元事件字段
	MetaEventType string `json:"meta_event_type,omitempty"`
	Interval      int64  `json:"interval,omitempty"`
	Status        *Status `json:"status,omitempty"`
}

type Sender struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Sex      string `json:"sex,omitempty"`
	Age      int    `json:"age,omitempty"`
	Card     string `json:"card,omitempty"` // 群名片
	Area     string `json:"area,omitempty"`
	Level    string `json:"level,omitempty"`
	Role     string `json:"role,omitempty"` // owner, admin, member
	Title    string `json:"title,omitempty"`
}

type Status struct {
	AppInitialized bool `json:"app_initialized"`
	AppEnabled     bool `json:"app_enabled"`
	PluginsGood    bool `json:"plugins_good"`
	AppGood        bool `json:"app_good"`
	Online         bool `json:"online"`
	Good           bool `json:"good"`
}

// MessageSegment 表示 OneBot 消息段（数组格式）
type MessageSegment struct {
	Type string            `json:"type"`
	Data map[string]string `json:"data"`
}

// MessageText 快速构造纯文本消息段
func MessageText(text string) MessageSegment {
	return MessageSegment{Type: "text", Data: map[string]string{"text": text}}
}

// MessageImage 构造图片消息段
func MessageImage(file string) MessageSegment {
	return MessageSegment{Type: "image", Data: map[string]string{"file": file}}
}

// MessageAt 构造 @ 消息段
func MessageAt(userID int64) MessageSegment {
	return MessageSegment{Type: "at", Data: map[string]string{"qq": fmt.Sprintf("%d", userID)}}
}

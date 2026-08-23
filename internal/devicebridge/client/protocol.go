// Package client 提供设备桥客户端共享库，实现与 remotedevice 插件通信的完整协议。
// 编译为 C 共享库后，GUI (Electron) 可通过 FFI 调用；CLI (waiter) 可直接导入 Go 包。
package client

import "encoding/json"

// ===== 消息类型（与 remotedevice plugin 协议对齐） =====

// HelloMsg 设备登记消息
type HelloMsg struct {
	Op     string     `json:"op"`
	Device DeviceMeta `json:"device"`
}

// DeviceMeta 设备元信息
type DeviceMeta struct {
	DeviceID string                 `json:"device_id"`
	Name     string                 `json:"name"`
	Kind     string                 `json:"kind"`
	Caps     []string               `json:"caps"`
	Info     map[string]interface{} `json:"info,omitempty"`
}

// BindMsg 设备绑定消息
type BindMsg struct {
	Op       string `json:"op"`
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

// CmdMsg 服务端下发的命令消息
type CmdMsg struct {
	Op      string `json:"op"`
	ReqID   string `json:"req_id"`
	Command string `json:"command"`
	CmdType string `json:"cmd_type"`
}

// CmdResult 命令执行结果
type CmdResult struct {
	Op       string `json:"op"`
	ReqID    string `json:"req_id"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

// DataStart 二进制数据传输开始（设备→网关）
type DataStart struct {
	Op        string `json:"op"`
	ReqID     string `json:"req_id"`
	Kind      string `json:"kind"`
	MIME      string `json:"mime"`
	Total     int    `json:"total"`
	ChunkSize int    `json:"chunk_size,omitempty"`
}

// DataEnd 二进制数据传输结束
type DataEnd struct {
	Op     string `json:"op"`
	ReqID  string `json:"req_id"`
	Status string `json:"status"`
	Total  int    `json:"total,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SpeechStart TTS 音频数据开始（网关→设备）
type SpeechStart struct {
	Op    string `json:"op"`
	ReqID string `json:"req_id"`
	Kind  string `json:"kind"`
	MIME  string `json:"mime"`
	Total int    `json:"total"`
}

// SpeechEnd TTS 音频数据结束
type SpeechEnd struct {
	Op    string `json:"op"`
	ReqID string `json:"req_id"`
}

// StatusMsg 设备状态上报
type StatusMsg struct {
	Op       string `json:"op"`
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
}

// EventMsg 设备主动上报事件
type EventMsg struct {
	Op       string `json:"op"`
	DeviceID string `json:"device_id,omitempty"`
	Type     string `json:"type"`
	Payload  interface{} `json:"payload,omitempty"`
}

// ===== 序列化辅助 =====

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
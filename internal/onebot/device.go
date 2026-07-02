package onebot

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// Device 将 OneBot 客户端包装为 IO 抽象层的 Device
// 作为 QQ 通道与 HomeAgent 之间的桥梁
type Device struct {
	name    string
	desc    string
	client  *Client
	iom     *agentIO.IOManager
}

// NewDevice 创建 OneBot IO 设备
// name: 设备名称（如 "qq"）
// wsURL: OneBot 前端 WebSocket 地址（如 "ws://127.0.0.1:6700"）
// accessToken: OneBot 鉴权令牌（可选）
func NewDevice(name, wsURL, accessToken string, iom *agentIO.IOManager) *Device {
	return &Device{
		name:   name,
		desc:   fmt.Sprintf("OneBot 标准 QQ 通道 (%s)", wsURL),
		client: NewClient(wsURL, accessToken),
		iom:    iom,
	}
}

// Name 返回设备名称
func (d *Device) Name() string { return d.name }

// Type 返回设备类型（双向 IO）
func (d *Device) Type() agentIO.DeviceType { return agentIO.DeviceIO }

// Description 返回设备描述
func (d *Device) Description() string { return d.desc }

// OutputCapabilities 返回输出能力（文本+文件+图片）
func (d *Device) OutputCapabilities() agentIO.OutputCapability {
	return agentIO.CapText | agentIO.CapFile | agentIO.CapImage
}

// Tools 返回 OneBot 标准 API 的工具定义
// AI 可以通过这些工具调用 OneBot 功能
func (d *Device) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{
		{
			Name:        d.name + "_send_private_msg",
			Description: "发送 QQ 私聊消息",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id":    map[string]interface{}{"type": "integer", "description": "目标 QQ 号"},
					"message":    map[string]interface{}{"type": "string", "description": "消息内容（支持 CQ 码，如 [CQ:image,file=xxx.jpg]）"},
					"auto_escape": map[string]interface{}{"type": "boolean", "description": "是否作为纯文本发送（不解析 CQ 码）"},
				},
				"required": []string{"user_id", "message"},
			},
		},
		{
			Name:        d.name + "_send_group_msg",
			Description: "发送 QQ 群消息",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"group_id":   map[string]interface{}{"type": "integer", "description": "目标群号"},
					"message":    map[string]interface{}{"type": "string", "description": "消息内容（支持 CQ 码）"},
					"auto_escape": map[string]interface{}{"type": "boolean", "description": "是否作为纯文本发送"},
				},
				"required": []string{"group_id", "message"},
			},
		},
		{
			Name:        d.name + "_get_group_member_info",
			Description: "获取 QQ 群成员信息",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"group_id": map[string]interface{}{"type": "integer", "description": "群号"},
					"user_id":  map[string]interface{}{"type": "integer", "description": "QQ 号"},
				},
				"required": []string{"group_id", "user_id"},
			},
		},
		{
			Name:        d.name + "_get_group_list",
			Description: "获取 QQ 群列表",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}
}

// Execute 执行 OneBot 工具调用
func (d *Device) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	if !d.client.Connected() {
		return nil, fmt.Errorf("onebot 未连接到前端")
	}

	// 去掉名称前缀以匹配方法名
	method := strings.TrimPrefix(tool, d.name+"_")

	switch method {
	case "send_private_msg":
		userID, _ := toInt64(args["user_id"])
		message, _ := args["message"].(string)
		autoEscape, _ := args["auto_escape"].(bool)
		return d.client.SendPrivateMessage(userID, message, autoEscape)

	case "send_group_msg":
		groupID, _ := toInt64(args["group_id"])
		message, _ := args["message"].(string)
		autoEscape, _ := args["auto_escape"].(bool)
		return d.client.SendGroupMessage(groupID, message, autoEscape)

	case "get_group_member_info":
		groupID, _ := toInt64(args["group_id"])
		userID, _ := toInt64(args["user_id"])
		return d.client.GetGroupMemberInfo(groupID, userID)

	case "get_group_list":
		return d.client.GetGroupList()

	default:
		return nil, fmt.Errorf("unknown onebot tool: %s", tool)
	}
}

// Start 连接到 OneBot 前端
func (d *Device) Start() error {
	// 非阻塞连接
	go func() {
		if err := d.client.Connect(); err != nil {
			log.Printf("[onebot] %s initial connect failed, will retry: %v", d.name, err)
		}
	}()

	// 注册事件处理：OneBot 事件 → IO InputEvent
	d.client.SetEventHandler(func(evt *Event) {
		d.handleEvent(evt)
	})

	return nil
}

// Stop 断开连接
func (d *Device) Stop() error {
	return d.client.Close()
}

// handleEvent 将 OneBot 事件转换为 IO InputEvent
func (d *Device) handleEvent(evt *Event) {
	if d.iom == nil {
		return
	}

	switch evt.PostType {
	case "message":
		var text string
		if evt.RawMessage != "" {
			text = evt.RawMessage
		} else if s, ok := evt.Message.(string); ok {
			text = s
		}

		if text == "" {
			return
		}

		// 构造输入源标识
		source := d.name
		payload := map[string]interface{}{
			"content":  text,
			"source":   source,
			"user_id":  evt.UserID,
			"sender":   evt.Sender,
		}
		if evt.MessageType == "group" {
			payload["group_id"] = evt.GroupID
			payload["label"] = fmt.Sprintf("group:%d:%d", evt.GroupID, evt.UserID)
		} else {
			payload["label"] = fmt.Sprintf("private:%d", evt.UserID)
		}

		d.iom.InjectTextTo(source, d.name, text)

	case "notice":
		log.Printf("[onebot] notice from %s: type=%s", d.name, evt.NoticeType)

	case "request":
		log.Printf("[onebot] request from %s: type=%s flag=%s", d.name, evt.RequestType, evt.Flag)

	case "meta_event":
		if evt.MetaEventType == "heartbeat" {
			log.Printf("[onebot] %s heartbeat: online=%v", d.name, evt.Status != nil && evt.Status.Online)
		}
	}
}

func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

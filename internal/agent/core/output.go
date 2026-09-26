package core

import (
	"encoding/json"
	"fmt"
	"strings"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func (a *Agent) executeOutputSendTool(tc agentAPI.ToolCall) string {
	channel := strings.TrimPrefix(tc.Name, "output_send__")
	payload, _ := tc.Arguments["payload"].(string)
	rawType, _ := tc.Arguments["type"].(string)
	if channel == "" || payload == "" {
		return "工具名称格式: output_send__{channel}，payload 不能为空"
	}
	// type 缺省按 text 处理：绝大多数输出就是文本，让模型为"省略一个默认值"付一次
	// 失败重试没有意义（判据该拦的是"不知道发什么"，不是"没写众所周知的默认值"）。
	if rawType == "" {
		rawType = "text"
	}
	// 授权闸（纵深防御）：模型可能凭名字直接调未授权的输出门。
	if !a.IsOutputAllowed(channel) {
		return fmt.Sprintf("通道 [%s] 未授权给本 agent。可用通道见 output_list_channels", channel)
	}

	meta, _ := tc.Arguments["meta"].(string)

	caps := a.io.GetChannelCapabilities(channel)
	if caps == 0 {
		return fmt.Sprintf("通道 [%s] 不存在或不可用。可用输出工具列表见 output_list_channels", channel)
	}
	switch rawType {
	case "text":
		if !caps.Supports(agentIO.CapText) {
			return fmt.Sprintf("通道 [%s] 不支持文本输出（能力: %s）", channel, caps.String())
		}
	case "voice", "audio":
		if !caps.Supports(agentIO.CapAudio) {
			return fmt.Sprintf("通道 [%s] 不支持语音输出（能力: %s）", channel, caps.String())
		}
	case "image":
		if !caps.Supports(agentIO.CapImage) {
			return fmt.Sprintf("通道 [%s] 不支持图片输出（能力: %s）", channel, caps.String())
		}
	case "file":
		if !caps.Supports(agentIO.CapFile) {
			return fmt.Sprintf("通道 [%s] 不支持文件输出（能力: %s）", channel, caps.String())
		}
	}

	args := map[string]interface{}{
		"payload": payload,
		"type":    rawType,
	}
	if meta != "" {
		args["meta"] = meta
	}

	stageCtx := &sdk.StageContext{
		FinalText: payload,
		Phase:     sdk.StageBeforeOutput,
	}
	a.runStage(sdk.StageBeforeOutput, stageCtx)
	if stageCtx.Response != nil {
		return fmt.Sprintf("输出被插件拦截: %s", *stageCtx.Response)
	}
	if stageCtx.FinalText == "" {
		return "输出被插件清空"
	}
	args["payload"] = stageCtx.FinalText

	if dev := a.io.GetDevice(channel); dev != nil {
		result, err := dev.Execute("output", args)
		if err != nil {
			return fmt.Sprintf("通过 [%s] 通道发送失败: %v", channel, err)
		}
		// 通道可能回报「未确认」（已提交但超时未拿到发送确认）——此时不能对模型
		// 谎报「已发送」，否则模型不会重试/核实（plan.md 11.1）。
		if m, ok := result.(map[string]interface{}); ok {
			if status, _ := m["status"].(string); status == "unconfirmed" || status == "queued" {
				note, _ := m["note"].(string)
				if note == "" {
					note = "发送已提交但未收到通道确认，结果未知"
				}
				return fmt.Sprintf("[%s] 通道发送结果未确认：%s", channel, note)
			}
		}
		// 成功回执：只返回极简标记，不回传完整插件响应。
		// 「已通过 [qq] 通道发送: map[status:sent message_id:xxx]」这类富回执
		// 会驱动模型继续调用 output_send（回声效应），是 output loop 的根源之一。
		return "ok"
	}

	a.io.EmitTextTo("agent_io", channel, payload)
	return fmt.Sprintf("已通过 [%s] 通道发送", channel)
}

func (a *Agent) executeOutputSendHelp(tc agentAPI.ToolCall) string {
	suffix := strings.TrimPrefix(tc.Name, "output_send__")
	channel := strings.TrimSuffix(suffix, "_help")
	if channel == "" {
		return "工具名称格式: output_send__{channel}_help"
	}

	dev := a.io.GetDevice(channel)
	if dev == nil {
		return fmt.Sprintf("通道 [%s] 不存在", channel)
	}

	caps := a.io.GetChannelCapabilities(channel)
	capStr := "无"
	if caps != 0 {
		capStr = caps.String()
	}

	desc := dev.Description()
	if desc == "" {
		desc = channel + " 输出通道"
	}

	return fmt.Sprintf(`通道 [%s]
描述: %s
能力: %s

【参数说明】
payload — 消息载荷（必填）。type=text 时直接填文字，type=file/image 时填 URL 或路径
meta    — JSON 对象，发送所需的元数据（可选，取决于通道是否需要路由信息）
type    — 载荷类型（必填），枚举值见下方

【type 枚举】
- text    — 文本消息
- voice   — 语音消息
- image   — 图片
- file    — 文件

【meta JSON 格式】
由通道描述定义，通常包含：
- "group_id"  群号（群聊时必填）
- "user_id"   目标用户 QQ 号（私聊时必填）
- "reply_to"  回复某条消息 ID（可选）

示例: output_send__%s(payload="你好", meta="{\"group_id\": 123456789}", type="text")`, channel, desc, capStr, channel)
}

func (a *Agent) executeOutputListChannels() string {
	channels := a.io.ListChannels()
	if len(channels) == 0 {
		return "没有可用通道"
	}
	var parts []string
	parts = append(parts, "可用通道:")
	for _, ch := range channels {
		if ch.OutputCaps == 0 {
			continue
		}
		if !a.IsOutputAllowed(ch.Name) {
			continue
		}
		line := fmt.Sprintf("  - %s: [%s] %s", ch.Name, ch.OutputCaps.String(), ch.Description)
		if t, ok := a.ResolveOutputTarget(ch.Name); ok {
			line += fmt.Sprintf("（目标: %s / inputch %s）", orDash(t.AgentID), orDash(t.InputCh))
		}
		parts = append(parts, line)
		for _, t := range ch.Tools {
			parts = append(parts, fmt.Sprintf("     工具: %s - %s", t.Name, t.Description))
		}
	}
	return strings.Join(parts, "\n")
}

// autoFillOutputMeta 在 meta 缺收件人时，从**本轮输入事件**自动补。
//
// 为何需要（线上实测 2026-09-26 17:57）：用户从 QQ 私聊发来消息，agent
// 生成了回复也调了 output_send__qq，但没填 meta：
//
//	17:57:45  qq_get_message → {user_id: 2198972886, message_type: private}
//	17:57:46  output_send__qq → 失败：meta 中需要 group_id 或 user_id
//	17:58:14  output_send__qq_help → 查格式
//	17:58:14  output_send__qq → ok      ← 靠重试成功，耗了 74s
//
// 信息内核本来就有（输入事件里带着 user_id/group_id），却要模型从
// qq_get_message 的返回里手抄一遍。抄错就失败，失败才去查 _help。
// 而"回复"这件事的收件人是确定的（= 消息来源），本不该由模型负责。
//
// 边界（都刻意收窄，宁可不补也不能补错）：
//   - 显式传了 meta ⇒ 原样返回。主动 DM 别人等场景必须保持原行为。
//   - meta 里有 group_id/user_id ⇒ 不覆盖。
//   - meta 是坏 JSON ⇒ 原样返回。让下游报"格式错"，而不是被静默替换
//     成一个模型没要求过的收件人（那比报错更坏：消息会发给错的人）。
//   - 非 qq 通道（如 webui）⇒ 不补。webui 走 ResponseCh，不过 output_send。
//   - 输入事件里没��件人信息 ⇒ 留空，让下游按原逻辑报"需要 user_id"。
//     宁可报错让模型重试，也不要编一个收件人。
func autoFillOutputMeta(meta, channel string, evt *agentIO.InputEvent) string {
	if meta != "" {
		// 已有内容：合法且已含收件人就不动；坏 JSON 也原样返回（见函数注释）
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(meta), &m); err != nil {
			return meta
		}
		if _, ok := m["group_id"]; ok {
			return meta
		}
		if _, ok := m["user_id"]; ok {
			return meta
		}
		// meta 存在但没收件人：补进去
		if id := recipientFromEvent(channel, evt); id != nil {
			m = mergeMeta(m, id)
			if out, err := json.Marshal(m); err == nil {
				return string(out)
			}
		}
		return meta
	}
	// meta 完全没传：能确定收件人才补
	if id := recipientFromEvent(channel, evt); id != nil {
		if out, err := json.Marshal(map[string]interface{}{
			id.key: id.val,
		}); err == nil {
			return string(out)
		}
	}
	return ""
}

// metaField 是从输入事件里提取出的收件人字段。
type metaField struct {
	key string // "user_id" 或 "group_id"
	val string
}

// recipientFromEvent 从输入事件推导收件人。
//
// 只处理 qq 通道：它是唯一一个"meta 必填收件人"的异步通道，且收件人
// 与消息来源一一对应。其它通道（wechat 等）不猜 —— 猜错等于发错人。
func recipientFromEvent(channel string, evt *agentIO.InputEvent) *metaField {
	if evt == nil || channel != "qq" {
		return nil
	}
	if evt.Payload == nil {
		return nil
	}
	// 群消息优先：群里的 user_id 是**发送者**，用它当收件人会发错人。
	// group_id 存在且非 "0" 时才是群聊（QQ 私聊时 group_id 会出现为 0）。
	if gid := payloadString(evt.Payload["group_id"]); gid != "" && gid != "0" {
		return &metaField{key: "group_id", val: gid}
	}
	if uid := payloadString(evt.Payload["user_id"]); uid != "" {
		return &metaField{key: "user_id", val: uid}
	}
	return nil
}

func mergeMeta(m map[string]interface{}, f *metaField) map[string]interface{} {
	if m == nil {
		m = map[string]interface{}{}
	}
	m[f.key] = f.val
	return m
}

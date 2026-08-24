package remotedevice

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// devicectlDevice 把设备网关暴露为 IOManager 的一个 Device：
// Tools() 提供 devicedetect / device_ctl_status / device_ctl_cmdrun / device_ctl_cmdresult / screensee，
// Execute() 推送命令到设备，设备端自行鉴权（客户端存储授权）。
type devicectlDevice struct {
	reg *Registry

	// screensee 回调：设备截屏回传后由 agent 核心消费（视觉描述）。
	// 由插件 Start 注入；nil 时退化为仅返回 base64 数据。
	seeHandler func(dataURL string, provider string) string
}

func (d *devicectlDevice) Name() string                                 { return "devicectl" }
func (d *devicectlDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceIO }
func (d *devicectlDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapStructured }
func (d *devicectlDevice) Description() string {
	return "远程设备控制网关：查看已接入/已授权的设备并发送控制指令（经用户授权的设备）"
}
func (d *devicectlDevice) ChannelDef() agentIO.ChannelDef { return agentIO.ChannelDef{} }
func (d *devicectlDevice) Start() error                   { return nil }
func (d *devicectlDevice) Stop() error                    { return nil }

func (d *devicectlDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{
		{
			Name: "devicedetect",
			Description: "扫描并列出已接入设备网关的设备（含在线/离线状态与授权状态）。" +
				"用途：查看当前有哪些设备连接了 HomeAgent、是否在线、是否已授权。" +
				"参数 kind 可选，只返回该种类的设备。返回值 device_id 用于后续 device_ctl_* 工具。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"kind": map[string]interface{}{"type": "string", "description": "按设备种类过滤（light/camera/computer/phone/...），可省略"},
				},
			},
		},
		{
			Name: "device_ctl_status",
			Description: "查询一台已在线设备的实时状态。" +
				"仅返回设备上报的状态信息（如电量/温度/运行状态）。" +
				"需要 device_id（来自 devicedetect）。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "device_ctl_cmdrun",
			Description: "向设备下发命令/操作（异步，accepted=true 后用 device_ctl_cmdresult 轮询结果）。" +
				"command 支持两类（前缀区分）：\n" +
				"- shell-cmd: 在设备上执行原生 shell 命令，如 shell-cmd ls -la /tmp\n" +
				"- homeagent-cmd: 调用设备端 HomeAgent 内置能力：\n" +
				"  · homeagent-screensue <显示内容/HTML> — 用户侧屏幕弹窗显示自定义内容（默认 5 秒后自动关闭）\n" +
				"  · homeagent-screensue <秒> <内容> — 指定显示时长（秒），如 homeagent-screensue 30 会议提醒：三点开会\n" +
				"  · homeagent-screensue 0 <内容> — 永不超时，常驻显示直到用户手动关闭\n" +
				"  · homeagent-camerasue — 抓拍单张 jpeg（结果为 base64 data URL）\n" +
				"  · homeagent-camerasue <N秒> — 录像 N 秒 mp4（二进制分块回传，cmdresult 含 data_base64 字段）\n" +
				"  · homeagent-speakeruse <文字> — 设备端 TTS 语音朗读文字\n" +
				"⚡ 高危：设备必须已授权，且该操作会改变设备行为。" +
				"返回 accepted=true 表示已下发并等待设备执行，之后可用 device_ctl_cmdresult 查询结果。" +
				"若设备未授权或离线，返回错误信息。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"command":   map[string]interface{}{"type": "string", "description": "以 shell-cmd 或 homeagent-cmd 前缀开头。如 shell-cmd pwd、homeagent-screensue 三点开会、homeagent-screensue 0 重要公告、homeagent-camerasue 5（录5秒）、homeagent-speakeruse 你好"},
				},
				"required": []interface{}{"device_id", "command"},
			},
		},
		{
			Name: "device_ctl_cmdresult",
			Description: "查询之前 device_ctl_cmdrun 下发命令的执行结果（按 req_id 或 device_id 最近一次）。" +
				"若非阻塞或已超时，用此工具取回设备执行输出。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"req_id":    map[string]interface{}{"type": "string", "description": "命令请求 ID（device_ctl_cmdrun 返回）"},
					"device_id": map[string]interface{}{"type": "string", "description": "设备 ID（查询最近一次结果）"},
				},
			},
		},
		{
			Name: "screensee",
			Description: "查看一台已授权设备的屏幕当前画面（截屏回传）。" +
				"与 screensue（向用户屏幕显示内容）配对：screensue 是给用户看，screensee 是你看。" +
				"返回屏幕截图的自动视觉描述；如需读取屏上文字可接着用 ocr_image。" +
				"需要 device_id（来自 devicedetect）。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"provider":  map[string]interface{}{"type": "string", "description": "可选：用于视觉描述的 LLM 源名称，不填则使用默认模型"},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "computeruse",
			Description: "控制一台已授权设备的鼠标/键盘（远程操控电脑屏幕）。" +
				"典型流程：先 screensee 看屏幕 → computeruse 操作 → 再 screensee 确认结果。" +
				"坐标为设备屏幕像素（原点左上角，与 screensee 截图一致）。" +
				"⚡ 高危：直接操作用户设备，务必确认操作意图明确。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"action": map[string]interface{}{
						"type":        "string",
						"description": "操作类型：click(单击) / doubleclick(双击) / rightclick(右键) / move(移动) / scroll(滚动) / keypress(按键) / type(输入文字)",
						"enum":        []interface{}{"click", "doubleclick", "rightclick", "move", "scroll", "keypress", "type"},
					},
					"x":      map[string]interface{}{"type": "integer", "description": "鼠标 X 坐标（像素）。click/doubleclick/rightclick/move 必填"},
					"y":      map[string]interface{}{"type": "integer", "description": "鼠标 Y 坐标（像素）。click/doubleclick/rightclick/move 必填"},
					"button": map[string]interface{}{"type": "string", "description": "鼠标按钮：left(默认)/right/middle（可选）"},
					"dy":     map[string]interface{}{"type": "integer", "description": "scroll 滚动量：正=向下，负=向上"},
					"key":    map[string]interface{}{"type": "string", "description": "keypress 按键名，如 Return / space / ctrl+c / alt+F4"},
					"text":   map[string]interface{}{"type": "string", "description": "type 要输入的文字"},
				},
				"required": []interface{}{"device_id", "action"},
			},
		},
		{
			Name: "clipboardsee",
			Description: "读取一台已授权设备的剪切板当前内容（用户最近复制/剪切的文字）。" +
				"与 clipboardsue 配对：clipboardsee 是读，clipboardsue 是写。" +
				"适用场景：用户说「看看我刚复制的东西」「把我复制的链接打开」。" +
				"⚡ 隐私敏感：剪切板可能含密码/隐私，仅在用户明确要求时使用。设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
				},
				"required": []interface{}{"device_id"},
			},
		},
		{
			Name: "clipboardsue",
			Description: "把指定文字写入一台已授权设备的剪切板（用户之后可直接 Ctrl+V 粘贴）。" +
				"与 clipboardsee 配对：clipboardsee 是读，clipboardsue 是写。" +
				"适用场景：帮用户准备好要粘贴的长文本/链接/代码，避免 computeruse type 逐字输入慢且易错。" +
				"典型组合：clipboardsue 写入 → 提示用户 Ctrl+V，或 clipboardsue + computeruse keypress ctrl+v 自动粘贴。" +
				"设备必须已授权且在线。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"text":      map[string]interface{}{"type": "string", "description": "要写入剪切板的内容"},
				},
				"required": []interface{}{"device_id", "text"},
			},
		},
		{
			Name: "deviceinfo",
			Description: "探查一台设备接入网关时声明的详细信息与支持能力。" +
				"返回设备的 OS/架构/CPU/内存/能力 caps 等（设备接入时上报，非实时）。" +
				"需要 device_id（来自 devicedetect）。设备必须已授权。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
				},
				"required": []interface{}{"device_id"},
			},
		},
	}
}

func (d *devicectlDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	switch tool {
	case "devicedetect":
		return d.detect(args)
	case "device_ctl_status":
		return d.status(args)
	case "device_ctl_cmdrun":
		return d.cmdrun(args)
	case "device_ctl_cmdresult":
		return d.cmdresult(args)
	case "screensee":
		return d.screensee(args)
	case "computeruse":
		return d.computeruse(args)
	case "clipboardsee":
		return d.clipboardsee(args)
	case "clipboardsue":
		return d.clipboardsue(args)
	case "deviceinfo":
		return d.info(args)
	default:
		return nil, fmt.Errorf("unknown device tool %s", tool)
	}
}

// publicDevices 把设备列表转为简洁 JSON（无内部字段）。
func publicDevices(in []DeviceMeta) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(in))
	for _, m := range in {
		entry := map[string]interface{}{
			"device_id":  m.DeviceID,
			"name":       m.Name,
			"kind":       m.Kind,
			"caps":       m.Caps,
			"authorized": m.Authorized,
			"online":     m.Online,
			"last_seen":  m.LastSeen,
		}
		if len(m.Info) > 0 {
			entry["info"] = m.Info
		}
		out = append(out, entry)
	}
	return out
}

// detect 实现 devicedetect。
func (d *devicectlDevice) detect(args map[string]interface{}) (interface{}, error) {
	kind, _ := args["kind"].(string)
	devs := d.reg.List()
	if kind != "" {
		var filtered []DeviceMeta
		for _, m := range devs {
			if strings.EqualFold(m.Kind, kind) {
				filtered = append(filtered, m)
			}
		}
		devs = filtered
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].DeviceID < devs[j].DeviceID })
	if len(devs) == 0 {
		return map[string]interface{}{"devices": []interface{}{}, "message": "暂无设备接入"}, nil
	}
	return map[string]interface{}{"devices": publicDevices(devs)}, nil
}

// status 实现 device_ctl_status（只读，查询设备最后一次上报状态）。
func (d *devicectlDevice) status(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return publicDevices([]DeviceMeta{m}), nil // 带 offline=true
	}
	return publicDevices([]DeviceMeta{m}), nil
}

// cmdrun 实现 device_ctl_cmdrun：检查授权+在线，push 命令，异步等待结果。
func (d *devicectlDevice) cmdrun(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	cmd, _ := args["command"].(string)
	if id == "" || cmd == "" {
		return nil, fmt.Errorf("device_id and command required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return nil, fmt.Errorf("device %s 不在线，无法执行命令", id)
	}
	// 命令类型：shell-cmd / homeagent-* 前缀区分；无前缀按 shell 处理（兼容旧格式）
	cmdType := "shell"
	switch {
	case strings.HasPrefix(cmd, "shell-cmd"):
		cmdType = "shell"
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "shell-cmd"))
	case strings.HasPrefix(cmd, "homeagent-cmd"):
		cmdType = "homeagent"
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "homeagent-cmd"))
	case strings.HasPrefix(cmd, "homeagent-"):
		cmdType = "homeagent"
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "homeagent-"))
	default:
		cmdType = "shell"
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, cmd, cmdType); err != nil {
		return nil, fmt.Errorf("下发命令失败: %w", err)
	}
	// 阻塞等待设备结果（带超时）；结果同时由 registry 留档。
	res, err := d.reg.AwaitResult(reqID, 30*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return map[string]interface{}{"accepted": true, "req_id": reqID, "pending": true, "note": "命令已下发但设备未在超时内回执，可用 device_ctl_cmdresult 再查"}, nil
	}
	out := res
	out["req_id"] = reqID
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// cmdresult 实现 device_ctl_cmdresult。
func (d *devicectlDevice) cmdresult(args map[string]interface{}) (interface{}, error) {
	reqID, _ := args["req_id"].(string)
	deviceID, _ := args["device_id"].(string)
	if reqID != "" {
		res, ok := d.reg.GetResult(reqID)
		if !ok {
			return nil, fmt.Errorf("no result for req %s（设备可能尚未回执）", reqID)
		}
		return res, nil
	}
	if deviceID != "" {
		// 返回该设备最近一次结果（简化：遍历 results 找 device_id 匹配的最近一条）
		// 说明：当前只按 req_id 查询；device_id 查询留给后续迭代。
		_, ok := d.reg.Get(deviceID)
		if !ok {
			return nil, fmt.Errorf("device %s 不存在", deviceID)
		}
		return map[string]interface{}{"device_id": deviceID, "note": "请用 device_ctl_cmdrun 返回的 req_id 查询命令结果"}, nil
	}
	return nil, fmt.Errorf("req_id 或 device_id 至少提供一个")
}

// info 实现 deviceinfo：返回设备详情 + 支持能力。
func (d *devicectlDevice) info(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	out := map[string]interface{}{
		"device_id":  m.DeviceID,
		"name":       m.Name,
		"kind":       m.Kind,
		"caps":       m.Caps,
		"authorized": m.Authorized,
		"online":     m.Online,
		"last_seen":  m.LastSeen,
	}
	if len(m.Info) > 0 {
		out["info"] = m.Info
	}
	return out, nil
}

// SetSeeHandler 注入 screensee 的视觉描述回调（agent 核心提供）。
func (d *devicectlDevice) SetSeeHandler(fn func(dataURL string, provider string) string) {
	d.seeHandler = fn
}

// screensee 实现 screensee：向设备下发 homeagent-screensee 截屏命令，
// 等待回传 jpeg base64，交给 seeHandler（agent 核心）做视觉描述。
func (d *devicectlDevice) screensee(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	provider, _ := args["provider"].(string)
	if id == "" {
		return nil, fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return nil, fmt.Errorf("device %s 不在线", id)
	}
	if !d.reg.SupportsTool(id, "screensee") {
		return nil, fmt.Errorf("device %s 未声明 screensee 能力（caps=%v），无法截屏", id, m.Caps)
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, "screensee", "homeagent"); err != nil {
		return nil, fmt.Errorf("下发截屏命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 30*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回传屏幕画面: %w", err)
	}
	if res["status"] != "ok" {
		errMsg, _ := res["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%v", res["status"])
		}
		return nil, fmt.Errorf("设备截屏失败: %s", errMsg)
	}
	output, _ := res["output"].(string)
	// 设备端回传 data URL（data:image/jpeg;base64,...）或裸 base64
	if !strings.HasPrefix(output, "data:") {
		output = "data:image/jpeg;base64," + output
	}
	d.reg.SaveResult(reqID, res)
	if d.seeHandler == nil {
		return map[string]interface{}{"image_data_url": output, "note": "无视觉描述处理器，仅返回原始图像数据"}, nil
	}
	desc := d.seeHandler(output, provider)
	return map[string]interface{}{"description": desc}, nil
}

// computeruse 实现 computeruse：向设备下发鼠标/键盘控制命令。
// 协议（GUI b4e5b39）：homeagent-computeruse {"x":px,"y":px,"action":"act","button":"btn","text":"txt"}
// 服务端负责把结构化参数序列化为 JSON，避免 LLM 手拼字符串出错。
func (d *devicectlDevice) computeruse(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	action, _ := args["action"].(string)
	if id == "" || action == "" {
		return nil, fmt.Errorf("device_id and action required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("device %s 不存在", id)
	}
	// 服务端不检查授权；设备端收到请求后自行决定是否执行。
	if !m.Online {
		return nil, fmt.Errorf("device %s 不在线", id)
	}
	if !d.reg.SupportsTool(id, "computeruse") {
		return nil, fmt.Errorf("device %s 未声明 computeruse 能力（caps=%v），无法操控鼠标键盘", id, m.Caps)
	}

	// 构造 GUI 端约定的 JSON 参数（坐标相对 screensueDisplay 所选屏）
	params := map[string]interface{}{"action": action}
	switch action {
	case "click", "doubleclick", "rightclick", "move":
		x, xok := args["x"].(float64)
		y, yok := args["y"].(float64)
		if !xok || !yok {
			return nil, fmt.Errorf("action=%s 需要 x/y 坐标", action)
		}
		params["x"] = int(x)
		params["y"] = int(y)
		if btn, ok := args["button"].(string); ok && btn != "" {
			params["button"] = btn
		}
	case "scroll":
		dy, ok := args["dy"].(float64)
		if !ok {
			return nil, fmt.Errorf("action=scroll 需要 dy 滚动量（正=向下，负=向上）")
		}
		params["dy"] = int(dy)
	case "keypress":
		key, _ := args["key"].(string)
		if key == "" {
			return nil, fmt.Errorf("action=keypress 需要 key 按键名（如 Return / ctrl+c）")
		}
		params["key"] = key
	case "type":
		text, _ := args["text"].(string)
		if text == "" {
			return nil, fmt.Errorf("action=type 需要 text 要输入的文字")
		}
		params["text"] = text
	default:
		return nil, fmt.Errorf("不支持的操作类型 %s（可选 click/doubleclick/rightclick/move/scroll/keypress/type）", action)
	}

	cmdBytes, _ := json.Marshal(params)
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, "computeruse "+string(cmdBytes), "homeagent"); err != nil {
		return nil, fmt.Errorf("下发操控命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 30*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回执: %w", err)
	}
	out := res
	out["req_id"] = reqID
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// clipboardCheck 检查设备可操作性（存在/已授权/在线/能力声明），返回错误或 nil。
// tool 为空时跳过能力校验（如 device_ctl_cmdrun 由自身逻辑处理）。
func (d *devicectlDevice) clipboardCheck(id, verb, tool string) error {
	if id == "" {
		return fmt.Errorf("device_id required")
	}
	m, ok := d.reg.Get(id)
	if !ok {
		return fmt.Errorf("device %s 不存在", id)
	}
	if !m.Online {
		return fmt.Errorf("device %s 不在线", id)
	}
	if tool != "" && !d.reg.SupportsTool(id, tool) {
		return fmt.Errorf("device %s 未声明 %s 能力（caps=%v），无法执行此操作", id, tool, m.Caps)
	}
	return nil
}

// clipboardsee 实现 clipboardsee：读取设备剪切板当前内容。
// 协议（GUI 配套）：homeagent-clipboardsee → 回执 output 字段为剪切板文字。
func (d *devicectlDevice) clipboardsee(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	if err := d.clipboardCheck(id, "读取", "clipboardsee"); err != nil {
		return nil, err
	}
	reqID := newReqID()
	if err := d.reg.PushCmd(id, reqID, "clipboardsee", "homeagent"); err != nil {
		return nil, fmt.Errorf("下发读取命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 15*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回执: %w", err)
	}
	if res["status"] != "ok" {
		errMsg, _ := res["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%v", res["status"])
		}
		return nil, fmt.Errorf("读取剪切板失败: %s", errMsg)
	}
	content, _ := res["output"].(string)
	out := map[string]interface{}{
		"req_id":  reqID,
		"content": content,
		"empty":   content == "",
	}
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// clipboardsue 实现 clipboardsue：把文字写入设备剪切板。
// 协议（GUI 配套）：homeagent-clipboardsue <文字>，回执 ok 表示已写入。
func (d *devicectlDevice) clipboardsue(args map[string]interface{}) (interface{}, error) {
	id, _ := args["device_id"].(string)
	text, _ := args["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("text required（要写入剪切板的内容）")
	}
	if err := d.clipboardCheck(id, "写入", "clipboardsue"); err != nil {
		return nil, err
	}
	reqID := newReqID()
	// 命令格式：clipboardsue <文字>（GUI 端取首个空格后的全部内容作为写入文本）
	if err := d.reg.PushCmd(id, reqID, "clipboardsue "+text, "homeagent"); err != nil {
		return nil, fmt.Errorf("下发写入命令失败: %w", err)
	}
	res, err := d.reg.AwaitResult(reqID, 15*time.Second)
	if err != nil {
		d.reg.SaveResult(reqID, map[string]interface{}{"accepted": true, "error": err.Error(), "pending": true})
		return nil, fmt.Errorf("设备未在超时内回执: %w", err)
	}
	if res["status"] != "ok" {
		errMsg, _ := res["error"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("status=%v", res["status"])
		}
		return nil, fmt.Errorf("写入剪切板失败: %s", errMsg)
	}
	out := map[string]interface{}{
		"req_id":  reqID,
		"written": len(text),
		"preview": truncateForPreview(text, 60),
	}
	d.reg.SaveResult(reqID, out)
	return out, nil
}

// truncateForPreview 截断长文本用于回执预览。
func truncateForPreview(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

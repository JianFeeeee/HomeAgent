package remotedevice

import (
	"fmt"
	"sort"
	"strings"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// devicectlDevice 把设备网关暴露为 IOManager 的一个 Device：
// Tools() 提供 devicedetect / device_ctl_status / device_ctl_cmdrun / device_ctl_cmdresult，
// Execute() 检查授权并路由到 WS 在线设备。
type devicectlDevice struct {
	reg     *Registry
	persist func() // 授权变更后持久化
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
				"- homeagent-cmd: 调用设备端 HomeAgent 内置能力，如 homeagent-camerasue（调用用户侧摄像头）、" +
				"homeagent-screensue（用户侧屏幕显示内容）\n" +
				"⚡ 高危：设备必须已授权，且该操作会改变设备行为。" +
				"返回 accepted=true 表示已下发并等待设备执行，之后可用 device_ctl_cmdresult 查询结果。" +
				"若设备未授权或离线，返回错误信息。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{"type": "string", "description": "目标设备 ID"},
					"command":   map[string]interface{}{"type": "string", "description": "以 shell-cmd 或 homeagent-cmd 前缀开头。如 shell-cmd pwd、homeagent-camerasue"},
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
	if !m.Authorized {
		return nil, fmt.Errorf("device %s 未授权，无法查询状态（需先在设备管理页或经 bind 授权）", id)
	}
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
	if !m.Authorized {
		return nil, fmt.Errorf("device %s 未授权，无法执行命令（请先在设备管理页授权）", id)
	}
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
		m, ok := d.reg.Get(deviceID)
		if !ok {
			return nil, fmt.Errorf("device %s 不存在", deviceID)
		}
		if !m.Authorized {
			return nil, fmt.Errorf("device %s 未授权", deviceID)
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
	if !m.Authorized {
		return nil, fmt.Errorf("device %s 未授权，无法探查信息（请先在设备管理页授权）", id)
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

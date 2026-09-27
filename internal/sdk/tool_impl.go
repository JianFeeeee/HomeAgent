package sdk

import (
	"fmt"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// toolImpl 桥接 StageHost（插件工具）与 IOManager（设备/通道工具）。
type toolImpl struct {
	stageHost ToolSource
	iom       *agentIO.IOManager
}

func NewTool(sh ToolSource, iom *agentIO.IOManager) ToolAPI {
	return &toolImpl{stageHost: sh, iom: iom}
}

func (t *toolImpl) GetToolDefs() []ToolDef {
	if t.stageHost == nil {
		return nil
	}
	return t.stageHost.GetToolDefs()
}

func (t *toolImpl) GetAllTools() []ToolDef {
	if t.iom == nil {
		return nil
	}
	defs := t.iom.GetAllTools()
	out := make([]ToolDef, 0, len(defs))
	for _, d := range defs {
		// ⚠️ ParallelSafe 必须一并带出：它决定该工具能否被并发执行。
		// 此前这里漏了它 ⇒ 插件看到的设备工具一律"不可并发"，
		// 设备工具的并发声明等于对插件不可见。
		out = append(out, ToolDef{
			Name:         d.Name,
			Description:  d.Description,
			Parameters:   d.Parameters,
			ParallelSafe: d.ParallelSafe,
		})
	}
	return out
}

func (t *toolImpl) ExecuteTool(name string, args map[string]interface{}) (interface{}, error) {
	if t.stageHost != nil {
		if def := t.stageHost.ToolDef(name); def != nil {
			return t.stageHost.ExecuteTool(name, args)
		}
	}
	if t.iom != nil {
		return t.iom.ExecuteTool(name, args)
	}
	return nil, fmt.Errorf("tool %s not found", name)
}

var _ ToolAPI = (*toolImpl)(nil)

// ToolDefByName 按名字查任一来源的工具声明（StageHost 优先，再查 IO 设备）。
//
// 用途：插件在**运行前**判断目标工具是否存在、是否并发安全。工具是动态
// 注册的，"不存在"是常态（插件未加载/已卸载/崩溃），因此查不到一律返回
// nil 交由调用方按"不存在"处理——不得 panic。
func (t *toolImpl) ToolDefByName(name string) *ToolDef {
	if t == nil || name == "" {
		return nil
	}
	if t.stageHost != nil {
		if def := t.stageHost.ToolDef(name); def != nil {
			return def
		}
	}
	if t.iom != nil {
		if def, ok := t.iom.ToolDefOf(name); ok {
			return &ToolDef{
				Name:         def.Name,
				Description:  def.Description,
				Parameters:   def.Parameters,
				ParallelSafe: def.ParallelSafe,
			}
		}
	}
	return nil
}

// CanUse 报告「执行该工具是否被授权」（D4）。
//
// 实现要点：**必须与 core.executeToolCallInner 里的那道闸同源同语义**，
// 否则两条路径判定不同，本身就是漏洞。判据 core.TestCanUseAgreesWithInnerPath
// 逐例比对两条路径的结论。
//
// 判据只有一条：设备类工具按 `device/<id>` 查当前 agent 的 allowedOutputs。
//
// ⚠️ device_id 缺失时**放行**（fail-open）——这是内核现状
// （core 的 TestDeviceToolAuth_* 依赖它）。两处行为已由
// TestCanUseMatchesInnerFailOpenOnMissingDeviceID 钉住；若将来要改成
// fail-closed，**必须两处同时改**，否则两条路径不一致。
func (t *toolImpl) CanUse(toolName string, args map[string]interface{}) bool {
	if t == nil || t.iom == nil {
		return true // 拿不到设备视图 ⇒ 不拦（与内核无 io 时的行为一致）
	}
	// 非设备工具不受此闸影响：闸的作用域必须窄，否则会把所有工具锁死。
	if _, isDeviceTool := t.iom.DeviceOfTool(toolName); !isDeviceTool {
		return true
	}
	id, _ := args["device_id"].(string)
	if id == "" {
		return true // fail-open，与内核一致
	}
	return t.canUseDevice(id)
}

// canUseDevice 是**可注入**的授权查询。
//
// 为何不直接在 toolImpl 里调 Agent：toolImpl 在 internal/sdk 包，
// 而 IsOutputAllowed 是 core.*Agent 的方法（core 反向依赖 sdk，
// sdk 不能依赖 core）。故内核在装配时把查询函数注入进来。
var deviceAuthQuery func(deviceID string) bool

// SetDeviceAuthQuery 注入"设备是否已授权"的查询（由内核在装配时调用）。
//
// 传 nil 表示尚未注入 ⇒ CanUse 对设备工具**放行**（保持存量行为不变）。
func SetDeviceAuthQuery(fn func(deviceID string) bool) { deviceAuthQuery = fn }

func (t *toolImpl) canUseDevice(deviceID string) bool {
	if deviceAuthQuery == nil {
		return true
	}
	return deviceAuthQuery(deviceID)
}

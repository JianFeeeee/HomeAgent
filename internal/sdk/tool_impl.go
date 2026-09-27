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

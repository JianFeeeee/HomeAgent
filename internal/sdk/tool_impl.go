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
		out = append(out, ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
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

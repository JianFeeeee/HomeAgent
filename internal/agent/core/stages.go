package core

import (
	"fmt"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/plugin/sdk"
)

type StageHost struct {
	plugins  []*sdk.PluginAPI
	toolDefs []sdk.ToolDef
	tools    map[string]sdk.ToolHandler
}

func NewStageHost() *StageHost {
	return &StageHost{
		tools: make(map[string]sdk.ToolHandler),
	}
}

func (h *StageHost) RegisterPlugin(api *sdk.PluginAPI) {
	h.plugins = append(h.plugins, api)
	for name, handler := range api.Tools() {
		h.tools[name] = handler
		h.toolDefs = append(h.toolDefs, sdk.ToolDef{Name: name})
	}
}

// SyncFromRegistry 从插件注册表同步 SDK 插件
func (h *StageHost) SyncFromRegistry(reg *plugin.Registry) {
	if reg == nil {
		return
	}
	for _, td := range reg.GetAllSDKToolDefs() {
		h.toolDefs = append(h.toolDefs, td)
	}
}

func (h *StageHost) GetToolDefs() []sdk.ToolDef {
	return h.toolDefs
}

func (h *StageHost) ExecuteTool(name string, args map[string]interface{}) (interface{}, error) {
	if handler, ok := h.tools[name]; ok {
		return handler(args)
	}
	return nil, fmt.Errorf("tool %s not found in any plugin", name)
}

func (h *StageHost) RunStage(stage sdk.Stage, ctx *sdk.StageContext) {
	for _, p := range h.plugins {
		for _, handler := range p.StageHandlers(stage) {
			if err := handler(ctx); err != nil {
				return
			}
			if ctx.Response != nil {
				return
			}
		}
	}
}

func (h *StageHost) RunStageAll(stage sdk.Stage, ctx *sdk.StageContext) {
	for _, p := range h.plugins {
		for _, handler := range p.StageHandlers(stage) {
			handler(ctx)
		}
	}
}

func (h *StageHost) PluginCount() int {
	return len(h.plugins)
}

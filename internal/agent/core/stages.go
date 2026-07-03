package core

import (
	"fmt"
	"sync"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type StageHost struct {
	mu       sync.RWMutex
	toolDefs []sdk.ToolDef
	tools    map[string]sdk.ToolHandler
	stages   map[sdk.Stage][]sdk.StageHandler
}

func NewStageHost() *StageHost {
	return &StageHost{
		tools:  make(map[string]sdk.ToolHandler),
		stages: make(map[sdk.Stage][]sdk.StageHandler),
	}
}

func (h *StageHost) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.tools[name]; exists {
		return fmt.Errorf("tool %s already registered", name)
	}
	h.tools[name] = handler
	h.toolDefs = append(h.toolDefs, def)
	return nil
}

func (h *StageHost) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stages[stage] = append(h.stages[stage], handler)
}

func (h *StageHost) GetToolDefs() []sdk.ToolDef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	defs := make([]sdk.ToolDef, len(h.toolDefs))
	copy(defs, h.toolDefs)
	return defs
}

func (h *StageHost) ExecuteTool(name string, args map[string]interface{}) (interface{}, error) {
	h.mu.RLock()
	handler, ok := h.tools[name]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("tool %s not found in any plugin", name)
	}
	return handler(args)
}

// RunStage 并行调用同阶段所有注册的处理函数。
// 各 handler 共享 *StageContext，通过其内置 RWMutex 安全读写：
//   - 只读操作先调用 ctx.RLock() / defer ctx.RUnlock()
//   - 写操作（如设置 ctx.Response）先调用 ctx.Lock() / defer ctx.Unlock()
// 如果任意 handler 设置了 Response，后续 handler 可通过 ctx.IsResponded() 判断后提前返回。
func (h *StageHost) RunStage(stage sdk.Stage, ctx *sdk.StageContext) {
	h.mu.RLock()
	handlers := h.stages[stage]
	h.mu.RUnlock()
	if len(handlers) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, handler := range handlers {
		wg.Add(1)
		go func(fn sdk.StageHandler) {
			defer wg.Done()
			fn(ctx)
		}(handler)
	}
	wg.Wait()
}

func (h *StageHost) RunStageAll(stage sdk.Stage, ctx *sdk.StageContext) {
	h.RunStage(stage, ctx)
}

func (h *StageHost) ToolCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.tools)
}

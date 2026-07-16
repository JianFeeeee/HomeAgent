package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type StageHost struct {
	mu          sync.RWMutex
	toolDefs    []sdk.ToolDef
	tools       map[string]sdk.ToolHandler
	toolPlugins map[string]string
	stages      map[sdk.Stage][]sdk.StageHandler
}

func NewStageHost() *StageHost {
	return &StageHost{
		tools:       make(map[string]sdk.ToolHandler),
		toolPlugins: make(map[string]string),
		stages:      make(map[sdk.Stage][]sdk.StageHandler),
	}
}

func (h *StageHost) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.tools[name]; exists {
		return fmt.Errorf("tool %s already registered", name)
	}
	if def.Plugin == "" {
		def.Plugin = inferToolPlugin(name)
	}
	h.tools[name] = handler
	h.toolPlugins[name] = def.Plugin
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

func (h *StageHost) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[stage] tool %s handler panic: %v\n%s", name, r, debug.Stack())
			err = fmt.Errorf("tool %s handler panic: %v", name, r)
		}
	}()
	h.mu.RLock()
	handler, ok := h.tools[name]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("tool %s not found in any plugin", name)
	}
	if handler == nil {
		return nil, fmt.Errorf("tool %s has nil handler", name)
	}
	return handler(args)
}

func (h *StageHost) ToolPlugin(name string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.toolPlugins[name]
}

func (h *StageHost) UnregisterPluginTools(pluginName string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var keepDefs []sdk.ToolDef
	for _, def := range h.toolDefs {
		if def.Plugin == pluginName {
			delete(h.tools, def.Name)
			delete(h.toolPlugins, def.Name)
		} else {
			keepDefs = append(keepDefs, def)
		}
	}
	h.toolDefs = keepDefs
}

func inferToolPlugin(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '_' {
			return name[:i]
		}
	}
	return ""
}

// RunStage 并行调用同阶段所有注册的处理函数。
// 各 handler 共享 *StageContext，通过其内置 RWMutex 安全读写：
//   - 只读操作先调用 ctx.RLock() / defer ctx.RUnlock()
//   - 写操作（如设置 ctx.Response）先调用 ctx.Lock() / defer ctx.Unlock()
// 如果任意 handler 设置了 Response，后续 handler 可通过 ctx.IsResponded() 判断后提前返回。
// handler 返回的 error 会被收集到 ctx.Errors 中并记录日志，不会中断其他 handler 的执行。
func (h *StageHost) RunStage(stage sdk.Stage, ctx *sdk.StageContext) {
	h.mu.RLock()
	handlers := h.stages[stage]
	h.mu.RUnlock()
	if len(handlers) == 0 {
		return
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(handlers))
	for _, handler := range handlers {
		wg.Add(1)
		go func(fn sdk.StageHandler) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[stage] handler panic: %v", r)
				}
			}()
			if err := fn(ctx); err != nil {
				errCh <- err
			}
		}(handler)
	}
	wg.Wait()
	close(errCh)

	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
		log.Printf("[stage] %s handler error: %v", stage, err)
	}
	if len(errs) > 0 {
		ctx.Lock()
		ctx.Errors = append(ctx.Errors, errs...)
		ctx.Unlock()
	}
}

func (h *StageHost) ToolCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.tools)
}

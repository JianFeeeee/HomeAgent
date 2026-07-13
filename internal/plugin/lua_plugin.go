package plugin

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"
	luaSDK "gitcode.com/JianFeeeee/HomeAgent/internal/lua/sdk"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type toolReg struct {
	def     sdk.ToolDef
	handler *lua.LFunction
}

// luaPlugin wraps a Lua script as an sdk.Plugin.
type luaPlugin struct {
	name     string
	L        *lua.LState
	tbl      *lua.LTable
	tools    map[string]*toolReg
	stages   map[sdk.Stage]*lua.LFunction
	mu       sync.Mutex
}

func newLuaPlugin(luaPath, name string) (*luaPlugin, error) {
	L := lua.NewState()

	// 1) 加载嵌入式 sdk.lua（接口定义 + pure Lua mock 实现）
	if err := L.DoString(luaSDK.SDKSource); err != nil {
		L.Close()
		return nil, fmt.Errorf("load sdk.lua: %w", err)
	}

	sdkTbl := L.GetGlobal("sdk")
	sdkTable, ok := sdkTbl.(*lua.LTable)
	if !ok {
		L.Close()
		return nil, fmt.Errorf("sdk.lua must set global 'sdk' table")
	}

	// 清除 DoString 留在栈上的返回值，栈顶归零
	L.SetTop(0)

	plg := &luaPlugin{
		name:   name,
		L:      L,
		tools:  make(map[string]*toolReg),
		stages: make(map[sdk.Stage]*lua.LFunction),
	}

	// 2) 替换 !impl 函数为 Go stub（暂存 handler，等 Start 时注册到真实 SDK）
	replaceSDKStubs(L, sdkTable, plg)

	// 3) 加载插件主脚本（此时 sdk.* 全局已就绪，带 stub 实现）
	if err := L.DoFile(luaPath); err != nil {
		L.Close()
		return nil, fmt.Errorf("load %s: %w", luaPath, err)
	}

	// 4) 如果脚本返回了 table，保存
	if L.GetTop() > 0 {
		if tbl, ok := L.Get(-1).(*lua.LTable); ok {
			plg.tbl = tbl
			L.Pop(1)
		}
	}

	return plg, nil
}

// replaceSDKStubs 替换 sdk 表中的 !impl 函数为 Go stub。
// stub 暂存 handler，等 Start 时才注册到真实 SDK。
func replaceSDKStubs(L *lua.LState, t *lua.LTable, plg *luaPlugin) {
	t.RawSetString("log", L.NewFunction(func(L *lua.LState) int {
		level := L.ToString(1)
		msg := L.ToString(2)
		fmt.Printf("[lua-plugin/%s] %s: %s\n", plg.name, level, msg)
		return 0
	}))

	t.RawSetString("register_tool", L.NewFunction(func(L *lua.LState) int {
		toolName := L.CheckString(1)
		defTbl := L.CheckTable(2)
		handler := L.CheckFunction(3)

		goDef := sdk.ToolDef{Name: toolName, Plugin: plg.name}
		goDef.Description = defTbl.RawGetString("description").String()
		if params := defTbl.RawGetString("parameters"); params != nil {
			if pt, ok := params.(*lua.LTable); ok {
				goDef.Parameters = make(map[string]interface{})
				pt.ForEach(func(k, v lua.LValue) {
					goDef.Parameters[k.String()] = luaValueToGo(v)
				})
			}
		}

		plg.mu.Lock()
		plg.tools[toolName] = &toolReg{def: goDef, handler: handler}
		plg.mu.Unlock()
		return 0
	}))

	t.RawSetString("register_stage", L.NewFunction(func(L *lua.LState) int {
		stage := sdk.Stage(L.CheckString(1))
		handler := L.CheckFunction(2)
		plg.mu.Lock()
		plg.stages[stage] = handler
		plg.mu.Unlock()
		return 0
	}))

	t.RawSetString("register_api", L.NewFunction(func(L *lua.LState) int {
		return 0
	}))

	t.RawSetString("get_setting", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNil)
		return 1
	}))
	t.RawSetString("set_setting", L.NewFunction(func(L *lua.LState) int {
		return 0
	}))

	t.RawSetString("inject_text", L.NewFunction(func(L *lua.LState) int { return 0 }))
	t.RawSetString("inject_interrupt", L.NewFunction(func(L *lua.LState) int { return 0 }))
	t.RawSetString("inject_text_no_memory", L.NewFunction(func(L *lua.LState) int { return 0 }))

	// http 子表
	if httpTable, ok := t.RawGetString("http").(*lua.LTable); ok {
		httpTable.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
			url := L.CheckString(1)
			resp, err := http.Get(url)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			result := L.NewTable()
			result.RawSetString("status", lua.LNumber(resp.StatusCode))
			result.RawSetString("body", lua.LString(string(body)))
			headers := L.NewTable()
			for k, v := range resp.Header {
				headers.RawSetString(k, lua.LString(strings.Join(v, ", ")))
			}
			result.RawSetString("headers", headers)
			L.Push(result)
			L.Push(lua.LNil)
			return 2
		}))
		httpTable.RawSetString("post", L.NewFunction(func(L *lua.LState) int {
			url := L.CheckString(1)
			body := L.CheckString(2)
			contentType := L.OptString(3, "application/json")
			resp, err := http.Post(url, contentType, strings.NewReader(body))
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			result := L.NewTable()
			result.RawSetString("status", lua.LNumber(resp.StatusCode))
			result.RawSetString("body", lua.LString(string(respBody)))
			L.Push(result)
			L.Push(lua.LNil)
			return 2
		}))
	}
}

// replaceSDKReal 用真实 SDK 实现替换 sdk 表。
// 此时 plg.handlers/stages 已存有加载期间注册的 handler。
func replaceSDKReal(L *lua.LState, t *lua.LTable, plg *luaPlugin, s *sdk.PluginSDK) {
	t.RawSetString("register_tool", L.NewFunction(func(L *lua.LState) int {
		toolName := L.CheckString(1)
		defTbl := L.CheckTable(2)
		handler := L.CheckFunction(3)

		goDef := sdk.ToolDef{Name: toolName, Plugin: plg.name}
		goDef.Description = defTbl.RawGetString("description").String()
		if params := defTbl.RawGetString("parameters"); params != nil {
			if pt, ok := params.(*lua.LTable); ok {
				goDef.Parameters = make(map[string]interface{})
				pt.ForEach(func(k, v lua.LValue) {
					goDef.Parameters[k.String()] = luaValueToGo(v)
				})
			}
		}

		h := makeToolHandler(plg, toolName, handler)
		if err := s.RegisterTool(toolName, goDef, h); err != nil {
			L.RaiseError("register_tool: %v", err)
		}
		return 0
	}))

	t.RawSetString("register_stage", L.NewFunction(func(L *lua.LState) int {
		stage := sdk.Stage(L.CheckString(1))
		handler := L.CheckFunction(2)

		h := makeStageHandler(plg, stage, handler)
		s.RegisterStage(stage, h)
		return 0
	}))

	t.RawSetString("register_api", L.NewFunction(func(L *lua.LState) int {
		apiName := L.CheckString(1)
		s.RegisterPluginAPI(apiName)
		return 0
	}))

	t.RawSetString("get_setting", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val, _ := s.Settings().Get(key)
		L.Push(goValueToLua(L, val))
		return 1
	}))
	t.RawSetString("set_setting", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := luaValueToGo(L.CheckAny(2))
		s.Settings().Set(key, val)
		return 0
	}))

	t.RawSetString("inject_text", L.NewFunction(func(L *lua.LState) int {
		s.InjectText(L.CheckString(1), L.CheckString(2), L.CheckString(3))
		return 0
	}))
	t.RawSetString("inject_interrupt", L.NewFunction(func(L *lua.LState) int {
		s.InjectInterruptText(L.CheckString(1), L.CheckString(2), L.CheckString(3))
		return 0
	}))
	t.RawSetString("inject_text_no_memory", L.NewFunction(func(L *lua.LState) int {
		s.InjectTextNoMemory(L.CheckString(1), L.CheckString(2), L.CheckString(3))
		return 0
	}))
}

func makeToolHandler(plg *luaPlugin, name string, fn *lua.LFunction) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		plg.mu.Lock()
		defer plg.mu.Unlock()
		L := plg.L
		L.Push(fn)
		L.Push(goValueToLua(L, args))
		if err := L.PCall(1, 1, nil); err != nil {
			return nil, fmt.Errorf("lua tool %s: %w", name, err)
		}
		result := L.Get(-1)
		L.Pop(1)
		return luaValueToGo(result), nil
	}
}

func makeStageHandler(plg *luaPlugin, stage sdk.Stage, fn *lua.LFunction) sdk.StageHandler {
	return func(sc *sdk.StageContext) error {
		plg.mu.Lock()
		defer plg.mu.Unlock()
		L := plg.L
		L.Push(fn)
		L.Push(goValueToLua(L, map[string]interface{}{
			"raw_message": sc.RawMessage,
			"user_id":     sc.UserID,
			"phase":       string(sc.Phase),
		}))
		if err := L.PCall(1, 0, nil); err != nil {
			return fmt.Errorf("lua stage %s: %w", stage, err)
		}
		return nil
	}
}

func (p *luaPlugin) Name() string { return p.name }

func (p *luaPlugin) Start(s *sdk.PluginSDK) error {
	// 1) 用真实 SDK 实现替换 sdk 表函数
	sdkTbl := p.L.GetGlobal("sdk")
	if sdkTable, ok := sdkTbl.(*lua.LTable); ok {
		replaceSDKReal(p.L, sdkTable, p, s)
	}

	// 2) 批量注册加载期已暂存的 tool handler
	p.mu.Lock()
	tools := make(map[string]*toolReg, len(p.tools))
	for k, v := range p.tools {
		tools[k] = v
	}
	stages := make(map[sdk.Stage]*lua.LFunction, len(p.stages))
	for k, v := range p.stages {
		stages[k] = v
	}
	p.mu.Unlock()

	for toolName, reg := range tools {
		h := makeToolHandler(p, toolName, reg.handler)
		s.RegisterTool(toolName, reg.def, h)
	}
	for stage, fn := range stages {
		h := makeStageHandler(p, stage, fn)
		s.RegisterStage(stage, h)
	}

	// 3) 调用插件的 start(sdk) 回调
	if p.tbl != nil {
		fn := p.tbl.RawGetString("start")
		if fn != nil && fn != lua.LNil {
			p.mu.Lock()
			L := p.L
			L.Push(fn)
			L.Push(sdkTbl)
			err := L.PCall(1, 0, nil)
			p.mu.Unlock()
			if err != nil {
				return fmt.Errorf("lua start %s: %w", p.name, err)
			}
		}
	}

	return nil
}

func (p *luaPlugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.tbl != nil {
		fn := p.tbl.RawGetString("stop")
		if fn != nil && fn != lua.LNil {
			L := p.L
			L.Push(fn)
			if err := L.PCall(0, 0, nil); err != nil {
				p.L.Close()
				return fmt.Errorf("lua stop %s: %w", p.name, err)
			}
		}
	}
	p.L.Close()
	return nil
}

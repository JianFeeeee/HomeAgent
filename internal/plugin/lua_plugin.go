package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
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

type outputChReg struct {
	caps    int
	desc    string
	def     sdk.ChannelDef
	handler *lua.LFunction
}

type stageReg struct {
	handler *lua.LFunction
	scope   sdk.StageScope
}

// luaPlugin wraps a Lua script as an sdk.Plugin.
type luaPlugin struct {
	name      string
	L         *lua.LState
	tbl       *lua.LTable
	tools     map[string]*toolReg
	stages    map[sdk.Stage]*stageReg
	outputChs map[string]*outputChReg
	inputDefs map[string]sdk.ChannelDef
	mu        sync.Mutex
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
		name:      name,
		L:         L,
		tools:     make(map[string]*toolReg),
		stages:    make(map[sdk.Stage]*stageReg),
		outputChs: make(map[string]*outputChReg),
		inputDefs: make(map[string]sdk.ChannelDef),
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

		goDef := parseToolDef(L, defTbl, plg, toolName)

		plg.mu.Lock()
		plg.tools[toolName] = &toolReg{def: goDef, handler: handler}
		plg.mu.Unlock()
		return 0
	}))

	t.RawSetString("register_stage", L.NewFunction(func(L *lua.LState) int {
		stage := sdk.Stage(L.CheckString(1))
		handler := L.CheckFunction(2)
		scope := parseStageScope(L)
		plg.mu.Lock()
		plg.stages[stage] = &stageReg{handler: handler, scope: scope}
		plg.mu.Unlock()
		return 0
	}))

	t.RawSetString("register_api", L.NewFunction(func(L *lua.LState) int {
		return 0
	}))

	t.RawSetString("register_output_channel", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		caps := L.CheckInt(2)
		desc := L.CheckString(3)
		defTbl := L.CheckTable(4)
		handler := L.CheckFunction(5)

		chDef := parseChannelDef(L, defTbl, plg)

		plg.mu.Lock()
		plg.outputChs[name] = &outputChReg{caps: caps, desc: desc, def: chDef, handler: handler}
		plg.mu.Unlock()
		return 0
	}))

	t.RawSetString("register_input_channel", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		defTbl := L.CheckTable(2)

		chDef := parseChannelDef(L, defTbl, plg)

		plg.mu.Lock()
		plg.inputDefs[name] = chDef
		plg.mu.Unlock()
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

		goDef := parseToolDef(L, defTbl, plg, toolName)

		h := makeToolHandler(plg, toolName, handler)
		if err := s.RegisterTool(toolName, goDef, h); err != nil {
			L.RaiseError("register_tool: %v", err)
		}
		return 0
	}))

	t.RawSetString("register_stage", L.NewFunction(func(L *lua.LState) int {
		stage := sdk.Stage(L.CheckString(1))
		handler := L.CheckFunction(2)
		scope := parseStageScope(L)

		h := makeStageHandler(plg, stage, handler)
		s.RegisterStage(stage, h, scope)
		return 0
	}))

	t.RawSetString("register_api", L.NewFunction(func(L *lua.LState) int {
		apiName := L.CheckString(1)
		s.RegisterPluginAPI(apiName)
		return 0
	}))

	t.RawSetString("register_output_channel", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		caps := L.CheckInt(2)
		desc := L.CheckString(3)
		defTbl := L.CheckTable(4)
		handler := L.CheckFunction(5)

		chDef := parseChannelDef(L, defTbl, plg)

		h := makeOutputHandler(plg, handler)
		if err := s.RegisterOutputChannel(name, caps, desc, chDef, h); err != nil {
			L.RaiseError("register_output_channel: %v", err)
		}
		return 0
	}))

	t.RawSetString("register_input_channel", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		defTbl := L.CheckTable(2)

		chDef := parseChannelDef(L, defTbl, plg)

		if err := s.RegisterInputChannel(name, chDef); err != nil {
			L.RaiseError("register_input_channel: %v", err)
		}
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

	// ---- 数据类 API（与 C ABI 外部插件面完全对齐）----
	// 约定：结果型返回 (result, err)，void 型返回 (nil, err)，成功时 err 为 nil。

	subTable := func(name string) *lua.LTable {
		if v := t.RawGetString(name); v != nil {
			if st, ok := v.(*lua.LTable); ok {
				return st
			}
		}
		st := L.NewTable()
		t.RawSetString(name, st)
		return st
	}
	pushVal := func(val interface{}) int {
		L.Push(jsonToLuaValue(L, val))
		L.Push(lua.LNil)
		return 2
	}
	// pushList 归一化 nil/空 切片与 map 为 Lua 空表。
	pushList := func(val interface{}) int {
		if val == nil {
			return pushVal([]interface{}{})
		}
		v := reflect.ValueOf(val)
		switch v.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			if v.Len() == 0 {
				return pushVal([]interface{}{})
			}
		}
		return pushVal(val)
	}
	pushErr := func(err error) int {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	pushNil := func() int {
		L.Push(lua.LNil)
		L.Push(lua.LNil)
		return 2
	}

	// sdk.set_auto_restart(enabled)
	t.RawSetString("set_auto_restart", L.NewFunction(func(L *lua.LState) int {
		s.SetAutoRestart(L.CheckBool(1))
		return 0
	}))

	// ---- sdk.memory.* (graph memory, 对齐 CORE_MEMORY_*) ----
	memTbl := subTable("memory")
	memTbl.RawSetString("recall", L.NewFunction(func(L *lua.LState) int {
		if m := s.Memory(); m != nil {
			var query []string
			switch v := L.Get(1).(type) {
			case *lua.LTable:
				v.ForEach(func(_, e lua.LValue) { query = append(query, e.String()) })
			default:
				query = []string{L.CheckString(1)}
			}
			entities, relations, err := m.Recall(query, L.OptInt(2, 1))
			if err != nil {
				return pushErr(err)
			}
			return pushVal(map[string]interface{}{"entities": entities, "relations": relations})
		}
		return pushVal(map[string]interface{}{"entities": []interface{}{}, "relations": []interface{}{}})
	}))
	memTbl.RawSetString("commit", L.NewFunction(func(L *lua.LState) int {
		var triples []sdk.Triple
		if tbl := L.OptTable(1, nil); tbl != nil {
			tbl.ForEach(func(_, v lua.LValue) {
				if t2, ok := v.(*lua.LTable); ok {
					triples = append(triples, sdk.Triple{
						Subject:     t2.RawGetString("subject").String(),
						Relation:    t2.RawGetString("relation").String(),
						Object:      t2.RawGetString("object").String(),
						Confidence:  float64(lua.LVAsNumber(t2.RawGetString("confidence"))),
						SubjectType: t2.RawGetString("subject_type").String(),
						ObjectType:  t2.RawGetString("object_type").String(),
					})
				}
			})
		}
		if m := s.Memory(); m != nil {
			if err := m.Commit(triples); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))
	memTbl.RawSetString("introspect", L.NewFunction(func(L *lua.LState) int {
		if m := s.Memory(); m != nil {
			r, err := m.Introspect()
			if err != nil {
				return pushErr(err)
			}
			return pushVal(r)
		}
		return pushVal(map[string]interface{}{})
	}))
	memTbl.RawSetString("merge", L.NewFunction(func(L *lua.LState) int {
		if m := s.Memory(); m != nil {
			n, err := m.MergeEntities(L.CheckString(1), L.CheckString(2))
			if err != nil {
				return pushErr(err)
			}
			return pushVal(n)
		}
		return pushVal(0)
	}))
	memTbl.RawSetString("purge", L.NewFunction(func(L *lua.LState) int {
		mode := "soft"
		if L.OptBool(2, false) {
			mode = "hard"
		}
		criteria := map[string]string{}
		if tbl := L.OptTable(1, nil); tbl != nil {
			tbl.ForEach(func(k, v lua.LValue) {
				criteria[k.String()] = v.String()
			})
		}
		if m := s.Memory(); m != nil {
			n, err := m.Purge(criteria, mode)
			if err != nil {
				return pushErr(err)
			}
			return pushVal(n)
		}
		return pushVal(0)
	}))

	// ---- sdk.doc.* (document memory, 对齐 CORE_DOC_*) ----
	docTbl := subTable("doc")
	docTbl.RawSetString("query", L.NewFunction(func(L *lua.LState) int {
		if dm := s.DocMemory(); dm != nil {
			return pushList(dm.Query(L.CheckString(1), L.OptInt(2, 5)))
		}
		return pushVal([]interface{}{})
	}))
	docTbl.RawSetString("insert", L.NewFunction(func(L *lua.LState) int {
		if dm := s.DocMemory(); dm != nil {
			tbl := L.CheckTable(1)
			if err := dm.Insert(&sdk.Doc{
				ID:      tbl.RawGetString("id").String(),
				Title:   tbl.RawGetString("title").String(),
				Content: tbl.RawGetString("content").String(),
			}); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))
	docTbl.RawSetString("remove", L.NewFunction(func(L *lua.LState) int {
		if dm := s.DocMemory(); dm != nil {
			dm.Remove(L.CheckString(1))
		}
		return pushNil()
	}))
	docTbl.RawSetString("stats", L.NewFunction(func(L *lua.LState) int {
		if dm := s.DocMemory(); dm != nil {
			return pushVal(dm.Stats())
		}
		return pushVal(map[string]interface{}{})
	}))

	// ---- sdk.knowledge.* (对齐 CORE_KNOWLEDGE_*) ----
	knTbl := subTable("knowledge")
	knTbl.RawSetString("search", L.NewFunction(func(L *lua.LState) int {
		if kn := s.Knowledge(); kn != nil {
			results, err := kn.Search(L.CheckString(1), L.OptInt(2, 5))
			if err != nil {
				return pushErr(err)
			}
			return pushList(results)
		}
		return pushVal([]interface{}{})
	}))
	knTbl.RawSetString("add", L.NewFunction(func(L *lua.LState) int {
		if kn := s.Knowledge(); kn != nil {
			if err := kn.Add(L.CheckString(1), L.CheckString(2)); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))
	knTbl.RawSetString("list", L.NewFunction(func(L *lua.LState) int {
		if kn := s.Knowledge(); kn != nil {
			list, err := kn.List()
			if err != nil {
				return pushErr(err)
			}
			return pushList(list)
		}
		return pushVal([]interface{}{})
	}))

	// ---- sdk.text_memory.* (对齐 CORE_TEXT_MEMORY_APPEND) ----
	tmTbl := subTable("text_memory")
	tmTbl.RawSetString("append", L.NewFunction(func(L *lua.LState) int {
		if tmem := s.TextMemory(); tmem != nil {
			tbl := L.CheckTable(1)
			if err := tmem.Append(sdk.TextEvent{
				Role:      tbl.RawGetString("role").String(),
				Content:   tbl.RawGetString("content").String(),
				Timestamp: int64(lua.LVAsNumber(tbl.RawGetString("timestamp"))),
				Channel:   tbl.RawGetString("channel").String(),
			}); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))

	// ---- sdk.llm.* (对齐 CORE_LLM_*) ----
	llmTbl := subTable("llm")
	llmTbl.RawSetString("list_sources", L.NewFunction(func(L *lua.LState) int {
		if llm := s.LLM(); llm != nil {
			return pushList(llm.ListSources())
		}
		return pushVal([]interface{}{})
	}))
	llmTbl.RawSetString("set_source", L.NewFunction(func(L *lua.LState) int {
		if llm := s.LLM(); llm != nil {
			if err := llm.SetSource(L.CheckString(1)); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))
	llmTbl.RawSetString("current_source", L.NewFunction(func(L *lua.LState) int {
		if llm := s.LLM(); llm != nil {
			return pushVal(llm.CurrentSource())
		}
		return pushVal(nil)
	}))

	// ---- sdk.social.* (只读，对齐 CORE_SOCIAL_*，当前核心未装配 SocialAPI 时为 nil) ----
	socTbl := subTable("social")
	socTbl.RawSetString("get_person", L.NewFunction(func(L *lua.LState) int {
		if social := s.Social(); social != nil {
			p, err := social.GetPerson(L.CheckString(1))
			if err != nil {
				return pushErr(err)
			}
			return pushVal(p)
		}
		return pushVal(nil)
	}))
	socTbl.RawSetString("get_network", L.NewFunction(func(L *lua.LState) int {
		if social := s.Social(); social != nil {
			profiles, err := social.GetNetwork(L.CheckString(1), L.OptInt(2, 1))
			if err != nil {
				return pushErr(err)
			}
			return pushList(profiles)
		}
		return pushVal([]interface{}{})
	}))
	socTbl.RawSetString("get_trait", L.NewFunction(func(L *lua.LState) int {
		if social := s.Social(); social != nil {
			val, ok := social.GetTrait(L.CheckString(1), L.CheckString(2))
			return pushVal(map[string]interface{}{"value": val, "found": ok})
		}
		return pushVal(map[string]interface{}{"value": nil, "found": false})
	}))
	socTbl.RawSetString("get_relations", L.NewFunction(func(L *lua.LState) int {
		if social := s.Social(); social != nil {
			rels, err := social.GetRelations(L.CheckString(1))
			if err != nil {
				return pushErr(err)
			}
			return pushList(rels)
		}
		return pushVal([]interface{}{})
	}))
	socTbl.RawSetString("list_persons", L.NewFunction(func(L *lua.LState) int {
		if social := s.Social(); social != nil {
			persons, err := social.ListPersons()
			if err != nil {
				return pushErr(err)
			}
			return pushList(persons)
		}
		return pushVal([]interface{}{})
	}))

	// ---- sdk.settings.* (作用域变体，对齐 CORE_SETTINGS_*) ----
	settTbl := subTable("settings")
	settTbl.RawSetString("get_core", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			v, err := st.GetCore(L.CheckString(1))
			if err != nil {
				return pushErr(err)
			}
			return pushVal(v)
		}
		return pushVal(nil)
	}))
	settTbl.RawSetString("set_core", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			if err := st.SetCore(L.CheckString(1), luaValueToGo(L.CheckAny(2))); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))
	settTbl.RawSetString("list_core", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			keys, err := st.ListCore(L.OptString(1, ""))
			if err != nil {
				return pushErr(err)
			}
			return pushList(keys)
		}
		return pushVal([]interface{}{})
	}))
	settTbl.RawSetString("get_plugin", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			v, err := st.GetPlugin(L.CheckString(1), L.CheckString(2))
			if err != nil {
				return pushErr(err)
			}
			return pushVal(v)
		}
		return pushVal(nil)
	}))
	settTbl.RawSetString("set_plugin", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			if err := st.SetPlugin(L.CheckString(1), L.CheckString(2), luaValueToGo(L.CheckAny(3))); err != nil {
				return pushErr(err)
			}
		}
		return pushNil()
	}))
	settTbl.RawSetString("list_plugin", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			keys, err := st.ListPlugin(L.CheckString(1), L.OptString(2, ""))
			if err != nil {
				return pushErr(err)
			}
			return pushList(keys)
		}
		return pushVal([]interface{}{})
	}))
	settTbl.RawSetString("list", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			keys, err := st.List(L.OptString(1, ""))
			if err != nil {
				return pushErr(err)
			}
			return pushList(keys)
		}
		return pushVal([]interface{}{})
	}))
	settTbl.RawSetString("register_def", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			tbl := L.CheckTable(1)
			def := sdk.ConfigDef{
				Key:         tbl.RawGetString("key").String(),
				Default:     luaValueToGo(tbl.RawGetString("default")),
				Type:        tbl.RawGetString("type").String(),
				DisplayName: tbl.RawGetString("display_name").String(),
				Description: tbl.RawGetString("description").String(),
				Category:    tbl.RawGetString("category").String(),
				Min:         float64(lua.LVAsNumber(tbl.RawGetString("min"))),
				Max:         float64(lua.LVAsNumber(tbl.RawGetString("max"))),
				Step:        float64(lua.LVAsNumber(tbl.RawGetString("step"))),
				Required:    lua.LVAsBool(tbl.RawGetString("required")),
				Secret:      lua.LVAsBool(tbl.RawGetString("secret")),
			}
			if opts := tbl.RawGetString("options"); opts != nil {
				if ot, ok := opts.(*lua.LTable); ok {
					ot.ForEach(func(_, v lua.LValue) {
						def.Options = append(def.Options, v.String())
					})
				}
			}
			st.RegisterDef(def)
		}
		return pushNil()
	}))
	settTbl.RawSetString("defs", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			return pushList(st.Defs(L.OptString(1, "")))
		}
		return pushVal([]interface{}{})
	}))
	settTbl.RawSetString("dump", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			return pushVal(st.Dump())
		}
		return pushVal(map[string]interface{}{})
	}))
	settTbl.RawSetString("plugins", L.NewFunction(func(L *lua.LState) int {
		if st := s.Settings(); st != nil {
			return pushList(st.Plugins())
		}
		return pushVal([]interface{}{})
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
		ctx := map[string]interface{}{
			"raw_message": sc.RawMessage,
			"user_id":     sc.UserID,
			"group_id":    sc.GroupID,
			"phase":       string(sc.Phase),
			"llm_text":    sc.LLMText,
			"final_text":  sc.FinalText,
			"no_memory":   sc.NoMemory,
		}
		if sc.Response != nil {
			ctx["response"] = *sc.Response
		}
		if len(sc.ToolCalls) > 0 {
			ctx["tool_calls"] = jsonToIface(sc.ToolCalls)
		}
		if len(sc.ToolResults) > 0 {
			ctx["tool_results"] = jsonToIface(sc.ToolResults)
		}
		ctxTbl := goValueToLua(L, ctx).(*lua.LTable)
		L.Push(fn)
		L.Push(ctxTbl)
		if err := L.PCall(1, 0, nil); err != nil {
			return fmt.Errorf("lua stage %s: %w", stage, err)
		}
		// 写回：Lua handler 对 ctx table 的字段修改同步回内核 StageContext
		applyLuaStageResult(sc, luaValueToGo(ctxTbl))
		return nil
	}
}

// applyLuaStageResult 将 Lua stage handler 修改后的 ctx 字段写回内核 StageContext。
// Lua 侧修改的字段以 Lua table（引用）形式读回，仅回写插件有权改写的键。
func applyLuaStageResult(sc *sdk.StageContext, modified interface{}) {
	m, ok := modified.(map[string]interface{})
	if !ok {
		return
	}
	sc.Lock()
	defer sc.Unlock()
	if v, ok := m["raw_message"].(string); ok {
		sc.RawMessage = v
	}
	if v, ok := m["llm_text"].(string); ok {
		sc.LLMText = v
	}
	if v, ok := m["final_text"].(string); ok {
		sc.FinalText = v
	}
	if v, ok := m["user_id"].(string); ok {
		sc.UserID = v
	}
	if v, ok := m["group_id"].(string); ok {
		sc.GroupID = v
	}
	if v, ok := m["no_memory"].(bool); ok {
		sc.NoMemory = v
	}
	if v, ok := m["response"].(string); ok {
		vv := v
		sc.Response = &vv
	}
	if v, ok := m["tool_calls"].([]interface{}); ok && len(v) > 0 {
		if b, err := json.Marshal(v); err == nil {
			var tcs []sdk.ToolCall
			if json.Unmarshal(b, &tcs) == nil {
				sc.ToolCalls = tcs
			}
		}
	}
	if v, ok := m["tool_results"].([]interface{}); ok && len(v) > 0 {
		if b, err := json.Marshal(v); err == nil {
			var trs []sdk.ToolResult
			if json.Unmarshal(b, &trs) == nil {
				sc.ToolResults = trs
			}
		}
	}
}

func parseStageScope(L *lua.LState) sdk.StageScope {
	if L.GetTop() >= 3 && L.ToString(3) == "own_tools" {
		return sdk.StageScopeOwnTools
	}
	return sdk.StageScopeGlobal
}

func parseToolDef(L *lua.LState, defTbl *lua.LTable, plg *luaPlugin, name string) sdk.ToolDef {
	goDef := sdk.ToolDef{Name: name, Plugin: plg.name}
	goDef.Description = defTbl.RawGetString("description").String()
	if v := defTbl.RawGetString("no_memory"); v != nil {
		goDef.NoMemory = lua.LVAsBool(v)
	}
	if v := defTbl.RawGetString("cleaner"); v != nil && v.Type() == lua.LTFunction {
		goDef.Cleaner = makeLuaCleaner(plg, v.(*lua.LFunction))
	}
	if params := defTbl.RawGetString("parameters"); params != nil {
		if pt, ok := params.(*lua.LTable); ok {
			goDef.Parameters = make(map[string]interface{})
			pt.ForEach(func(k, v lua.LValue) {
				goDef.Parameters[k.String()] = luaValueToGo(v)
			})
		}
	}
	return goDef
}

func parseChannelDef(L *lua.LState, defTbl *lua.LTable, plg *luaPlugin) sdk.ChannelDef {
	chDef := sdk.ChannelDef{}
	if v := defTbl.RawGetString("no_memory"); v != nil {
		chDef.NoMemory = lua.LVAsBool(v)
	}
	if v := defTbl.RawGetString("cleaner"); v != nil && v.Type() == lua.LTFunction {
		chDef.Cleaner = makeLuaCleaner(plg, v.(*lua.LFunction))
	}
	return chDef
}

// jsonToIface 通过 JSON 往返把任意 Go 值转换为 JSON 兼容的 interface{} 树。
func jsonToIface(v interface{}) interface{} {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// jsonToLuaValue 通过 JSON 往返把任意 Go 值（struct/slice/map）转换为 Lua 值，
// 语义与 C ABI 外部插件跨边界 JSON 序列化一致。
func jsonToLuaValue(L *lua.LState, v interface{}) lua.LValue {
	return goValueToLua(L, jsonToIface(v))
}

func makeLuaCleaner(plg *luaPlugin, fn *lua.LFunction) func(string) string {
	return func(s string) string {
		plg.mu.Lock()
		defer plg.mu.Unlock()
		L := plg.L
		L.Push(fn)
		L.Push(lua.LString(s))
		if err := L.PCall(1, 1, nil); err != nil {
			return s
		}
		result := L.Get(-1)
		L.Pop(1)
		if str, ok := result.(lua.LString); ok {
			return string(str)
		}
		return s
	}
}

func makeOutputHandler(plg *luaPlugin, fn *lua.LFunction) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		plg.mu.Lock()
		defer plg.mu.Unlock()
		L := plg.L
		L.Push(fn)
		L.Push(goValueToLua(L, args))
		if err := L.PCall(1, 1, nil); err != nil {
			return nil, fmt.Errorf("lua output channel: %w", err)
		}
		result := L.Get(-1)
		L.Pop(1)
		return luaValueToGo(result), nil
	}
}

func (p *luaPlugin) Name() string { return p.name }

func (p *luaPlugin) Start(s *sdk.PluginSDK) error {
	// 1) 用真实 SDK 实现替换 sdk 表函数
	sdkTbl := p.L.GetGlobal("sdk")
	if sdkTable, ok := sdkTbl.(*lua.LTable); ok {
		replaceSDKReal(p.L, sdkTable, p, s)
	}

	// 2) 批量注册加载期已暂存的 tool / stage / channel handler
	p.mu.Lock()
	tools := make(map[string]*toolReg, len(p.tools))
	for k, v := range p.tools {
		tools[k] = v
	}
	stages := make(map[sdk.Stage]*stageReg, len(p.stages))
	for k, v := range p.stages {
		stages[k] = v
	}
	outputChs := make(map[string]*outputChReg, len(p.outputChs))
	for k, v := range p.outputChs {
		outputChs[k] = v
	}
	inputDefs := make(map[string]sdk.ChannelDef, len(p.inputDefs))
	for k, v := range p.inputDefs {
		inputDefs[k] = v
	}
	p.mu.Unlock()

	for toolName, reg := range tools {
		h := makeToolHandler(p, toolName, reg.handler)
		s.RegisterTool(toolName, reg.def, h)
	}
	for stage, reg := range stages {
		h := makeStageHandler(p, stage, reg.handler)
		s.RegisterStage(stage, h, reg.scope)
	}
	for chName, reg := range outputChs {
		h := makeOutputHandler(p, reg.handler)
		s.RegisterOutputChannel(chName, reg.caps, reg.desc, reg.def, h)
	}
	for chName, def := range inputDefs {
		s.RegisterInputChannel(chName, def)
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

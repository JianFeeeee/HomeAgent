package lua

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	lua "github.com/yuin/gopher-lua"
)

//go:embed adapters/*.lua
var bundledAdapters embed.FS

type VM struct {
	mu          sync.Mutex
	state       *lua.LState
	adapterDir  string
	loaded      map[string]*lua.LTable
}

type APIAdapter struct {
	Name    string
	Version string
	Script  string
}

func (v *VM) AdapterDir() string {
	return v.adapterDir
}

func NewVM(adapterDir string) *VM {
	return &VM{
		adapterDir: adapterDir,
		loaded:     make(map[string]*lua.LTable),
	}
}

func (v *VM) Start() error {
	os.MkdirAll(v.adapterDir, 0755)

	if err := v.writeBundledAdapters(); err != nil {
		return fmt.Errorf("write bundled adapters: %w", err)
	}

	v.state = lua.NewState()

	v.state.SetGlobal("log", v.state.NewFunction(func(L *lua.LState) int {
		level := L.ToString(1)
		msg := L.ToString(2)
		fmt.Printf("[lua/%s] %s\n", level, msg)
		return 0
	}))

	jsonTable := v.state.NewTable()
	v.state.SetGlobal("json", jsonTable)
	v.state.SetField(jsonTable, "encode", v.state.NewFunction(func(L *lua.LState) int {
		val := L.CheckAny(1)
		goVal := luaValueToGo(val)
		b, err := json.Marshal(goVal)
		if err != nil {
			L.Push(lua.LString("null"))
			return 1
		}
		L.Push(lua.LString(string(b)))
		return 1
	}))
	v.state.SetField(jsonTable, "decode", v.state.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		var val interface{}
		if err := json.Unmarshal([]byte(str), &val); err != nil {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(goValueToLua(L, val))
		return 1
	}))

	v.state.SetGlobal("http_get", v.state.NewFunction(func(L *lua.LState) int {
		url := L.ToString(1)
		L.Push(lua.LString(fmt.Sprintf(`{"url":%q,"status":200,"body":"mock"}`, url)))
		return 1
	}))

	v.state.SetGlobal("http_post", v.state.NewFunction(func(L *lua.LState) int {
		url := L.ToString(1)
		body := L.ToString(2)
		L.Push(lua.LString(fmt.Sprintf(`{"url":%q,"body":%q,"status":200}`, url, body)))
		return 1
	}))

	if err := v.loadAdapters(); err != nil {
		return fmt.Errorf("load adapters: %w", err)
	}

	return nil
}

func (v *VM) Stop() {
	if v.state != nil {
		v.state.Close()
	}
}

func (v *VM) loadAdapters() error {
	entries, err := os.ReadDir(v.adapterDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".lua" {
			continue
		}
		path := filepath.Join(v.adapterDir, entry.Name())
		if err := v.LoadAdapter(path); err != nil {
			fmt.Printf("[lua] load %s: %v\n", entry.Name(), err)
		}
	}

	return nil
}

func (v *VM) LoadAdapter(path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read adapter: %w", err)
	}

	script := string(data)

	if err := v.state.DoString(script); err != nil {
		return fmt.Errorf("execute adapter script: %w", err)
	}

	adapterTable := v.state.Get(-1)
	v.state.Pop(1)

	tbl, ok := adapterTable.(*lua.LTable)
	if !ok {
		return fmt.Errorf("adapter script must return a table")
	}

	name := ""
	if nameVal := tbl.RawGetString("name"); nameVal != nil {
		name = nameVal.String()
	}
	if name == "" {
		name = filepath.Base(path)
	}

	v.loaded[name] = tbl
	fmt.Printf("[lua] loaded adapter: %s\n", name)
	return nil
}

func (v *VM) CallTransformRequest(name, rawJSON string) (string, error) {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("adapter %s not loaded", name)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_request")
	if fn == nil {
		return "", fmt.Errorf("adapter %s missing transform_request", name)
	}

	v.state.Push(fn)
	v.state.Push(lua.LString(rawJSON))

	if err := v.state.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("transform_request: %w", err)
	}

	result := v.state.Get(-1)
	v.state.Pop(1)

	return result.String(), nil
}

func (v *VM) CallTransformResponse(name, rawJSON string) (string, error) {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return rawJSON, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_response")
	if fn == nil {
		return rawJSON, nil
	}

	v.state.Push(fn)
	v.state.Push(lua.LString(rawJSON))

	if err := v.state.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("transform_response: %w", err)
	}

	result := v.state.Get(-1)
	v.state.Pop(1)

	return result.String(), nil
}

func (v *VM) CallTransformStreamChunk(name, rawLine string) (string, error) {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return rawLine, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_stream_chunk")
	if fn == nil {
		return rawLine, nil
	}

	v.state.Push(fn)
	v.state.Push(lua.LString(rawLine))

	if err := v.state.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("transform_stream_chunk: %w", err)
	}

	result := v.state.Get(-1)
	v.state.Pop(1)

	if result.String() == "" {
		return "", nil
	}
	return result.String(), nil
}

func (v *VM) GetAdapterEndpoint(name string) string {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return ""
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if ep := adapter.RawGetString("endpoint"); ep != nil {
		return ep.String()
	}
	return ""
}

func (v *VM) GetAdapterHeaders(name string) map[string]string {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	headers := make(map[string]string)
	if ht := adapter.RawGetString("headers"); ht != nil {
		if tbl, ok := ht.(*lua.LTable); ok {
			tbl.ForEach(func(key, val lua.LValue) {
				headers[key.String()] = val.String()
			})
		}
	}
	return headers
}

func (v *VM) ListAdapters() []APIAdapter {
	v.mu.Lock()
	defer v.mu.Unlock()

	adapters := make([]APIAdapter, 0)
	for name, tbl := range v.loaded {
		adapter := APIAdapter{Name: name}
		if v := tbl.RawGetString("version"); v != nil {
			adapter.Version = v.String()
		}
		adapters = append(adapters, adapter)
	}
	return adapters
}

func (v *VM) ReloadAll() error {
	v.mu.Lock()
	v.loaded = make(map[string]*lua.LTable)
	v.mu.Unlock()

	if v.state != nil {
		v.state.Close()
	}
	v.state = lua.NewState()

	return v.Start()
}

func luaValueToGo(lv lua.LValue) interface{} {
	switch v := lv.(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return float64(v)
	case lua.LBool:
		return bool(v)
	case *lua.LTable:
		if v.MaxN() > 0 {
			arr := make([]interface{}, 0, v.MaxN())
			v.ForEach(func(_, val lua.LValue) {
				arr = append(arr, luaValueToGo(val))
			})
			return arr
		}
		m := make(map[string]interface{})
		v.ForEach(func(key, val lua.LValue) {
			m[key.String()] = luaValueToGo(val)
		})
		return m
	default:
		return nil
	}
}

func goValueToLua(L *lua.LState, val interface{}) lua.LValue {
	switch v := val.(type) {
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case int:
		return lua.LNumber(v)
	case int64:
		return lua.LNumber(v)
	case bool:
		return lua.LBool(v)
	case nil:
		return lua.LNil
	case []interface{}:
		tbl := L.NewTable()
		for i, item := range v {
			tbl.RawSetInt(i+1, goValueToLua(L, item))
		}
		return tbl
	case map[string]interface{}:
		tbl := L.NewTable()
		for k, item := range v {
			tbl.RawSetString(k, goValueToLua(L, item))
		}
		return tbl
	default:
		return lua.LNil
	}
}

func (v *VM) writeBundledAdapters() error {
	entries, err := bundledAdapters.ReadDir("adapters")
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		dstPath := filepath.Join(v.adapterDir, entry.Name())
		if _, err := os.Stat(dstPath); err == nil {
			continue
		}

		data, err := bundledAdapters.ReadFile(filepath.Join("adapters", entry.Name()))
		if err != nil {
			continue
		}

		if err := os.WriteFile(dstPath, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", entry.Name(), err)
		}
		fmt.Printf("[lua] installed bundled adapter: %s\n", entry.Name())
	}
	return nil
}



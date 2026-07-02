package lua

import (
	"embed"
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

	v.state.SetGlobal("json_encode", v.state.NewFunction(func(L *lua.LState) int {
		val := L.CheckAny(1)
		L.Push(lua.LString(fmt.Sprintf("%v", val)))
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

func (v *VM) CallTransform(name string, input map[string]interface{}) (map[string]interface{}, error) {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("adapter %s not loaded", name)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_request")
	if fn == nil {
		return nil, fmt.Errorf("adapter %s missing transform_request", name)
	}

	inputTable := mapToTable(v.state, input)
	v.state.Push(fn)
	v.state.Push(inputTable)

	if err := v.state.PCall(1, 1, nil); err != nil {
		return nil, fmt.Errorf("transform_request: %w", err)
	}

	result := v.state.Get(-1)
	v.state.Pop(1)

	resultTable, ok := result.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("transform_request must return a table")
	}

	return tableToMap(resultTable), nil
}

func (v *VM) CallResponseTransform(name string, raw []byte) ([]byte, error) {
	v.mu.Lock()
	adapter, ok := v.loaded[name]
	v.mu.Unlock()

	if !ok {
		return raw, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_response")
	if fn == nil {
		return raw, nil
	}

	v.state.Push(fn)
	v.state.Push(lua.LString(string(raw)))

	if err := v.state.PCall(1, 1, nil); err != nil {
		return nil, fmt.Errorf("transform_response: %w", err)
	}

	result := v.state.Get(-1)
	v.state.Pop(1)

	return []byte(result.String()), nil
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

func mapToTable(L *lua.LState, m map[string]interface{}) *lua.LTable {
	tbl := L.NewTable()
	for k, v := range m {
		switch val := v.(type) {
		case string:
			tbl.RawSetString(k, lua.LString(val))
		case float64:
			tbl.RawSetString(k, lua.LNumber(val))
		case int:
			tbl.RawSetString(k, lua.LNumber(val))
		case bool:
			tbl.RawSetString(k, lua.LBool(val))
		case map[string]interface{}:
			tbl.RawSetString(k, mapToTable(L, val))
		case []interface{}:
			arr := L.NewTable()
			for i, item := range val {
				if m, ok := item.(map[string]interface{}); ok {
					arr.RawSetInt(i+1, mapToTable(L, m))
				} else {
					arr.RawSetInt(i+1, lua.LString(fmt.Sprintf("%v", item)))
				}
			}
			tbl.RawSetString(k, arr)
		default:
			tbl.RawSetString(k, lua.LString(fmt.Sprintf("%v", v)))
		}
	}
	return tbl
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

func tableToMap(tbl *lua.LTable) map[string]interface{} {
	result := make(map[string]interface{})
	tbl.ForEach(func(key lua.LValue, val lua.LValue) {
		k := key.String()
		switch v := val.(type) {
		case lua.LString:
			result[k] = string(v)
		case lua.LNumber:
			result[k] = float64(v)
		case lua.LBool:
			result[k] = bool(v)
		case *lua.LTable:
			if v.MaxN() == 0 {
				result[k] = tableToMap(v)
			} else {
				var arr []interface{}
				v.ForEach(func(_, item lua.LValue) {
					if tbl, ok := item.(*lua.LTable); ok {
						arr = append(arr, tableToMap(tbl))
					} else {
						arr = append(arr, item.String())
					}
				})
				result[k] = arr
			}
		}
	})
	return result
}

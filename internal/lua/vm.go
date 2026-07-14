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

type APIAdapter struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// AdapterCache 预加载容器：启动时编译全部脚本到内存，运行时只读缓存，无文件 I/O
type AdapterCache struct {
	mu     sync.RWMutex
	state  *lua.LState
	items  map[string]*lua.LTable
}

func newAdapterCache() *AdapterCache {
	return &AdapterCache{
		state: lua.NewState(),
		items: make(map[string]*lua.LTable),
	}
}

func (c *AdapterCache) setupGlobals() {
	s := c.state
	s.SetGlobal("log", s.NewFunction(func(L *lua.LState) int {
		level := L.ToString(1)
		msg := L.ToString(2)
		fmt.Printf("[lua/%s] %s\n", level, msg)
		return 0
	}))

	jsonTable := s.NewTable()
	s.SetGlobal("json", jsonTable)
	s.SetField(jsonTable, "encode", s.NewFunction(func(L *lua.LState) int {
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
	s.SetField(jsonTable, "decode", s.NewFunction(func(L *lua.LState) int {
		str := L.CheckString(1)
		var val interface{}
		if err := json.Unmarshal([]byte(str), &val); err != nil {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(goValueToLua(L, val))
		return 1
	}))

	s.SetGlobal("http_get", s.NewFunction(func(L *lua.LState) int {
		url := L.ToString(1)
		L.Push(lua.LString(fmt.Sprintf(`{"url":%q,"status":200,"body":"mock"}`, url)))
		return 1
	}))

	s.SetGlobal("http_post", s.NewFunction(func(L *lua.LState) int {
		url := L.ToString(1)
		body := L.ToString(2)
		L.Push(lua.LString(fmt.Sprintf(`{"url":%q,"body":%q,"status":200}`, url, body)))
		return 1
	}))
}

// Preload 编译单个 Lua 适配器脚本并注入缓存
func (c *AdapterCache) Preload(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read adapter: %w", err)
	}
	return c.PreloadSource(filepath.Base(path), string(data))
}

// PreloadSource 从源码字符串编译适配器并注入缓存
func (c *AdapterCache) PreloadSource(name, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.state.DoString(code); err != nil {
		return fmt.Errorf("compile adapter: %w", err)
	}

	tbl, ok := c.state.Get(-1).(*lua.LTable)
	c.state.Pop(1)
	if !ok {
		return fmt.Errorf("adapter script must return a table")
	}

	if n := tbl.RawGetString("name"); n != nil && n.String() != "" {
		name = n.String()
	}

	c.items[name] = tbl
	return nil
}

// Get 运行时从缓存读取已编译的适配器表（无文件 I/O）
func (c *AdapterCache) Get(name string) *lua.LTable {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.items[name]
}

// Remove 从缓存移除适配器
func (c *AdapterCache) Remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, name)
}

// List 返回缓存中所有适配器摘要
func (c *AdapterCache) List() []APIAdapter {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := make([]APIAdapter, 0, len(c.items))
	for name, tbl := range c.items {
		a := APIAdapter{Name: name}
		if v := tbl.RawGetString("version"); v != nil {
			a.Version = v.String()
		}
		list = append(list, a)
	}
	return list
}

// Close 释放 Lua 状态
func (c *AdapterCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != nil {
		c.state.Close()
		c.state = nil
	}
	c.items = nil
}

// VM 运行时虚拟机，封装 AdapterCache 提供适配器调用
type VM struct {
	mu         sync.Mutex
	cache      *AdapterCache
	adapterDir string
}

func NewVM(adapterDir string) *VM {
	return &VM{
		adapterDir: adapterDir,
		cache:      newAdapterCache(),
	}
}

func (v *VM) AdapterDir() string { return v.adapterDir }
func (v *VM) Cache() *AdapterCache { return v.cache }

func (v *VM) Start() error {
	if err := os.MkdirAll(v.adapterDir, 0755); err != nil {
		return fmt.Errorf("mkdir adapter dir: %w", err)
	}
	if err := v.writeBundledAdapters(); err != nil {
		return fmt.Errorf("write bundled adapters: %w", err)
	}

	v.cache.setupGlobals()

	// 预加载：扫描适配器目录，全部编译到缓存
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
		if err := v.cache.Preload(path); err != nil {
			fmt.Printf("[lua] preload %s: %v\n", entry.Name(), err)
		}
	}
	return nil
}

func (v *VM) Stop() {
	v.cache.Close()
}

// LoadAdapter 对外接口：从文件加载并编译适配器到缓存（运行时安全，不影响其他适配器）
func (v *VM) LoadAdapter(path string) error {
	return v.cache.Preload(path)
}

// RemoveAdapter 对外接口：从缓存移除适配器（运行时安全）
func (v *VM) RemoveAdapter(name string) {
	v.cache.Remove(name)
}

func (v *VM) ListAdapters() []APIAdapter {
	return v.cache.List()
}

func (v *VM) CallTransformRequest(name, rawJSON string) (string, error) {
	adapter := v.cache.Get(name)
	if adapter == nil {
		return "", fmt.Errorf("adapter %s not loaded", name)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_request")
	if fn == nil {
		return "", fmt.Errorf("adapter %s missing transform_request", name)
	}

	state := v.cache.state
	state.Push(fn)
	state.Push(lua.LString(rawJSON))
	if err := state.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("transform_request: %w", err)
	}
	result := state.Get(-1)
	state.Pop(1)
	return result.String(), nil
}

func (v *VM) CallTransformResponse(name, rawJSON string) (string, error) {
	adapter := v.cache.Get(name)
	if adapter == nil {
		return rawJSON, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_response")
	if fn == nil {
		return rawJSON, nil
	}

	state := v.cache.state
	state.Push(fn)
	state.Push(lua.LString(rawJSON))
	if err := state.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("transform_response: %w", err)
	}
	result := state.Get(-1)
	state.Pop(1)
	return result.String(), nil
}

func (v *VM) CallTransformStreamChunk(name, rawLine string) (string, error) {
	adapter := v.cache.Get(name)
	if adapter == nil {
		return rawLine, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	fn := adapter.RawGetString("transform_stream_chunk")
	if fn == nil {
		return rawLine, nil
	}

	state := v.cache.state
	state.Push(fn)
	state.Push(lua.LString(rawLine))
	if err := state.PCall(1, 1, nil); err != nil {
		return "", fmt.Errorf("transform_stream_chunk: %w", err)
	}
	result := state.Get(-1)
	state.Pop(1)
	if result.String() == "" {
		return "", nil
	}
	return result.String(), nil
}

func (v *VM) GetAdapterEndpoint(name string) string {
	adapter := v.cache.Get(name)
	if adapter == nil {
		return ""
	}
	if ep := adapter.RawGetString("endpoint"); ep != nil {
		return ep.String()
	}
	return ""
}

func (v *VM) GetAdapterHeaders(name string) map[string]string {
	adapter := v.cache.Get(name)
	if adapter == nil {
		return nil
	}
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

func (v *VM) writeBundledAdapters() error {
	known := []string{
		"openai", "anthropic", "deepseek", "gemini",
		"github", "groq", "mistral", "ollama",
	}
	for _, name := range known {
		srcPath := "adapters/" + name + ".lua"
		dstPath := filepath.Join(v.adapterDir, name+".lua")
		if _, err := os.Stat(dstPath); err == nil {
			continue
		}
		data, err := bundledAdapters.ReadFile(srcPath)
		if err != nil {
			continue
		}
		if err := os.WriteFile(dstPath, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", name+".lua", err)
		}
		fmt.Printf("[lua] installed bundled adapter: %s\n", name+".lua")
	}
	return nil
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

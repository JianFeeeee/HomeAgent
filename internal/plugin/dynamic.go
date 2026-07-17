package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"reflect"
	"unsafe"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin/cabi"
)

// .so 插件必须导出函数 NewPlugin，签名与 NativeFactory 一致：
//
//	func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
//	    return &myPlugin{name: name}, nil
//	}
const (
	soEntry   = "plugin.so"
	dllEntry  = "plugin.dll"
	luaEntry  = "main.lua"
	metaEntry = "plugin.json"
)

type dynamicPlugin struct {
	name string
	impl pubsdk.Plugin
}

func (p *dynamicPlugin) Name() string { return p.name }
func (p *dynamicPlugin) Start(s *sdk.PluginSDK) error {
	return p.impl.Start(s.PluginSDK)
}
func (p *dynamicPlugin) Stop() error { return p.impl.Stop() }

// readManifest 读取插件目录下的 plugin.json。文件不存在时不报错。
func readManifest(dir string) *PluginManifest {
	data, err := os.ReadFile(filepath.Join(dir, metaEntry))
	if err != nil {
		return nil
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}

// cabiPlugin wraps a C ABI loaded plugin (.so via -buildmode=c-shared).
type cabiPlugin struct {
	name   string
	handle *cabi.Handle
}

func (p *cabiPlugin) Name() string { return p.name }
func (p *cabiPlugin) Start(s *sdk.PluginSDK) error {
	// Build CoreAPI from the provided PluginSDK and pass to plugin
	corePtr := buildCoreAPI(s, p.name)
	if err := p.handle.Start(corePtr); err != nil {
		return err
	}

	// Discover tools/stages/channels registered by the plugin during Start
	defs, _ := p.handle.GetToolDefs()
	for _, d := range defs {
		var td pubsdk.ToolDef
		if err := json.Unmarshal(d, &td); err != nil {
			continue
		}
		toolName := td.Name
		td.Plugin = p.name
		s.RegisterTool(toolName, sdk.ToolDef{
			Name:        toolName,
			Description: td.Description,
			Parameters:  td.Parameters,
			Plugin:      p.name,
		}, makeCABIHandler(p.handle, toolName))
	}

	stages, _ := p.handle.GetStages()
	for _, stage := range stages {
		st := sdk.Stage(stage)
		s.RegisterStage(st, func(sc *sdk.StageContext) error {
			ctxJSON, _ := json.Marshal(map[string]interface{}{
				"raw_message": sc.RawMessage,
				"user_id":     sc.UserID,
				"phase":       string(sc.Phase),
			})
			return p.handle.InvokeStage(stage, string(ctxJSON))
		})
	}

	return nil
}

func (p *cabiPlugin) Stop() error {
	p.handle.Close()
	return nil
}

func makeCABIHandler(handle *cabi.Handle, toolName string) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		return handle.InvokeTool(toolName, args)
	}
}

// buildCoreAPI creates a C-compatible CoreAPI function table from a PluginSDK.
// Returns an unsafe.Pointer to a C-allocated struct.
// TODO: implement CoreAPI dispatch that calls back into the Go PluginSDK
func buildCoreAPI(s *sdk.PluginSDK, pluginName string) unsafe.Pointer {
	return unsafe.Pointer(nil) // placeholder - will be implemented in core dispatch
}

// tryLoadSO 尝试从插件目录加载 plugin.so。
// 优先尝试 C ABI 加载（-buildmode=c-shared），失败时回退到 Go plugin.Open。
func tryLoadSO(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	soPath := filepath.Join(dir, soEntry)
	if _, err := os.Stat(soPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Try C ABI first
	handle, err := cabi.Load(soPath, name, config)
	if err == nil {
		return &cabiPlugin{name: name, handle: handle}, nil
	}

	// Fall back to Go plugin.Open
	data, err := os.ReadFile(soPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", soPath, err)
	}
	h := sha256.Sum256(data)
	cacheKey := fmt.Sprintf("plugin_%s_%s.so", name, hex.EncodeToString(h[:8]))
	cachePath := filepath.Join(os.TempDir(), cacheKey)
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		if err := os.WriteFile(cachePath, data, 0644); err != nil {
			return nil, fmt.Errorf("write cache %s: %w", cachePath, err)
		}
	}

	p, err := plugin.Open(cachePath)
	if err != nil {
		return nil, fmt.Errorf("plugin.Open %s: %w", cachePath, err)
	}

	sym, err := p.Lookup("NewPlugin")
	if err != nil {
		return nil, fmt.Errorf(".so %s must export NewPlugin: %w", soPath, err)
	}

	rv := reflect.ValueOf(sym)
	if rv.Kind() != reflect.Func {
		return nil, fmt.Errorf("NewPlugin in %s is not a function (type=%T)", soPath, sym)
	}
	if rv.Type().NumIn() != 2 || rv.Type().NumOut() != 2 {
		return nil, fmt.Errorf("NewPlugin in %s has wrong arity", soPath)
	}
	outs := rv.Call([]reflect.Value{reflect.ValueOf(name), reflect.ValueOf(config)})
	if len(outs) != 2 {
		return nil, fmt.Errorf("NewPlugin in %s returned unexpected values", soPath)
	}
	if !outs[1].IsNil() {
		if err, ok := outs[1].Interface().(error); ok {
			return nil, fmt.Errorf("NewPlugin %s: %w", name, err)
		}
		return nil, fmt.Errorf("NewPlugin %s returned non-error second value", name)
	}
	plg, ok := outs[0].Interface().(pubsdk.Plugin)
	if !ok {
		return nil, fmt.Errorf("NewPlugin in %s does not implement pubsdk.Plugin", soPath)
	}

	return &dynamicPlugin{name: name, impl: plg}, nil
}

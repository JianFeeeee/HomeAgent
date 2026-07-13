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

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
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

// tryLoadSO 尝试从插件目录加载 plugin.so（Go plugin -buildmode=plugin）。
// 返回 nil,nil 表示目录中没有 plugin.so。
func tryLoadSO(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	soPath := filepath.Join(dir, soEntry)
	if _, err := os.Stat(soPath); os.IsNotExist(err) {
		return nil, nil
	}

	// 复制到临时路径以绕过 Go plugin.Open 的路径缓存
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
		return nil, fmt.Errorf("NewPlugin in %s has wrong arity: type=%s in=%d out=%d", soPath, rv.Type().String(), rv.Type().NumIn(), rv.Type().NumOut())
	}
	arg0 := rv.Type().In(0)
	arg1 := rv.Type().In(1)
	out0 := rv.Type().Out(0)
	out1 := rv.Type().Out(1)
	if arg0.Kind() != reflect.String || arg1.Kind() != reflect.Map || out1.String() != "error" {
		return nil, fmt.Errorf("NewPlugin in %s signature mismatch: type=%s arg0=%s arg1=%s out0=%s out1=%s", soPath, rv.Type().String(), arg0.String(), arg1.String(), out0.String(), out1.String())
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
		return nil, fmt.Errorf("NewPlugin in %s returned value that does not implement pubsdk.Plugin", soPath)
	}

	return &dynamicPlugin{name: name, impl: plg}, nil
}

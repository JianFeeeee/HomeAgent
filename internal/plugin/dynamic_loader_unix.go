//go:build linux || darwin

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

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin/cabi"
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

type cabiPlugin struct {
	name   string
	handle *cabi.Handle
}

func (p *cabiPlugin) Name() string { return p.name }
func (p *cabiPlugin) Start(s *sdk.PluginSDK) error {
	corePtr := p.handle.CreateCoreAPI(s)
	if corePtr == nil {
		return fmt.Errorf("cabi: failed to create CoreAPI for %s", p.name)
	}
	if err := p.handle.Start(corePtr); err != nil {
		return fmt.Errorf("cabi: start %s: %w", p.name, err)
	}
	return nil
}

func (p *cabiPlugin) Stop() error {
	_ = p.handle.Stop()
	return nil
}

// Close 卸载动态库（dlclose）。卸载/重载后必须调用，否则同一路径的 dlopen
// 会复用旧句柄（Linux dlopen 语义），新版本的 plugin.so 不会生效。
func (p *cabiPlugin) Close() error {
	p.handle.Close()
	return nil
}

func tryLoadSO(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	soPath := filepath.Join(dir, soEntry)
	if _, err := os.Stat(soPath); os.IsNotExist(err) {
		// 回退尝试 plugin.dylib (macOS 原生扩展名)
		dylibPath := filepath.Join(dir, "plugin.dylib")
		if _, err2 := os.Stat(dylibPath); err2 == nil {
			soPath = dylibPath
		} else {
			return nil, nil
		}
	}

	handle, err := cabi.Load(soPath, name, config)
	if err == nil {
		return &cabiPlugin{name: name, handle: handle}, nil
	}

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

var _ = json.Marshal

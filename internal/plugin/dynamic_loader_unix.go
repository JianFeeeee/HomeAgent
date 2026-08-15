//go:build linux || darwin

package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
	// 本项目插件统一由 plugindev 编译为 c-shared 走 C ABI；
	// 对 c-shared .so 调用 Go plugin.Open 会 fatal（no plugin module data），
	// 因此不再 fallback 到 Go plugin，直接返回加载错误避免崩溃。
	return nil, err
}

var _ = json.Marshal

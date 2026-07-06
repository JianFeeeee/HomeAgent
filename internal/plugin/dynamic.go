package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"plugin"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// .so 插件必须导出函数 NewPlugin，签名与 NativeFactory 一致：
//
//	func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
//	    return &myPlugin{name: name}, nil
//	}
const (
	soEntry   = "plugin.so"
	luaEntry  = "main.lua"
	metaEntry = "plugin.json"
)

type dynamicPlugin struct {
	name string
	impl sdk.Plugin
}

func (p *dynamicPlugin) Name() string               { return p.name }
func (p *dynamicPlugin) Start(s *sdk.PluginSDK) error { return p.impl.Start(s) }
func (p *dynamicPlugin) Stop() error                { return p.impl.Stop() }

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

	fn, ok := sym.(func(string, map[string]interface{}) (sdk.Plugin, error))
	if !ok {
		return nil, fmt.Errorf("NewPlugin in %s has wrong signature", soPath)
	}

	plg, err := fn(name, config)
	if err != nil {
		return nil, fmt.Errorf("NewPlugin %s: %w", name, err)
	}

	return &dynamicPlugin{name: name, impl: plg}, nil
}

// tryLoadLua 尝试从插件目录加载 main.lua（Lua 插件）。
// 返回 nil,nil 表示目录中没有 main.lua。
func tryLoadLua(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	luaPath := filepath.Join(dir, luaEntry)
	if _, err := os.Stat(luaPath); os.IsNotExist(err) {
		return nil, nil
	}

	// 预留：Lua 插件需在 LuaVM 中注册一个 LuaPlugin 包装器
	return nil, fmt.Errorf("lua plugin loading not yet implemented: %s", name)
}

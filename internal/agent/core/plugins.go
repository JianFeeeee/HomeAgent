package core

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

func (a *Agent) executePluginReload() string {
	if a.pluginReg == nil {
		return "插件系统未启用"
	}
	msg, err := a.pluginReg.Reload(a.pluginDir)
	if err != nil {
		return fmt.Sprintf("插件重载失败: %v", err)
	}
	return msg
}

func (a *Agent) autoReloadPlugins() {
	if a.pluginReg == nil {
		return
	}
	for _, name := range a.pluginHealth.pendingReloads() {
		if !a.pluginReg.AutoRestartEnabled(name) {
			log.Printf("[agent] skip auto-reload plugin %s: auto-restart disabled by plugin", name)
			continue
		}
		log.Printf("[agent] auto-reloading unhealthy plugin: %s", name)
		if a.stageHost != nil {
			a.stageHost.UnregisterPluginTools(name)
		}
		if err := a.pluginReg.ReloadOne(name); err != nil {
			log.Printf("[agent] auto-reload plugin %s failed: %v", name, err)
		} else {
			a.pluginHealth.markReloaded(name)
			log.Printf("[agent] plugin %s reloaded successfully", name)
		}
	}
}

func (a *Agent) resolveToolPlugin(name string) string {
	if a.stageHost != nil {
		if plugin := a.stageHost.ToolPlugin(name); plugin != "" {
			return plugin
		}
	}
	if idx := strings.IndexByte(name, '_'); idx > 0 {
		return name[:idx]
	}
	return "core"
}

// executeGetPluginTools 返回指定插件的完整工具定义(名称/参数/用途)。
// 支持按插件名拉取, 未指定时返回全部插件的工具摘要。
func (a *Agent) executeGetPluginTools(pluginName string) string {
	type toolItem struct {
		name, desc string
		params     interface{}
	}
	var items []toolItem

	// 收集 StageHost(SDK 插件)工具
	if a.stageHost != nil {
		for _, d := range a.stageHost.GetToolDefs() {
			plg := d.Plugin
			if plg == "" {
				plg = a.resolveToolPlugin(d.Name)
			}
			if pluginName != "" && plg != pluginName {
				continue
			}
			items = append(items, toolItem{d.Name, d.Description, d.Parameters})
		}
	}
	// 收集 IOManager(设备/通道)工具
	if a.io != nil {
		for _, d := range a.io.GetAllTools() {
			plg := a.resolveToolPlugin(d.Name)
			if pluginName != "" && plg != pluginName {
				continue
			}
			items = append(items, toolItem{d.Name, d.Description, d.Parameters})
		}
	}
	if len(items) == 0 {
		if pluginName != "" {
			return fmt.Sprintf("插件 %s 没有可用的工具定义", pluginName)
		}
		return "当前没有可用的工具定义"
	}
	var sb strings.Builder
	if pluginName == "" {
		sb.WriteString(fmt.Sprintf("共 %d 个工具:\n", len(items)))
	} else {
		sb.WriteString(fmt.Sprintf("插件 %s 共 %d 个工具:\n", pluginName, len(items)))
	}
	for _, it := range items {
		sb.WriteString(fmt.Sprintf("\n### %s\n", it.name))
		if it.desc != "" {
			sb.WriteString(it.desc + "\n")
		}
		if it.params != nil {
			if b, err := json.Marshal(it.params); err == nil && len(b) < 600 {
				sb.WriteString("参数: " + string(b) + "\n")
			}
		}
	}
	return sb.String()
}

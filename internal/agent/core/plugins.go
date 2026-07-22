package core

import (
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

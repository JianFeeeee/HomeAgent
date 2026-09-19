package core

import (
	"log"
	"sync"
	"time"
)

const (
	maxPluginCrashes = 3
	crashWindow      = 5 * time.Minute
	reloadCooldown   = 30 * time.Second
)

type pluginHealthTracker struct {
	mu      sync.Mutex
	records map[string]*pluginHealthRecord
}

type pluginHealthRecord struct {
	CrashCount int
	FirstCrash time.Time
	LastCrash  time.Time
	Unhealthy  bool
	LastReload time.Time
}

func newPluginHealthTracker() *pluginHealthTracker {
	return &pluginHealthTracker{
		records: make(map[string]*pluginHealthRecord),
	}
}

// recordCrash 记录一次崩溃，返回 true 表示需要触发重载
func (t *pluginHealthTracker) recordCrash(plugin string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	r, ok := t.records[plugin]
	if !ok {
		r = &pluginHealthRecord{}
		t.records[plugin] = r
	}

	if now.Sub(r.LastCrash) > crashWindow {
		r.CrashCount = 0
		r.FirstCrash = now
	}

	r.CrashCount++
	r.LastCrash = now

	if r.CrashCount >= maxPluginCrashes {
		r.Unhealthy = true
		log.Printf("[plugin] %s: %d crashes within %v, marking unhealthy", plugin, r.CrashCount, crashWindow)
		return true
	}

	log.Printf("[plugin] %s: crash #%d", plugin, r.CrashCount)
	return false
}

// isHealthy 检查插件是否健康；冷却期后自动恢复
func (t *pluginHealthTracker) isHealthy(plugin string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.records[plugin]
	if !ok {
		return true
	}
	if !r.Unhealthy {
		return true
	}
	if time.Since(r.LastReload) > reloadCooldown {
		r.Unhealthy = false
		r.CrashCount = 0
		log.Printf("[plugin] %s: cooldown passed, restored to healthy", plugin)
		return true
	}
	return false
}

// markReloaded 标记插件已重载
func (t *pluginHealthTracker) markReloaded(plugin string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.records[plugin]
	if ok {
		r.Unhealthy = false
		r.CrashCount = 0
		r.LastReload = time.Now()
	}
}

// pendingReloads 返回已过冷却期、需要重载的插件列表
func (t *pluginHealthTracker) pendingReloads() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []string
	now := time.Now()
	for name, r := range t.records {
		if !r.Unhealthy {
			continue
		}
		if now.Sub(r.LastReload) > reloadCooldown {
			result = append(result, name)
		}
	}
	return result
}

// unhealthyPlugins 返回当前所有不健康的插件名
func (t *pluginHealthTracker) unhealthyPlugins() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []string
	for name, r := range t.records {
		if r.Unhealthy {
			result = append(result, name)
		}
	}
	return result
}

//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// 完全复刻内核 loader.go case 2 + templates.go go_invoke_stage 的链路
type StageCtx struct {
	mu       sync.RWMutex
	LLMText  string
	ToolRes  []string
}

func (c *StageCtx) Lock()    { c.mu.Lock() }
func (c *StageCtx) Unlock()  { c.mu.Unlock() }
func (c *StageCtx) RLock()   { c.mu.RLock() }
func (c *StageCtx) RUnlock() { c.mu.RUnlock() }

// === 模拟外部插件（副本模型）===
func externalPlugin(tag string, ctxJSON string) string {
	// go_invoke_stage: 新建全新对象
	sc := &StageCtx{}
	var m map[string]interface{}
	json.Unmarshal([]byte(ctxJSON), &m)
	if v, ok := m["llm_text"].(string); ok { sc.LLMText = v }

	// 插件 handler：ctx.Lock() 锁的是这个新对象 → 空转
	sc.Lock()
	sc.LLMText = sc.LLMText + "[" + tag + "]"
	sc.Unlock()

	out, _ := json.Marshal(map[string]interface{}{"llm_text": sc.LLMText})
	return string(out)
}

// === 模拟内核 case 2 handler ===
func kernelStageHandler(sc *StageCtx, tag string) {
	sc.RLock()
	snap, _ := json.Marshal(map[string]interface{}{"llm_text": sc.LLMText})
	sc.RUnlock()

	result := externalPlugin(tag, string(snap))

	// applyStageResult
	var m map[string]interface{}
	json.Unmarshal([]byte(result), &m)
	sc.Lock()
	if v, ok := m["llm_text"].(string); ok { sc.LLMText = v }
	sc.Unlock()
}

// === 内置插件：直接改同一对象 ===
func nativePlugin(sc *StageCtx, tag string) {
	sc.Lock()
	sc.LLMText = sc.LLMText + "[" + tag + "]"
	sc.Unlock()
}

func runCase(name string, fn func(*StageCtx, string), tags []string, rounds int) {
	lost := 0
	for r := 0; r < rounds; r++ {
		sc := &StageCtx{LLMText: "BASE"}
		var wg sync.WaitGroup
		for _, t := range tags {
			wg.Add(1)
			go func(t string) { defer wg.Done(); fn(sc, t) }(t)
		}
		wg.Wait()
		// 检查是否所有 tag 都在
		for _, t := range tags {
			if !strings.Contains(sc.LLMText, "["+t+"]") { lost++; break }
		}
	}
	fmt.Printf("  %-28s %d/%d 轮出现修改丢失  (%.1f%%)\n", name, lost, rounds, float64(lost)/float64(rounds)*100)
}

func main() {
	tags := []string{"A", "B", "C", "D", "E"}
	fmt.Println("5 个插件并发在 StageBeforeToolcall 追加标记，各 2000 轮：")
	fmt.Println()
	runCase("内置插件(共享同一对象)", nativePlugin, tags, 2000)
	runCase("外部插件(快照-副本-写回)", kernelStageHandler, tags, 2000)
	fmt.Println()
	fmt.Println("→ 副本模型下 read-modify-write 非原子：快照与写回之间的窗口导致覆盖")
}

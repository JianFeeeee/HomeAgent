//go:build ignore

package main

// 精确复刻现网 AfterToolcall 上 sanitizer(Global,改写) + weather(OwnTools,只读) 的并发
import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type ToolResult struct {
	Name   string      `json:"name"`
	Plugin string      `json:"plugin"`
	Result interface{} `json:"result"`
}
type Ctx struct {
	mu      sync.RWMutex
	ToolRes []ToolResult
}
func (c *Ctx) Lock(){c.mu.Lock()}; func (c *Ctx) Unlock(){c.mu.Unlock()}
func (c *Ctx) RLock(){c.mu.RLock()}; func (c *Ctx) RUnlock(){c.mu.RUnlock()}

func cleanText(s string) string {
	// 模拟 sanitizer：去掉 ANSI/坏字节
	return strings.ReplaceAll(s, "\x1b[31m", "")
}

// 内核 case 2 handler（外部插件通用路径）
func kernelExternal(sc *Ctx, pluginFn func(*Ctx)) {
	// 1. 快照
	sc.RLock()
	snap, _ := json.Marshal(map[string]interface{}{"tool_results": sc.ToolRes})
	sc.RUnlock()

	// 2. go_invoke_stage: 插件进程内全新对象
	local := &Ctx{}
	var m map[string]interface{}
	json.Unmarshal(snap, &m)
	if v, ok := m["tool_results"]; ok {
		b, _ := json.Marshal(v)
		json.Unmarshal(b, &local.ToolRes)
	}

	// 3. 插件 handler 跑在副本上
	pluginFn(local)

	// 4. stageContextWritable: 无条件回传 tool_results
	out := map[string]interface{}{}
	if len(local.ToolRes) > 0 { out["tool_results"] = local.ToolRes }
	rb, _ := json.Marshal(out)

	// 5. applyStageResult 写回内核
	var rm map[string]interface{}
	json.Unmarshal(rb, &rm)
	sc.Lock()
	if v, ok := rm["tool_results"]; ok {
		b, _ := json.Marshal(v)
		var trs []ToolResult
		if json.Unmarshal(b, &trs) == nil { sc.ToolRes = trs }
	}
	sc.Unlock()
}

func sanitizerStage(ctx *Ctx) {
	ctx.Lock(); defer ctx.Unlock()
	for i, tr := range ctx.ToolRes {
		if s, ok := tr.Result.(string); ok {
			ctx.ToolRes[i].Result = cleanText(s)
		}
	}
}
func weatherStage(ctx *Ctx) {
	ctx.Lock(); defer ctx.Unlock()
	// 只读打印，不改（own_tools scope 已匹配）
	_ = len(ctx.ToolRes)
}

func main() {
	const rounds = 3000
	dirty := "\x1b[31m晴 25°C"
	polluted := 0
	for r := 0; r < rounds; r++ {
		sc := &Ctx{ToolRes: []ToolResult{{Name:"weather_query", Plugin:"weather", Result: dirty}}}
		var wg sync.WaitGroup
		wg.Add(2)
		go func(){ defer wg.Done(); kernelExternal(sc, sanitizerStage) }()
		go func(){ defer wg.Done(); kernelExternal(sc, weatherStage) }()
		wg.Wait()
		if s, ok := sc.ToolRes[0].Result.(string); ok && strings.Contains(s, "\x1b[31m") {
			polluted++
		}
	}
	fmt.Printf("现网场景复刻：模型调用 weather_query，sanitizer+weather 并发跑 AfterToolcall\n")
	fmt.Printf("  %d 轮中 %d 轮清洗结果被覆盖 (%.1f%%)\n", rounds, polluted, float64(polluted)/rounds*100)
	if polluted > 0 {
		fmt.Printf("\n  ⚠️ 确认：weather 回传的未清洗快照覆盖了 sanitizer 的清洗结果\n")
		fmt.Printf("  → 脏数据（ANSI 转义）进入 LLM 上下文\n")
	}
}

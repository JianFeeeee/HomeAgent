package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	doc "gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

var (
	hcStageHost      *agentCore.StageHost
	hcIOMgr          *agentIO.IOManager
	hcPluginReg      *plugin.Registry
	hcMemory         *memory.GraphDB
	hcKnowledge      *knowledge.Store
	hcDocStore       *doc.Store
	hcProviderMgr    *agentAPI.ProviderManager
	hcStatusProvider agentCore.StatusProvider
)

type toolInfo struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Pass   bool   `json:"pass"`
}

// llmReport 由 LLM 通过 healthcheck_report 工具提交。
type llmReport struct {
	ToolName string `json:"tool_name"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

func Configure(sh *agentCore.StageHost, iom *agentIO.IOManager, pr *plugin.Registry,
	mem *memory.GraphDB, ks *knowledge.Store, ds *doc.Store, pm *agentAPI.ProviderManager, sp agentCore.StatusProvider) {
	hcStageHost = sh
	hcIOMgr = iom
	hcPluginReg = pr
	hcMemory = mem
	hcKnowledge = ks
	hcDocStore = ds
	hcProviderMgr = pm
	hcStatusProvider = sp
}

func init() {
	plugin.RegisterFactory("healthcheck", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		if hcStageHost == nil {
			return nil, nil
		}
		return New(name), nil
	})
}

type Plugin struct {
	name       string
	mu         sync.Mutex
	reports    []llmReport
	sessionID  string
	selfToolNames map[string]bool

	stopCh  chan struct{}
	stopOnce sync.Once
	perfData   PerfData
}

type PerfData struct {
	LastCheck time.Time         `json:"last_check"`
	Checks    []PerfCheckPoint  `json:"checks"`
}
type PerfCheckPoint struct {
	Time   time.Time `json:"time"`
	Passed int       `json:"passed"`
	Failed int       `json:"failed"`
	Total  int       `json:"total"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

func New(name string) *Plugin {
	return &Plugin{
		name:          name,
		selfToolNames: make(map[string]bool),
		stopCh:        make(chan struct{}),
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.selfToolNames["healthcheck"] = true
	s.RegisterTool("healthcheck", sdk.ToolDef{
		Name:        "healthcheck",
		Description: "运行系统全面健康检查。先执行静态检查（插件/工具列表/记忆/知识库/文档），再启动 LLM 驱动检查：LLM 主动发现并逐个测试各插件提供的工具，并通过 healthcheck_report 上报结果。返回详细的状态报告。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.runFullCheck(s)
	})

	p.selfToolNames["healthcheck_plugins"] = true
	s.RegisterTool("healthcheck_plugins", sdk.ToolDef{
		Name:        "healthcheck_plugins",
		Description: "列出所有已加载的插件及其状态。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.checkPlugins(s)
	})

	p.selfToolNames["healthcheck_tools"] = true
	s.RegisterTool("healthcheck_tools", sdk.ToolDef{
		Name:        "healthcheck_tools",
		Description: "列出系统中所有已注册的工具及其来源。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.listAllTools()
	})

	p.selfToolNames["healthcheck_memory"] = true
	s.RegisterTool("healthcheck_memory", sdk.ToolDef{
		Name:        "healthcheck_memory",
		Description: "测试图记忆系统：写入、查询、清理一条测试实体。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return p.checkMemory()
	})

	p.selfToolNames["healthcheck_report"] = true
	s.RegisterTool("healthcheck_report", sdk.ToolDef{
		Name:        "healthcheck_report",
		Description: "LLM 健康检查结果上报工具。LLM 在逐一测试各工具后，通过此工具提交每个工具的测试状态。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tool_name": map[string]interface{}{"type": "string", "description": "被测试的工具名称"},
				"status":    map[string]interface{}{"type": "string", "description": "测试结果：ok / fail / skip", "enum": []string{"ok", "fail", "skip"}},
				"detail":    map[string]interface{}{"type": "string", "description": "测试详情或错误描述"},
			},
			"required": []string{"tool_name", "status"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		toolName, _ := args["tool_name"].(string)
		status, _ := args["status"].(string)
		detail, _ := args["detail"].(string)
		p.mu.Lock()
		p.reports = append(p.reports, llmReport{ToolName: toolName, Status: status, Detail: detail})
		count := len(p.reports)
		p.mu.Unlock()
		log.Printf("[healthcheck] LLM report: tool=%s status=%s (total %d)", toolName, status, count)
		return map[string]interface{}{"ok": true, "received": count}, nil
	})

	if hcStatusProvider != nil {
		p.selfToolNames["healthcheck_kernel"] = true
		s.RegisterTool("healthcheck_kernel", sdk.ToolDef{
			Name:        "healthcheck_kernel",
			Description: "查询 Agent 内核运行状态快照，包括插件/工具/记忆/知识库/LLM Provider/运行时等各子系统信息。Agent 可通过此工具自主监测内核健康。",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		}, func(args map[string]interface{}) (interface{}, error) {
			return hcStatusProvider.GetKernelStatus(), nil
		})
	}

	p.selfToolNames["healthcheck_perf"] = true
	s.RegisterTool("healthcheck_perf", sdk.ToolDef{
		Name:        "healthcheck_perf",
		Description: "查询健康检查性能监控数据，包括最近检查时间、历史检查记录（最多 100 条）及通过/失败统计。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		passed := 0
		failed := 0
		for _, c := range p.perfData.Checks {
			passed += c.Passed
			failed += c.Failed
		}
		return map[string]interface{}{
			"status":      "ok",
			"last_check":  p.perfData.LastCheck,
			"total_checks": len(p.perfData.Checks),
			"total_passed": passed,
			"total_failed": failed,
			"history":      p.perfData.Checks,
		}, nil
	})

	p.startAutoCheck(s, 30*time.Minute)

	log.Printf("[healthcheck] ready (stageHost=%v iom=%v reg=%v mem=%v ks=%v ds=%v pm=%v sp=%v)",
		hcStageHost != nil, hcIOMgr != nil, hcPluginReg != nil,
		hcMemory != nil, hcKnowledge != nil, hcDocStore != nil, hcProviderMgr != nil, hcStatusProvider != nil)
	return nil
}

func (p *Plugin) Stop() error {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	return nil
}

func (p *Plugin) startAutoCheck(s *sdk.PluginSDK, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.runAutoCheck(s)
			}
		}
	}()
	log.Printf("[healthcheck] auto-check started, interval=%v", interval)
}

func (p *Plugin) runAutoCheck(s *sdk.PluginSDK) {
	result, err := p.runFullCheck(s)
	if err != nil {
		log.Printf("[healthcheck] auto-check error: %v", err)
		return
	}
	resp, _ := result.(map[string]interface{})
	passed, _ := resp["passed"].(int)
	failed, _ := resp["failed"].(int)
	total, _ := resp["total"].(int)
	elapsed := int64(0)
	if checks, ok := resp["checks"]; ok {
		if arr, ok := checks.([]checkResult); ok && len(arr) > 0 {
			elapsed = int64(len(arr)) // approximate
		}
	}

	pt := PerfCheckPoint{
		Time:      time.Now(),
		Passed:    passed,
		Failed:    failed,
		Total:     total,
		ElapsedMs: elapsed,
	}
	p.mu.Lock()
	p.perfData.LastCheck = pt.Time
	p.perfData.Checks = append(p.perfData.Checks, pt)
	if len(p.perfData.Checks) > 100 {
		p.perfData.Checks = p.perfData.Checks[len(p.perfData.Checks)-100:]
	}
	p.mu.Unlock()

	log.Printf("[healthcheck] auto-check complete: passed=%d failed=%d total=%d", passed, failed, total)
}

func (p *Plugin) runFullCheck(s *sdk.PluginSDK) (interface{}, error) {
	results := []checkResult{}

	pluginResult := p.checkPluginsRaw()
	results = append(results, pluginResult...)

	toolResult := p.checkToolsRaw()
	results = append(results, toolResult...)

	if hcMemory != nil {
		r := p.testMemoryRaw()
		results = append(results, r)
	} else {
		results = append(results, checkResult{Name: "memory", Status: "skip", Detail: "图记忆未初始化", Pass: true})
	}

	if hcKnowledge != nil {
		r := p.testKnowledgeRaw()
		results = append(results, r)
	} else {
		results = append(results, checkResult{Name: "knowledge", Status: "skip", Detail: "知识库未初始化", Pass: true})
	}

	if hcDocStore != nil {
		r := p.testDocStoreRaw()
		results = append(results, r)
	} else {
		results = append(results, checkResult{Name: "documents", Status: "skip", Detail: "文档记忆未初始化", Pass: true})
	}

	if hcProviderMgr != nil {
		r := p.testLLMDriven()
		results = append(results, r)
	} else {
		results = append(results, checkResult{Name: "llm_discovery", Status: "skip", Detail: "LLM Provider 未初始化", Pass: true})
	}

	passCount := 0
	failCount := 0
	for _, r := range results {
		if r.Pass {
			passCount++
		} else {
			failCount++
		}
	}

	summary := fmt.Sprintf("通过 %d / %d, 失败 %d", passCount, len(results), failCount)

	return map[string]interface{}{
		"status":   "ok",
		"summary":  summary,
		"total":    len(results),
		"passed":   passCount,
		"failed":   failCount,
		"checks":   results,
	}, nil
}

func (p *Plugin) checkPlugins(s *sdk.PluginSDK) (interface{}, error) {
	results := p.checkPluginsRaw()
	return map[string]interface{}{
		"status":  "ok",
		"plugins": results,
		"count":   len(results),
	}, nil
}

func (p *Plugin) checkPluginsRaw() []checkResult {
	if hcPluginReg == nil {
		return []checkResult{{Name: "plugins", Status: "skip", Detail: "插件注册表未初始化", Pass: true}}
	}

	names := hcPluginReg.List()
	if names == nil {
		names = []string{}
	}
	return []checkResult{{
		Name:   "plugins",
		Status: "ok",
		Detail: fmt.Sprintf("已加载 %d 个插件: %v", len(names), names),
		Pass:   true,
	}}
}

func (p *Plugin) listAllTools() (interface{}, error) {
	tools := p.collectAllTools()
	return map[string]interface{}{
		"status": "ok",
		"count":  len(tools),
		"tools":  tools,
	}, nil
}

func (p *Plugin) checkToolsRaw() []checkResult {
	tools := p.collectAllTools()
	return []checkResult{{
		Name:   "tools",
		Status: "ok",
		Detail: fmt.Sprintf("系统中共 %d 个工具", len(tools)),
		Pass:   true,
	}}
}

func (p *Plugin) collectAllTools() []toolInfo {
	seen := map[string]bool{}
	var tools []toolInfo

	addTool := func(name, source, desc string) {
		if seen[name] {
			return
		}
		seen[name] = true
		tools = append(tools, toolInfo{Name: name, Source: source, Description: desc})
	}

	if hcStageHost != nil {
		for _, def := range hcStageHost.GetToolDefs() {
			addTool(def.Name, "plugin", def.Description)
		}
	}

	if hcIOMgr != nil {
		for _, def := range hcIOMgr.GetAllTools() {
			addTool(def.Name, "device", def.Description)
		}
	}

	return tools
}

func (p *Plugin) testMemoryRaw() checkResult {
	marker := fmt.Sprintf("_hc_%d", time.Now().UnixNano())
	triples := []memory.Triple{
		{Subject: marker, Relation: "is", Object: "healthcheck_test", SubjectType: "System", ObjectType: "Flag"},
	}

	start := time.Now()
	ec, rc, err := hcMemory.Commit(triples, "healthcheck", 0)
	if err != nil {
		return checkResult{Name: "memory_write", Status: "fail", Detail: fmt.Sprintf("写入失败: %v", err), Pass: false}
	}

	if _, _, err := hcMemory.Commit(triples, "healthcheck_cleanup", 0); err != nil {
		log.Printf("[healthcheck] memory cleanup error: %v", err)
	}

	n, err := hcMemory.Purge(map[string]string{"subject_contains": marker}, "hard")
	if err != nil {
		return checkResult{Name: "memory_purge", Status: "fail", Detail: fmt.Sprintf("清理失败: %v", err), Pass: false}
	}

	elapsed := time.Since(start)
	return checkResult{
		Name:   "memory",
		Status: "ok",
		Detail: fmt.Sprintf("写入 %d 实体/%d 关系, 清理 %d 条, 耗时 %v", ec, rc, n, elapsed.Round(time.Millisecond)),
		Pass:   true,
	}
}

func (p *Plugin) testKnowledgeRaw() checkResult {
	marker := fmt.Sprintf("_hc_knowledge_test_%d", time.Now().UnixNano())
	start := time.Now()

	if err := hcKnowledge.Add(marker, "健康检查测试标记，可忽略"); err != nil {
		return checkResult{Name: "knowledge", Status: "fail", Detail: fmt.Sprintf("写入失败: %v", err), Pass: false}
	}

	results := hcKnowledge.Search("健康检查测试标记", 3)
	elapsed := time.Since(start)

	if len(results) > 0 {
		return checkResult{
			Name:   "knowledge",
			Status: "ok",
			Detail: fmt.Sprintf("写入+查询正常, 耗时 %v", elapsed.Round(time.Millisecond)),
			Pass:   true,
		}
	}

	return checkResult{
		Name:   "knowledge",
		Status: "warn",
		Detail: fmt.Sprintf("写入成功但查询缓存未命中, 耗时 %v", elapsed.Round(time.Millisecond)),
		Pass:   true,
	}
}

func (p *Plugin) testDocStoreRaw() checkResult {
	start := time.Now()
	doc := &doc.Doc{
		Summary: "健康检查测试文档",
		Content: "这是一条由 healthcheck 插件创建的测试文档，用于验证文档记忆系统是否正常工作。",
		Tags:    []string{"healthcheck", "test"},
		Source:  "healthcheck",
	}
	if err := hcDocStore.Insert(doc); err != nil {
		return checkResult{Name: "documents", Status: "fail", Detail: fmt.Sprintf("写入失败: %v", err), Pass: false}
	}

	if doc.ID != "" {
		hcDocStore.Remove(doc.ID)
	}

	elapsed := time.Since(start)
	return checkResult{
		Name:   "documents",
		Status: "ok",
		Detail: fmt.Sprintf("写入+删除正常, 耗时 %v", elapsed.Round(time.Millisecond)),
		Pass:   true,
	}
}

func (p *Plugin) testLLMDriven() checkResult {
	provider := hcProviderMgr.Default()
	if provider == nil {
		return checkResult{Name: "llm_discovery", Status: "skip", Detail: "无可用 LLM Provider", Pass: true}
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 收集所有工具定义（排除健康检查自身的工具以避免循环测试）
	toolDefs := p.collectToolDefsForLLM()

	if len(toolDefs) == 0 {
		return checkResult{Name: "llm_discovery", Status: "skip", Detail: "没有可测试的工具", Pass: true}
	}

	// 重置会话
	p.mu.Lock()
	p.reports = nil
	p.sessionID = fmt.Sprintf("hc_llm_%d", time.Now().UnixNano())
	p.mu.Unlock()

	// 构建 prompt
	prompt := p.buildDiscoveryPrompt(toolDefs)

	msgs := []agentAPI.Message{{Role: "user", Content: prompt}}
	tools := convertToolDefs(toolDefs)

	llmName := provider.Name()
	turnCount := 0
	toolCallCount := 0

	for turn := 0; turn < 20; turn++ {
		resp, err := provider.Chat(ctx, &agentAPI.CompletionRequest{
			Messages:   msgs,
			MaxTokens:  4096,
			Tools:      tools,
			ToolChoice: "auto",
		})
		if err != nil {
			return checkResult{
				Name:   "llm_discovery",
				Status: "fail",
				Detail: fmt.Sprintf("Provider %s 第 %d 轮调用失败: %v (耗时 %v)",
					llmName, turn+1, err, time.Since(start).Round(time.Millisecond)),
				Pass: false,
			}
		}

		turnCount++

		if len(resp.ToolCalls) == 0 {
			break
		}

		msgs = append(msgs, agentAPI.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})

		for _, tc := range resp.ToolCalls {
			toolCallCount++
			content := p.executeToolForLLM(tc)
			msgs = append(msgs, agentAPI.Message{Role: "tool", ToolCallID: tc.ID, Content: content})
		}
	}

	p.mu.Lock()
	reportCount := len(p.reports)
	p.mu.Unlock()

	elapsed := time.Since(start).Round(time.Millisecond)

	detail := fmt.Sprintf("Provider %s, %d 轮对话, %d 次工具调用, %d 份报告, 耗时 %v",
		llmName, turnCount, toolCallCount, reportCount, elapsed)

	return checkResult{
		Name:   "llm_discovery",
		Status: "ok",
		Detail: detail,
		Pass:   true,
	}
}

// collectToolDefsForLLM 收集全部已注册的工具定义供 LLM 发现和测试。
// 动态排除本插件自身注册的工具（通过 selfToolNames），避免 LLM 自我循环调用。
func (p *Plugin) collectToolDefsForLLM() []sdk.ToolDef {
	seen := map[string]bool{}
	var defs []sdk.ToolDef

	addDef := func(d sdk.ToolDef) {
		if p.selfToolNames[d.Name] || seen[d.Name] {
			return
		}
		seen[d.Name] = true
		defs = append(defs, d)
	}

	if hcStageHost != nil {
		for _, d := range hcStageHost.GetToolDefs() {
			addDef(d)
		}
	}
	if hcIOMgr != nil {
		for _, d := range hcIOMgr.GetAllTools() {
			addDef(sdk.ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
		}
	}

	return defs
}

// buildDiscoveryPrompt 为 LLM 构造工具探索 prompt。
func (p *Plugin) buildDiscoveryPrompt(toolDefs []sdk.ToolDef) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`你是一名系统健康检查专家。以下是系统中各插件提供的 %d 个工具（已自动排除健康检查插件自身工具）：

你的任务是：逐一尝试调用这些工具，验证它们是否正常工作，并对于每个工具使用 healthcheck_report 工具上报测试结果。

对于每个工具：
1. 理解它的参数和功能
2. 构造合适的测试参数调用它
3. 根据返回结果判断是否正常
4. 调用 healthcheck_report 工具上报（tool_name, status=ok/fail/skip, detail=详情）

注意：
- 有些工具有副作用（如写入数据），请使用安全参数，测试后应清理
- 尽可能覆盖所有工具
- 每个工具只需测试一次

开始测试！`, len(toolDefs)))
	return b.String()
}

// executeToolForLLM 在 LLM 工具循环中执行工具调用。
// healthcheck_report 通过 StageHost 路由到自身注册的 handler，负责收集 LLM 上报。
func (p *Plugin) executeToolForLLM(tc agentAPI.ToolCall) string {
	if hcStageHost != nil {
		result, err := hcStageHost.ExecuteTool(tc.Name, tc.Arguments)
		if err != nil {
			return fmt.Sprintf("调用工具 %s 失败: %v", tc.Name, err)
		}
		data, _ := json.Marshal(result)
		return string(data)
	}

	return fmt.Sprintf("工具 %s 不可执行（StageHost 未初始化）", tc.Name)
}

func convertToolDefs(defs []sdk.ToolDef) []interface{} {
	tools := make([]interface{}, len(defs))
	for i, d := range defs {
		tools[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        d.Name,
				"description": d.Description,
				"parameters":  d.Parameters,
			},
		}
	}
	return tools
}

func (p *Plugin) checkMemory() (interface{}, error) {
	if hcMemory == nil {
		return map[string]interface{}{"status": "skip", "pass": true, "detail": "图记忆未初始化"}, nil
	}
	r := p.testMemoryRaw()
	c := map[string]interface{}{
		"status": r.Status,
		"pass":   r.Pass,
	}
	if r.Detail != "" {
		c["detail"] = r.Detail
	}
	return c, nil
}

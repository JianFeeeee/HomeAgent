package core

import (
	"fmt"
	"runtime"
	"time"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/social"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
)

// StatusProvider 内核状态查询接口。插件通过此接口查看内核运行动态。
type StatusProvider interface {
	GetKernelStatus() *KernelStatus
}

// KernelStatus 内核各子系统运行状态的聚合快照。
type KernelStatus struct {
	Uptime    string `json:"uptime"`
	StartTime string `json:"start_time"`

	AgentID string `json:"agent_id"`

	Plugins  []PluginInfo  `json:"plugins"`
	Tools    []sdk.ToolDef `json:"tools"`
	Channels []ChannelInfo `json:"channels"`

	Memory     MemoryStatus     `json:"memory"`
	Knowledge  KnowledgeStatus  `json:"knowledge"`
	Documents  DocumentStatus   `json:"documents"`
	TextMemory TextMemoryStatus `json:"text_memory"`
	Social     SocialStatus     `json:"social"`
	Skills     SkillsStatus     `json:"skills"`

	LLM LLMStatus `json:"llm"`

	Context ContextStatus `json:"context"`

	Runtime RuntimeStatus `json:"runtime"`

	Tracker TrackerStatus `json:"tracker"`
}

type PluginInfo struct {
	Name   string `json:"name"`
	Loaded bool   `json:"loaded"`
}

type ChannelInfo struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Ready bool   `json:"ready"`
}

func channelInfoFromIO(ch agentIO.ChannelInfo) ChannelInfo {
	return ChannelInfo{
		Name:  ch.Name,
		Type:  fmt.Sprintf("%d", ch.Type),
		Ready: true,
	}
}

type MemoryStatus struct {
	Available      bool   `json:"available"`
	EntityCount    int    `json:"entity_count,omitempty"`
	RelationCount  int    `json:"relation_count,omitempty"`
	EntityTypes    int    `json:"entity_types,omitempty"`
}

type KnowledgeStatus struct {
	Available bool     `json:"available"`
	ItemCount int      `json:"item_count,omitempty"`
	Items     []string `json:"items,omitempty"`
}

type DocumentStatus struct {
	Available   bool `json:"available"`
	DocCount    int  `json:"doc_count,omitempty"`
	VectorCount int  `json:"vector_count,omitempty"`
}

type TextMemoryStatus struct {
	Available bool `json:"available"`
	FileCount int  `json:"file_count,omitempty"`
}

type SocialStatus struct {
	Available   bool `json:"available"`
	PersonCount int  `json:"person_count,omitempty"`
}

type SkillsStatus struct {
	Available bool     `json:"available"`
	SkillList []string `json:"skill_list,omitempty"`
}

type LLMStatus struct {
	Available bool   `json:"available"`
	Provider  string `json:"provider,omitempty"`
	Sources   int    `json:"sources,omitempty"`
}

type ContextStatus struct {
	EventCount int `json:"event_count,omitempty"`
}

type RuntimeStatus struct {
	Goroutines int    `json:"goroutines"`
	MemoryMB   int64  `json:"memory_mb"`
	GoVersion  string `json:"go_version"`
}

type TrackerStatus struct {
	Available bool   `json:"available"`
	Dir       string `json:"dir,omitempty"`
}

// collectKernelStatus 聚合内核各子系统状态快照。
// 接收所有子系统引用（均为可选——nil 表示不可用），返回统一的状态报告。
func collectKernelStatus(
	startTime time.Time,
	agentID string,
	providerName string,
	sourceCount int,
	stageHost *StageHost,
	iom *agentIO.IOManager,
	pluginReg *plugin.Registry,
	memDB *memory.GraphDB,
	ks interface{ List() []string },
	docStore *document.Store,
	textMem *text.Memory,
	socialStore *social.SocialStore,
	skMgr *skill.Manager,
	trk *tracker.Tracker,
) *KernelStatus {
	status := &KernelStatus{
		Uptime:    time.Since(startTime).Round(time.Second).String(),
		StartTime: startTime.Format(time.RFC3339),
		AgentID:   agentID,
		Runtime: RuntimeStatus{
			Goroutines: runtime.NumGoroutine(),
			GoVersion:  runtime.Version(),
		},
		LLM: LLMStatus{
			Available: providerName != "",
			Provider:  providerName,
			Sources:   sourceCount,
		},
	}

	// Plugins
	if pluginReg != nil {
		names := pluginReg.List()
		for _, n := range names {
			status.Plugins = append(status.Plugins, PluginInfo{Name: n, Loaded: true})
		}
	}

	// Tools
	if stageHost != nil {
		status.Tools = stageHost.GetToolDefs()
	}

	// Channels
	if iom != nil {
		for _, ch := range iom.ListChannels() {
			status.Channels = append(status.Channels, channelInfoFromIO(ch))
		}
	}

	// Graph memory
	if memDB != nil {
		status.Memory.Available = true
		if info, err := memDB.Introspect(); err == nil {
			if ec, ok := info["entity_count"].(int); ok {
				status.Memory.EntityCount = ec
			}
			if rc, ok := info["relation_count"].(int); ok {
				status.Memory.RelationCount = rc
			}
			if et, ok := info["entity_type_count"].(int); ok {
				status.Memory.EntityTypes = et
			}
		}
	}

	// Knowledge
	if ks != nil {
		status.Knowledge.Available = true
		status.Knowledge.Items = ks.List()
		status.Knowledge.ItemCount = len(status.Knowledge.Items)
	}

	// Documents
	if docStore != nil {
		status.Documents.Available = true
		stats := docStore.Stats()
		if dc, ok := stats["doc_count"].(int); ok {
			status.Documents.DocCount = dc
		}
		if vc, ok := stats["vector_count"].(int); ok {
			status.Documents.VectorCount = vc
		}
	}

	// Text memory
	if textMem != nil {
		status.TextMemory.Available = true
		status.TextMemory.FileCount = textMem.FileCount()
	}

	// Social
	if socialStore != nil {
		status.Social.Available = true
		if persons, err := socialStore.ListPersons(); err == nil {
			status.Social.PersonCount = len(persons)
		}
	}

	// Skills
	if skMgr != nil {
		status.Skills.Available = true
		skills := skMgr.List()
		status.Skills.SkillList = make([]string, len(skills))
		for i, sk := range skills {
			status.Skills.SkillList[i] = sk.Name
		}
	}

	// Tracker
	if trk != nil {
		status.Tracker.Available = true
		status.Tracker.Dir = trk.MergeDir()
	}

	// Memory
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	status.Runtime.MemoryMB = int64(m.Alloc / 1024 / 1024)

	return status
}

// GetKernelStatus 返回 Agent 驱动的内核状态快照。
func (a *Agent) GetKernelStatus() *KernelStatus {
	providerName := ""
	sourceCount := 0
	if a.providerManager != nil {
		sourceCount = len(a.providerManager.List())
	}
	if a.provider != nil {
		providerName = a.provider.Name()
	}

	var textMem *text.Memory
	if a.textMem != nil {
		textMem = a.textMem
	}

	var socialStore *social.SocialStore
	if a.social != nil {
		socialStore = a.social
	}

	var skMgr *skill.Manager
	if a.skills != nil {
		skMgr = a.skills
	}

	var trk *tracker.Tracker
	if a.tracker != nil {
		trk = a.tracker
	}

	ks := collectKernelStatus(
		a.startTime,
		string(a.id),
		providerName,
		sourceCount,
		a.stageHost,
		a.io,
		a.pluginReg,
		a.memory,
		a.knowledge,
		a.docStore,
		textMem,
		socialStore,
		skMgr,
		trk,
	)

	return ks
}

var _ StatusProvider = (*Agent)(nil)

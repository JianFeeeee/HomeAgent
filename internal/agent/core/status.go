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
	"gitcode.com/JianFeeeee/HomeAgent/internal/meta"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
)

// StatusProvider 内核状态查询接口。插件通过此接口查看内核运行动态。
type StatusProvider interface {
	GetKernelStatus() *KernelStatus
}

// 状态 DTO 使用内置 SDK 的中立类型，保证与插件层解耦。
type KernelStatus = sdk.KernelStatus
type PluginInfo = sdk.PluginInfo
type ChannelInfo = sdk.ChannelInfo
type MemoryStatus = sdk.MemoryStatus
type KnowledgeStatus = sdk.KnowledgeStatus
type DocumentStatus = sdk.DocumentStatus
type TextMemoryStatus = sdk.TextMemoryStatus
type SocialStatus = sdk.SocialStatus
type LLMStatus = sdk.LLMStatus
type ContextStatus = sdk.ContextStatus
type RuntimeStatus = sdk.RuntimeStatus
type TrackerStatus = sdk.TrackerStatus
type BuildStatus = sdk.BuildStatus

func channelInfoFromIO(ch agentIO.ChannelInfo) ChannelInfo {
	return ChannelInfo{
		Name:  ch.Name,
		Type:  fmt.Sprintf("%d", ch.Type),
		Ready: true,
	}
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
	trk *tracker.Tracker,
) *KernelStatus {
	status := &KernelStatus{
		Uptime:    time.Since(startTime).Round(time.Second).String(),
		StartTime: startTime.Format(time.RFC3339),
		AgentID:   agentID,
		// 构建身份取自内核自己的 meta（-ldflags 注入点），
		// 而非 SDK 仓的硬编码版本。
		Build: BuildStatus{
			Version:       meta.Version,
			Commit:        meta.Commit,
			BuildTime:     meta.BuildTime,
			SDKCompatible: meta.SDKCompatibleVersion,
			KernelName:    meta.KernelName,
		},
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
		trk,
	)

	return ks
}

var _ StatusProvider = (*Agent)(nil)
var _ sdk.StatusAPI = (*Agent)(nil)

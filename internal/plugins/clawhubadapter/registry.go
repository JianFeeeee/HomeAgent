package clawhubadapter

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

type ToolRegistry struct{}

func (r *ToolRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name        string                 `json:"name"`
		Label       string                 `json:"label"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
		return
	}
	toolName := fmt.Sprintf("%s_%s", pluginName, d.Name)
	tDef := sdk.ToolDef{
		Name:        toolName,
		Description: d.Description,
		Parameters:  d.Parameters,
	}
	handler := func(sp *sidecarProcess, ocToolName string) sdk.ToolHandler {
		return func(args map[string]interface{}) (interface{}, error) {
			return sp.CallTool(ocToolName, args)
		}
	}(sp, d.Name)
	if err := s.RegisterTool(toolName, tDef, handler); err != nil {
		log.Printf("[clawhubadapter] register tool %s: %v", toolName, err)
	}
}

type providerDef struct {
	ocType     string
	toolSuffix string
	desc       string
}

var providerMap = []providerDef{
	{"image_generation", "generate_image", "根据文本描述生成图片，返回图片 URL"},
	{"music_generation", "generate_music", "根据描述生成音乐"},
	{"video_generation", "generate_video", "根据描述生成视频"},
	{"speech", "synthesize_speech", "将文本合成为语音"},
	{"web_search", "web_search", "搜索互联网信息"},
	{"web_fetch", "web_fetch", "获取指定网页的内容"},
	{"media_understanding", "analyze_media", "分析图片、音频或视频内容"},
	{"realtime_transcription", "transcribe_audio", "将音频转写为文字"},
	{"realtime_voice", "voice_io", "实时语音输入输出"},
}

type ProviderRegistry struct{}

func (r *ProviderRegistry) Dispatch(typeStr string, data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.Unmarshal(data, &d)

	for _, p := range providerMap {
		if p.ocType == typeStr {
			toolName := fmt.Sprintf("%s_%s", pluginName, p.toolSuffix)
			desc := p.desc
			if d.Name != "" {
				desc = fmt.Sprintf("[%s] %s", d.Name, desc)
			}
			tDef := sdk.ToolDef{
				Name:        toolName,
				Description: desc,
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			}
			handler := func(sp *sidecarProcess, ocType string) sdk.ToolHandler {
				return func(args map[string]interface{}) (interface{}, error) {
					return sp.CallProvider(ocType, args)
				}
			}(sp, typeStr)
			if err := s.RegisterTool(toolName, tDef, handler); err != nil {
				log.Printf("[clawhubadapter] register provider tool %s: %v", toolName, err)
			}
			return
		}
	}

	log.Printf("[clawhubadapter] unknown provider type: %s (plugin: %s)", typeStr, pluginName)
}

type channelDevice struct {
	name       string
	pluginName string
	sp         *sidecarProcess
	ocType     string
}

func (d *channelDevice) Name() string                         { return d.name }
func (d *channelDevice) Type() agentIO.DeviceType             { return agentIO.DeviceIO }
func (d *channelDevice) Description() string                  { return fmt.Sprintf("OC channel %s (from %s)", d.name, d.pluginName) }
func (d *channelDevice) OutputCapabilities() agentIO.OutputCapability { return ocTypeToCap(d.ocType) }
func (d *channelDevice) Start() error                         { return nil }
func (d *channelDevice) Stop() error                          { return nil }
func (d *channelDevice) Tools() []agentIO.ToolDef             { return nil }
func (d *channelDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return d.sp.CallTool(tool, args)
}

func ocTypeToCap(t string) agentIO.OutputCapability {
	switch t {
	case "text":
		return agentIO.CapText
	case "file":
		return agentIO.CapFile
	case "image":
		return agentIO.CapImage
	case "audio":
		return agentIO.CapAudio
	default:
		return agentIO.CapText
	}
}

type ChannelRegistry struct{}

func (r *ChannelRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
		return
	}
	dev := &channelDevice{
		name:       d.Name,
		pluginName: pluginName,
		sp:         sp,
		ocType:     d.Type,
	}
	if err := s.RegisterChannel(d.Name, dev); err != nil {
		log.Printf("[clawhubadapter] register channel %s: %v", d.Name, err)
	}
}

type StageRegistry struct{}

func (r *StageRegistry) Dispatch(data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	var d struct {
		Name  string `json:"name"`
		Event string `json:"event"`
	}
	json.Unmarshal(data, &d)
	log.Printf("[clawhubadapter] hook %s/%s (plugin: %s) — stub: OC hooks need bidirectional bridge",
		d.Name, d.Event, pluginName)
}

type CapRecorder struct {
	mu   sync.Mutex
	caps []string
}

func (r *CapRecorder) Record(typeStr string, data json.RawMessage, pluginName string) {
	var d struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.Unmarshal(data, &d)
	capStr := fmt.Sprintf("[%s] capability: %s", pluginName, typeStr)
	if d.Name != "" {
		capStr += " (" + d.Name + ")"
	}
	if d.Description != "" {
		capStr += ": " + d.Description
	}
	r.mu.Lock()
	r.caps = append(r.caps, capStr)
	r.mu.Unlock()
}

func (r *CapRecorder) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.caps))
	copy(out, r.caps)
	return out
}

type RegistryDispatcher struct {
	toolReg     *ToolRegistry
	providerReg *ProviderRegistry
	channelReg  *ChannelRegistry
	stageReg    *StageRegistry
	capRecorder *CapRecorder
}

func NewDispatcher() *RegistryDispatcher {
	return &RegistryDispatcher{
		toolReg:     &ToolRegistry{},
		providerReg: &ProviderRegistry{},
		channelReg:  &ChannelRegistry{},
		stageReg:    &StageRegistry{},
		capRecorder: &CapRecorder{},
	}
}

func (d *RegistryDispatcher) Dispatch(typeStr string, data json.RawMessage, pluginName string, sp *sidecarProcess, s *sdk.PluginSDK) {
	switch typeStr {
	case "tool":
		d.toolReg.Dispatch(data, pluginName, sp, s)
	case "channel":
		d.channelReg.Dispatch(data, pluginName, sp, s)
	case "provider", "image_generation_provider", "music_generation_provider",
		"video_generation_provider", "speech_provider",
		"realtime_transcription_provider", "realtime_voice_provider",
		"media_understanding_provider",
		"web_fetch_provider", "web_search_provider",
		"embedding_provider", "memory_embedding_provider":
		d.providerReg.Dispatch(typeStr, data, pluginName, sp, s)
		d.capRecorder.Record(typeStr, data, pluginName)
	case "hook", "runtime_lifecycle", "lifecycle",
		"agent_event_subscription", "agent_harness",
		"session_event", "conversation_binding_resolved",
		"interactive_handler",
		"cli", "cli_backend", "node_cli_feature",
		"command", "http_route", "service",
		"gateway_method", "gateway_discovery_service",
		"trusted_tool_policy", "tool_metadata",
		"context_engine",
		"memory_capability", "memory_prompt_section",
		"memory_flush_plan", "memory_runtime",
		"memory_prompt_supplement", "memory_corpus_supplement",
		"session_extension", "session_scheduler_job",
		"session_action", "control_ui_descriptor",
		"agent_tool_result_middleware":
		d.stageReg.Dispatch(data, pluginName, sp, s)
		d.capRecorder.Record(typeStr, data, pluginName)
	default:
		d.capRecorder.Record(typeStr, data, pluginName)
	}
}

func (d *RegistryDispatcher) Capabilities() []string {
	return d.capRecorder.Snapshot()
}

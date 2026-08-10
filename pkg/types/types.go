package types

import "time"

type AgentState int

const (
	AgentStateStopped  AgentState = 0
	AgentStateRunning  AgentState = 1
	AgentStateDegraded AgentState = 2
	AgentStatePanic    AgentState = 3
)

type HealthStatus int

const (
	HealthUnknown  HealthStatus = 0
	HealthHealthy  HealthStatus = 1
	HealthUnstable HealthStatus = 2
	HealthDown     HealthStatus = 3
	HealthDegraded HealthStatus = 4
)

type AgentID string

type SnapshotID string

type Snapshot struct {
	ID          SnapshotID `json:"id"`
	AgentID     AgentID    `json:"agent_id"`
	CreatedAt   time.Time  `json:"created_at"`
	Reason      string     `json:"reason"`
	Size        int64      `json:"size_bytes"`
	DockerImage string     `json:"docker_image,omitempty"`
	Valid       bool       `json:"valid"`
}

type Heartbeat struct {
	AgentID      AgentID       `json:"agent_id"`
	Timestamp    time.Time     `json:"timestamp"`
	State        AgentState    `json:"state"`
	Health       HealthStatus  `json:"health"`
	Uptime       time.Duration `json:"uptime"`
	LLMConnected bool          `json:"llm_connected"`
	Error        string        `json:"error,omitempty"`
}

type NetworkCheckResult struct {
	LLMAPIReachable     bool          `json:"llm_api_reachable"`
	EndpointsConfigured bool          `json:"endpoints_configured"`
	DNSResolving        bool          `json:"dns_resolving"`
	TCPReachable        bool          `json:"tcp_reachable"`
	Latency             time.Duration `json:"latency_ms"`
	LatencyDegraded     bool          `json:"latency_degraded"`
	Error               string        `json:"error,omitempty"`
}

type SnapshotPolicy struct {
	Interval     time.Duration `json:"interval"`
	MaxSnapshots int           `json:"max_snapshots"`
	PreAction    bool          `json:"pre_action"`
	PostAction   bool          `json:"post_action"`
}

type RollbackPolicy struct {
	MaxRetries      int           `json:"max_retries"`
	HealthThreshold HealthStatus  `json:"health_threshold"`
	CooldownPeriod  time.Duration `json:"cooldown_period"`
	AutoRollback    bool          `json:"auto_rollback"`
}

type AgentConfig struct {
	ID              AgentID        `json:"id"`
	Image           string         `json:"image"`
	Name            string         `json:"name"`
	LLMEndpoints    []string       `json:"llm_endpoints"`
	SnapshotPolicy  SnapshotPolicy `json:"snapshot_policy"`
	RollbackPolicy  RollbackPolicy `json:"rollback_policy"`
	ResourceLimit   ResourceLimit  `json:"resource_limit"`
	OpenClawEnabled bool           `json:"openclaw_enabled"`
}

type ResourceLimit struct {
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	Disk    string `json:"disk"`
	Network bool   `json:"network"`
}

type OperationLog struct {
	ID         string     `json:"id"`
	AgentID    AgentID    `json:"agent_id"`
	Timestamp  time.Time  `json:"timestamp"`
	Action     string     `json:"action"`
	SnapshotID SnapshotID `json:"snapshot_id,omitempty"`
	Success    bool       `json:"success"`
}

type LLMSource struct {
	Name            string `json:"name"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	APIKey          string `json:"api_key,omitempty"`
	Adapter         string `json:"adapter"`
	AdapterPath     string `json:"adapter_path,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxConcurrent   int    `json:"max_concurrent,omitempty"`
	ThinkingEnabled bool   `json:"thinking_enabled,omitempty"`
}

type LLMConfig struct {
	Provider        string      `json:"provider"`
	Model           string      `json:"model"`
	BaseURL         string      `json:"base_url"`
	APIKey          string      `json:"api_key"`
	Adapter         string      `json:"adapter"`
	Temperature     float64     `json:"temperature"`
	MaxTokens       int         `json:"max_tokens"`
	ContextWindow   int         `json:"context_window,omitempty"`
	ThinkingEnabled bool        `json:"thinking_enabled"`
	Sources         []LLMSource `json:"sources,omitempty"`
}

type ImageProcessingConfig struct {
	FallbackProvider string `json:"fallback_provider" yaml:"fallback_provider"`
	FallbackModel    string `json:"fallback_model" yaml:"fallback_model"`
	DescribePrompt   string `json:"describe_prompt" yaml:"describe_prompt"`
	OCREnabled       bool   `json:"ocr_enabled" yaml:"ocr_enabled"`
	OCRPrompt        string `json:"ocr_prompt" yaml:"ocr_prompt"`
}

type AudioProcessingConfig struct {
	FallbackProvider string `json:"fallback_provider" yaml:"fallback_provider"`
	FallbackModel    string `json:"fallback_model" yaml:"fallback_model"`
	DescribePrompt   string `json:"describe_prompt" yaml:"describe_prompt"`
}

type InputProcessingConfig struct {
	Image ImageProcessingConfig `json:"image" yaml:"image"`
	Audio AudioProcessingConfig `json:"audio" yaml:"audio"`
}

type PluginDirConfig struct {
	Dir string `json:"dir"`
}

type Config struct {
	Daemon          DaemonConfig          `json:"daemon"`
	LLM             LLMConfig             `json:"llm"`
	Plugin          PluginDirConfig       `json:"plugin"`
	InputProcessing InputProcessingConfig `json:"input_processing"`
	Defaults        AgentConfig           `json:"defaults"`
	Agents          []AgentConfig         `json:"agents"`
}

type DaemonConfig struct {
	DataDir           string        `json:"data_dir"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	CheckInterval     time.Duration `json:"check_interval"`
	LogLevel          string        `json:"log_level"`
}

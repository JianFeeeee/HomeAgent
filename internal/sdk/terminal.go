package sdk

// TerminalAPI 是内核开放的终端会话与命令历史接口（「内核开，两个插件接」）。
//
// 为什么单开接口而不是塞进 KernelStatus：KernelStatus 是**全量运行态快照**，
// /api/v1/kernel 与前端总览页每 3 秒轮询一次；把每个终端最多 64KB 的输出
// 缓冲塞进去，会让每次轮询都背一份终端全屏内容。终端输出属于**按需拉取**的
// 明细，只该在 /terminals 被访问时取。
//
// 权威状态由内核维护（internal/agent/core/terminal_registry.go）：内核订阅
// 自己的事件总线，归并 EventToolCall（terminal_create/close、cmd_run）与
// EventTerminalOutput（agentcli 生命周期 + 输出），产出唯一真相。
// WebUI 与 CLI 都从这里读，不再各自订阅推导。
type TerminalAPI interface {
	// ListTerminals 返回终端会话快照（含输出缓冲与运行状态）。
	ListTerminals() []TerminalStatus
	// CmdHistory 返回命令执行历史快照（cmd_run，最近 100 条）。
	CmdHistory() []CmdExecStatus
}

// TerminalStatus 与 WebUI termState / CLI cliTermState 同一 JSON 口径。
type TerminalStatus struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`
	Output    string `json:"output"`
	CreatedAt string `json:"created_at"`
	Uptime    string `json:"uptime"`
}

// CmdExecStatus 与 WebUI CmdExec 同一 JSON 口径。
type CmdExecStatus struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status"`
	Time     string `json:"time"`
}

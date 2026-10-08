package webui

import (
	"net/http"

	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// 终端面：终端会话列表与命令历史接口。
//
// 「内核开，两个插件接」后，终端会话与命令历史的权威视图在内核
// （internal/agent/core/terminal_registry.go）：内核订阅自己的事件总线
// 归并 EventToolCall（terminal_create/close、cmd_run）与 EventTerminalOutput
// （agentcli 生命周期 + 输出）。WebUI 不再自己订阅事件攒一份——直接读内核
// 开放的 TerminalAPI（s.Terminal()），与 CLI 同源同口径。
//
// 为什么不塞进 KernelStatus：那是全量快照，前端每 3 秒轮询 /kernel，
// 把每终端最多 64KB 的输出缓冲背进去会让轮询成本爆炸。

// handleTerminals 返回内核权威的终端会话快照。
func (h *Handler) handleTerminals(w http.ResponseWriter, r *http.Request) {
	if h.term == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"terminals": []interface{}{}})
		return
	}
	terms := h.term.ListTerminals()
	if terms == nil {
		terms = []sdk.TerminalStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"terminals": terms})
}

// handleCmdHistory 返回内核权威的命令执行历史快照。
func (h *Handler) handleCmdHistory(w http.ResponseWriter, r *http.Request) {
	if h.term == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"history": []interface{}{}})
		return
	}
	cmds := h.term.CmdHistory()
	if cmds == nil {
		cmds = []sdk.CmdExecStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"history": cmds})
}

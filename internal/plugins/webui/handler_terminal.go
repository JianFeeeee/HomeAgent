package webui

import (
	"time"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"net/http"
)

// 终端面：持久终端会话状态、终端接口、命令历史。

type CmdExec struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status"`
	Time     string `json:"time"`
}

type termState struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`
	Output    string `json:"output"`
	CreatedAt string `json:"created_at"`
	Uptime    string `json:"uptime"`
	created   time.Time
}

const maxCmdHistory = 100

const maxTerminals = 50

// subscribeTerminalStream 常驻订阅终端实时画面推流（terminal_output 事件），
// 维护 termStates 的 Running 状态与全量输出缓冲，供 /api/v1/terminals 与前端轮询使用。
func (h *Handler) subscribeTerminalStream() {
	if h.sdk == nil {
		return
	}
	h.sdk.Subscribe(sdk.EventTerminalOutput, func(ev *sdk.Event) {
		id, _ := ev.Payload["terminal_id"].(string)
		if id == "" {
			return
		}
		output, _ := ev.Payload["output"].(string)
		running, _ := ev.Payload["running"].(bool)
		h.termMu.Lock()
		ts, ok := h.termStates[id]
		if !ok {
			ts = &termState{ID: id, created: time.Now()}
			h.termStates[id] = ts
		}
		ts.Running = running
		if output != "" {
			const maxTermOutput = 64 * 1024
			if len(ts.Output)+len(output) > maxTermOutput {
				excess := len(ts.Output) + len(output) - maxTermOutput
				if len(ts.Output) > excess {
					ts.Output = ts.Output[excess:]
				} else {
					ts.Output = ""
				}
			}
			ts.Output += output
		}
		h.termMu.Unlock()
	})
}

func (h *Handler) handleTerminals(w http.ResponseWriter, r *http.Request) {
	h.termMu.Lock()
	terms := make([]*termState, 0, len(h.termStates))
	for _, ts := range h.termStates {
		ts.Uptime = time.Since(ts.created).Round(time.Second).String()
		terms = append(terms, ts)
	}
	h.termMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"terminals": terms})
}

func (h *Handler) handleCmdHistory(w http.ResponseWriter, r *http.Request) {
	h.cmdMu.Lock()
	result := make([]CmdExec, len(h.cmdHistory))
	copy(result, h.cmdHistory)
	h.cmdMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"history": result})
}

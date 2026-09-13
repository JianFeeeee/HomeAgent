package webui

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"encoding/json"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"net/http"
)

// 对话面：聊天消息模型与内存历史、SSE 事件订阅、对话与历史接口。
//
// 持久化在 history.go（独立文件，位置由插件设置 history_file 决定）。

type ChatMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	Source           string         `json:"source,omitempty"`
	Time             string         `json:"time"`
	// Attachment 附件输出（output_send__webui type=image/file）：
	// image 前端内联展示，file 渲染下载卡片。nil 表示纯文本消息。
	Attachment *Attachment `json:"attachment,omitempty"`
}

// Attachment 描述一条附件消息（与 SSE agent_output 事件的 output_type/url/size 对应）。
type Attachment struct {
	Type string `json:"type"`           // "image" | "file"
	URL  string `json:"url"`            // /files/<name> 或远程 http(s) URL
	Size int64  `json:"size,omitempty"` // 字节数（远程 URL 为 0）
	Name string `json:"name,omitempty"` // 展示用文件名
}

type ChatToolCall struct {
	Tool   string      `json:"tool"`
	Name   string      `json:"name,omitempty"`
	Args   interface{} `json:"args,omitempty"`
	Result interface{} `json:"result,omitempty"`
	Status string      `json:"status,omitempty"`
	Plugin string      `json:"plugin,omitempty"`
}

const maxChatHistory = 200

// ===== client_msg_id 去重（防 GUI 断线重连/超时重试导致的消息重放）=====
// GUI 端每条发送消息带唯一 client_msg_id；服务端按 ID 单飞（singleflight）：
// 首次请求正常注入 agent，同 ID 重放等待首次结果并直接复用，不再重复处理。

const maxChatMsgCache = 256

type chatMsgEntry struct {
	done chan struct{}
	resp *agentIO.OutputEvent
}

func (h *Handler) claimChatMsg(id string) (*chatMsgEntry, bool) {
	h.chatMsgMu.Lock()
	defer h.chatMsgMu.Unlock()
	if e, ok := h.chatMsgCache[id]; ok {
		return e, true
	}
	e := &chatMsgEntry{done: make(chan struct{})}
	h.chatMsgCache[id] = e
	h.chatMsgOrder = append(h.chatMsgOrder, id)
	if len(h.chatMsgOrder) > maxChatMsgCache {
		old := h.chatMsgOrder[0]
		h.chatMsgOrder = h.chatMsgOrder[1:]
		delete(h.chatMsgCache, old)
	}
	return e, false
}

// completeChatMsg 记录首次处理结果并唤醒所有等待的同 ID 重放请求。
func (h *Handler) completeChatMsg(e *chatMsgEntry, resp *agentIO.OutputEvent) {
	e.resp = resp
	close(e.done)
}

func (h *Handler) loadChatHistory() {
	if h.history == nil {
		return
	}
	msgs := h.history.LoadWithMigration(h.settings)
	if len(msgs) == 0 {
		return
	}
	h.chatMu.Lock()
	h.chatHistory = msgs
	h.chatMu.Unlock()
}

func (h *Handler) trackToolEvents() {
	if h.sdk == nil {
		return
	}
	h.sdk.Subscribe(sdk.EventToolCall, func(ev *sdk.Event) {
		h.handleToolEvent(ev)
	})
}

// subscribeChatEvents 捕获所有通道（cli/qq/webui 等）的对话轮次，
// 与 handleChat 的注入一起构成完整的全通道对话历史。
func (h *Handler) subscribeChatEvents() {
	if h.sdk == nil {
		return
	}
	h.sdk.Subscribe(sdk.EventRawInput, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		source, _ := ev.Payload["source"].(string)
		// 用户上传的附件（handleChatFile 注入的 payload 携带 upload_* 字段）
		var att *Attachment
		if url, _ := ev.Payload["upload_url"].(string); url != "" {
			ut, _ := ev.Payload["upload_type"].(string)
			var size int64
			switch v := ev.Payload["upload_size"].(type) {
			case int64:
				size = v
			case float64:
				size = int64(v)
			}
			name, _ := ev.Payload["upload_name"].(string)
			att = &Attachment{Type: ut, URL: url, Size: size, Name: name}
		}
		if content == "" {
			return
		}
		h.chatMu.Lock()
		h.pendingIdx = -1
		h.chatMu.Unlock()
		h.addChatMsg(ChatMsg{
			Role:       "user",
			Content:    content,
			Source:     source,
			Time:       time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
			Attachment: att,
		})
	})
	h.sdk.Subscribe(sdk.EventToolCall, func(ev *sdk.Event) {
		tool, _ := ev.Payload["tool"].(string)
		if tool == "" {
			return
		}
		channel, _ := ev.Payload["channel"].(string)
		if channel == "_consolidation_" {
			return
		}
		plugin, _ := ev.Payload["plugin"].(string)
		status, _ := ev.Payload["status"].(string)
		if status == "" {
			status = "ok"
		}
		tc := ChatToolCall{
			Tool:   tool,
			Name:   tool,
			Args:   ev.Payload["args"],
			Result: ev.Payload["result"],
			Status: status,
			Plugin: plugin,
		}
		h.chatMu.Lock()
		msg := h.pendingAssistantLocked()
		if msg == nil {
			h.chatHistory = append(h.chatHistory, ChatMsg{Role: "assistant", Time: time.Now().Format(time.RFC3339)})
			h.pendingIdx = len(h.chatHistory) - 1
			msg = &h.chatHistory[h.pendingIdx]
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
		h.persistChatLocked()
		h.chatMu.Unlock()
	})
	h.sdk.Subscribe(sdk.EventReasoning, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		if content == "" {
			return
		}
		channel, _ := ev.Payload["channel"].(string)
		if channel == "_consolidation_" {
			return
		}
		h.chatMu.Lock()
		msg := h.pendingAssistantLocked()
		if msg == nil {
			h.chatHistory = append(h.chatHistory, ChatMsg{Role: "assistant", Time: time.Now().Format(time.RFC3339)})
			h.pendingIdx = len(h.chatHistory) - 1
			msg = &h.chatHistory[h.pendingIdx]
		}
		msg.ReasoningContent += content
		h.persistChatLocked()
		h.chatMu.Unlock()
	})
	h.sdk.Subscribe(sdk.EventAgentOutput, func(ev *sdk.Event) {
		content, _ := ev.Payload["content"].(string)
		channel, _ := ev.Payload["channel"].(string)
		kind, _ := ev.Payload["kind"].(string)
		h.chatMu.Lock()
		// 输出通道主动输出(output_send__{通道})作为独立气泡,不并入最终回复
		if kind == "channel_output" {
			h.pendingIdx = -1
			// 附件输出（output_type=image/file）：存 attachment 字段供前端渲染，
			// content 保留原始 payload 作为备选文案（历史兼容旧数据）。
			var att *Attachment
			if ot, _ := ev.Payload["output_type"].(string); ot == "image" || ot == "file" {
				url, _ := ev.Payload["url"].(string)
				size, _ := ev.Payload["size"].(int64)
				if f, ok := ev.Payload["size"].(float64); ok && size == 0 {
					size = int64(f)
				}
				name := url
				if i := strings.LastIndexByte(url, '/'); i >= 0 {
					name = url[i+1:]
				}
				att = &Attachment{Type: ot, URL: url, Size: size, Name: name}
			}
			if content == "" && att == nil {
				h.chatMu.Unlock()
				return
			}
			m := ChatMsg{
				Role:       "assistant",
				Source:     channel,
				Time:       time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
				Attachment: att,
			}
			// 附件消息不把本地路径当正文展示（如 "/tmp/homeagent.png"），置空
			if att != nil {
				m.Content = ""
			} else {
				m.Content = content
			}
			// 已持 chatMu：直接操作 chatHistory + persist，不可调 addChatMsg
			//（内部会重入加锁导致死锁——output_send__webui 发图 60s 超时的根因）
			h.chatHistory = append(h.chatHistory, m)
			if len(h.chatHistory) > maxChatHistory {
				drop := len(h.chatHistory) - maxChatHistory
				h.chatHistory = h.chatHistory[drop:]
				if h.pendingIdx >= 0 {
					h.pendingIdx -= drop
					if h.pendingIdx < 0 {
						h.pendingIdx = -1
					}
				}
			}
			h.persistChatLocked()
			h.chatMu.Unlock()
			return
		}
		if msg := h.pendingAssistantLocked(); msg != nil && content != "" {
			msg.Content = content
			if channel != "" {
				msg.Source = channel
			}
			h.pendingIdx = -1
			h.persistChatLocked()
			h.chatMu.Unlock()
			return
		}
		h.pendingIdx = -1
		h.chatMu.Unlock()
		if content == "" {
			return
		}
		h.addChatMsg(ChatMsg{
			Role:    "assistant",
			Content: content,
			Source:  channel,
			Time:    time.Unix(ev.Timestamp, 0).Format(time.RFC3339),
		})
	})
}

// pendingAssistantLocked 返回 chatHistory 中当前进行中的 assistant 消息（已持有 chatMu）。
// 仅当最后一条是 assistant 且尚未产出最终内容时视为进行中，避免跨轮次误合并。
// chatSaveThrottle 控制写盘频率：变更后延迟这么久落盘，合并连续更新。
const chatSaveThrottle = 3 * time.Second

// chatSaveMaxDelay 是连续写入时的强制落盘上限：聊天再密也不超过这么久。
const chatSaveMaxDelay = 10 * time.Second

// persistChatLocked 标记聊天记录待写盘（调用方已持 chatMu）。
//
// 真正的写盘在 chatPersistLoop 里做，并带节流：原先这里是**每条消息都整段
// 重写一次记录文件**，而一轮对话会触发多次（用户消息、每个工具事件、收尾消息）——
// 200 条上限下文件可达数 MB，于是单轮就放大出几十 MB 写。
func (h *Handler) persistChatLocked() {
	if h.history == nil {
		return
	}
	if !h.chatDirty {
		h.chatDirtySince = time.Now()
	}
	h.chatDirty = true
	select {
	case h.chatSaveWake <- struct{}{}:
	default: // 已有待处理信号，合并即可
	}
}

// chatPersistLoop 把聊天记录按节流节奏落盘，直到 Close。
func (h *Handler) chatPersistLoop() {
	defer close(h.chatLoopDone)
	timer := time.NewTimer(chatSaveThrottle)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-h.chatStop:
			h.flushChat() // 关停前把最后一次变更写下去
			return
		case <-h.chatSaveWake:
			delay := chatSaveThrottle
			h.chatMu.Lock()
			if !h.chatDirtySince.IsZero() {
				if left := chatSaveMaxDelay - time.Since(h.chatDirtySince); left < delay {
					if left < 0 {
						left = 0
					}
					delay = left
				}
			}
			h.chatMu.Unlock()
			timer.Reset(delay)
		case <-timer.C:
			h.flushChat()
		}
	}
}

// flushChat 把当前聊天记录快照写盘。文件 IO 不持 chatMu（快照拷出来再写），
// 写失败则重新标脏，等下一轮重试。
func (h *Handler) flushChat() {
	if h.history == nil {
		return
	}
	h.chatMu.Lock()
	if !h.chatDirty {
		h.chatMu.Unlock()
		return
	}
	h.chatDirty = false
	h.chatDirtySince = time.Time{}
	msgs := make([]ChatMsg, len(h.chatHistory))
	copy(msgs, h.chatHistory)
	h.chatMu.Unlock()

	if err := h.history.Save(msgs); err != nil {
		log.Printf("[webui] 写聊天记录 %s 失败（稍后重试）: %v", h.history.Path(), err)
		h.chatMu.Lock()
		h.chatDirty = true
		if h.chatDirtySince.IsZero() {
			h.chatDirtySince = time.Now()
		}
		h.chatMu.Unlock()
	}
}

// Close 停掉写盘协程并把最后一次变更落盘（幂等）。
// 由插件 Stop 调用；不这样做会丢掉最后一轮对话。
func (h *Handler) Close() {
	h.chatCloseOnce.Do(func() {
		close(h.chatStop)
		<-h.chatLoopDone
	})
}

func (h *Handler) pendingAssistantLocked() *ChatMsg {
	if h.pendingIdx < 0 || h.pendingIdx >= len(h.chatHistory) {
		return nil
	}
	msg := &h.chatHistory[h.pendingIdx]
	if msg.Role != "assistant" || msg.Content != "" {
		return nil
	}
	return msg
}

func (h *Handler) handleToolEvent(ev *sdk.Event) {
	payload := ev.Payload
	tool, _ := payload["tool"].(string)
	args, _ := payload["args"].(map[string]interface{})
	status, _ := payload["status"].(string)
	ts := time.Now()

	switch tool {
	case "cmd_run":
		exec := CmdExec{
			Command: getStr(args, "command"),
			Status:  status,
			Time:    ts.Format(time.RFC3339),
		}
		h.cmdMu.Lock()
		h.cmdHistory = append(h.cmdHistory, exec)
		if len(h.cmdHistory) > maxCmdHistory {
			h.cmdHistory = h.cmdHistory[len(h.cmdHistory)-maxCmdHistory:]
		}
		h.cmdMu.Unlock()

	case "terminal_create":
		id := getStr(args, "id")
		if id == "" {
			// agent 调用时不知道生成的 id，从工具结果中回填
			if res, ok := payload["result"].(map[string]interface{}); ok {
				id = getStr(res, "id")
			}
		}
		if id == "" {
			break
		}
		cmd := getStr(args, "command")
		if cmd == "" {
			if res, ok := payload["result"].(map[string]interface{}); ok {
				cmd = getStr(res, "command")
			}
		}
		now := time.Now()
		term := &termState{
			ID:        id,
			Command:   cmd,
			Running:   true,
			CreatedAt: now.Format(time.RFC3339),
			created:   now,
		}
		h.termMu.Lock()
		if old, ok := h.termStates[id]; ok {
			old.Command = cmd
			old.Running = true
			old.created = now
		} else {
			h.termStates[id] = term
		}
		if len(h.termStates) > maxTerminals {
			for k := range h.termStates {
				delete(h.termStates, k)
				break
			}
		}
		h.termMu.Unlock()

	case "terminal_close":
		id := getStr(args, "id")
		if id != "" {
			h.termMu.Lock()
			if t, ok := h.termStates[id]; ok {
				t.Running = false
			}
			h.termMu.Unlock()
		}
	}
}

func (h *Handler) addChatMsg(msg ChatMsg) {
	h.chatMu.Lock()
	h.chatHistory = append(h.chatHistory, msg)
	if len(h.chatHistory) > maxChatHistory {
		drop := len(h.chatHistory) - maxChatHistory
		h.chatHistory = h.chatHistory[drop:]
		if h.pendingIdx >= 0 {
			h.pendingIdx -= drop
			if h.pendingIdx < 0 {
				h.pendingIdx = -1
			}
		}
	}
	h.persistChatLocked()
	h.chatMu.Unlock()
}

// handleChatHistory 返回对话历史，支持分段懒加载。
//
// 查询参数（全部可选，省略时保持旧行为=返回全量，向后兼容旧客户端）：
//   - limit:  返回条数上限（1..maxChatHistory）。带 limit 时默认取「最新的 limit 条」。
//   - before: 游标，只返回下标 < before 的消息（配合 limit 向上翻页取更早历史）。
//
// 响应额外返回 total / offset / has_more，供前端判断是否继续向上加载。
// 注意：不对 tool_calls / reasoning_content 做任何裁剪——工具调用详情是排查与
// 上下文还原的关键信息，必须完整下发；瘦身只通过分页控制条数。
func (h *Handler) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 0)
	if limit < 0 {
		limit = 0
	}
	if limit > maxChatHistory {
		limit = maxChatHistory
	}

	h.chatMu.Lock()
	total := len(h.chatHistory)
	// before 游标：默认取到末尾（最新）
	end := parseIntDefault(q.Get("before"), total)
	if end < 0 || end > total {
		end = total
	}
	start := 0
	if limit > 0 && end-limit > 0 {
		start = end - limit
	}
	result := make([]ChatMsg, end-start)
	copy(result, h.chatHistory[start:end])
	h.chatMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messages": result,
		"total":    total,
		"offset":   start,
		"has_more": start > 0,
	})
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message     string `json:"message"`
		DeviceID    string `json:"device_id"`     // 消息来源设备（GUI/受控设备），可选
		DeviceName  string `json:"device_name"`   // 设备显示名，可选
		ClientMsgID string `json:"client_msg_id"` // 客户端唯一消息 ID（防断线重放/超时重试）
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	if h.sdk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}

	// client_msg_id 去重：同 ID 重放等待首次结果直接复用，不重复注入 agent。
	// 无 ID 的旧客户端走原路径（agent 核心层另有内容级短窗口去重兑底）。
	var entry *chatMsgEntry
	if body.ClientMsgID != "" {
		var replay bool
		entry, replay = h.claimChatMsg(body.ClientMsgID)
		if replay {
			log.Printf("[webui] duplicate chat msg %s: waiting for first request result", body.ClientMsgID)
			select {
			case <-entry.done:
				resp := entry.resp
				if resp == nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
					return
				}
				content, _ := resp.Payload["content"].(string)
				reasoning, _ := resp.Payload["reasoning_content"].(string)
				result := map[string]interface{}{"response": content, "deduplicated": true}
				if reasoning != "" {
					result["reasoning_content"] = reasoning
				}
				writeJSON(w, http.StatusOK, result)
			case <-r.Context().Done():
				return
			}
			return
		}
	}

	// 来源编码：带设备身份时用 webui/{device_id}（agent 经 injectSourceContext 可见来源）；
	// 无设备时保持 webui（兼容旧调用）。device_name 一并注入便于 agent 识别。
	source := "webui"
	if body.DeviceID != "" {
		source = "webui/" + body.DeviceID
	}
	payload := map[string]interface{}{"content": body.Message}
	if body.DeviceID != "" {
		payload["device_id"] = body.DeviceID
		payload["device_name"] = body.DeviceName
	}
	if body.ClientMsgID != "" {
		payload["client_msg_id"] = body.ClientMsgID
	}
	// 带超时的上下文，防止 InjectInputSync 长时间阻塞 HTTP 请求。
	// 注意：ctx 派生自 r.Context()，客户端提前断开（前端 15s ackTimer abort）时
	// 立即取消，不会真等满 300s；300s 只约束"连接保持 + agent 排队/长生成"场景
	// （agent 串行处理，后发消息的排队时间也计入，60s 曾导致连发第 3 条必超时）。
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	respCh := make(chan *agentIO.OutputEvent, 1)
	go func() {
		respCh <- h.sdk.InjectInputSync(source, "webui", "text", payload)
	}()

	var resp *agentIO.OutputEvent
	select {
	case resp = <-respCh:
	case <-ctx.Done():
		if entry != nil {
			h.completeChatMsg(entry, nil)
		}
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "agent timeout (60s)"})
		return
	}

	if entry != nil {
		h.completeChatMsg(entry, resp)
	}

	if resp == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent unavailable"})
		return
	}
	content, _ := resp.Payload["content"].(string)
	reasoning, _ := resp.Payload["reasoning_content"].(string)
	result := map[string]interface{}{
		"response": content,
	}
	if reasoning != "" {
		result["reasoning_content"] = reasoning
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := r.Context().Done()
	if h.sdk == nil {
		fmt.Fprintf(w, "event: error\ndata: {\"msg\":\"event bus unavailable\"}\n\n")
		flusher.Flush()
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// writeCh 不 close：Subscribe 回调闭包持有它，handler 退出后回调仍可能被
	// 总线异步触发，close 后再发送会 panic（send on closed channel，生产日志中
	// 单日数千次）。writer goroutine 通过 done 退出；发送侧 select on done 防泄漏。
	// 缓冲 2048：reasoning/content 增量是高频小包（LLM token 级），
	// 512 时连续 tool_call + reasoning + delta 密集期会溢出导致前端丢帧。
	// 写入侧用短超时(50ms)兜底，比立即丢弃更友好。
	writeCh := make(chan string, 2048)
	writerDone := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SSE] writer panic: %v", r)
			}
			close(writerDone)
		}()
		// 批量合并窗口：16ms 内收集的增量一次性 flush，降 flush 次数、
		// 避免高频小包拖慢 socket 写导致 writeCh 积压丢 delta。
		pending := make([]string, 0, 64)
		flushPending := func() {
			if len(pending) == 0 {
				return
			}
			for _, line := range pending {
				fmt.Fprintf(w, "%s\n", line)
			}
			flusher.Flush()
			pending = pending[:0]
		}
		flushTicker := time.NewTicker(16 * time.Millisecond)
		defer flushTicker.Stop()
		for {
			select {
			case line := <-writeCh:
				pending = append(pending, line)
				// 大批量一次性 flush：阈值从 64 提高，利用批量减少 syscall 开销
				if len(pending) >= 128 {
					flushPending()
				}
			case <-flushTicker.C:
				flushPending()
			case <-done:
				flushPending()
				return
			}
		}
	}()

	log.Printf("[SSE] handler started, subscribing to events")

	// 解析 Last-Event-ID（断线重连时客户端携带），重放期间内遗漏的事件。
	// 注意：本 handler 的 Last-Event-ID 重放仅为 GUI (cmd/gui/renderer/app.js) 服务。
	// 浏览器原生 EventSource (webui/dashboard.html 使用) 由浏览器自动处理 Last-Event-ID 重连。
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		log.Printf("[SSE] client reported Last-Event-ID: %s", lastEventID)
		if h.sseEvents != nil {
			replayed := h.sseEvents.After(lastEventID)
			if len(replayed) == 0 {
				log.Printf("[SSE] replay: nothing after id %s (id not in ring or already at tip)", lastEventID)
				// ID 不在 ring：说明最后一帧是 delta（delta 不进 ring）或已到最新。
				// 显式通知前端补拉历史，避免其空等后续聚合事件（表现为消息同步不及时）。
				fmt.Fprintf(w, "event: sync_required\ndata: {}\n\n")
				flusher.Flush()
			} else {
				log.Printf("[SSE] replay: sending %d events after id %s", len(replayed), lastEventID)
				for _, rec := range replayed {
					fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n", rec.id, rec.eventType, string(rec.data))
					flusher.Flush()
				}
				log.Printf("[SSE] replay complete, wrote %d events", len(replayed))
			}
		}
	}

	subTypes := []string{"agent_output", "reasoning", "agent_error", "tool_call", "stage", "agent_llm_chain", "terminal_output"}
	// token 级流式增量事件：实时转发给浏览器做逐 token 渲染。
	// 不进 sseEventRing —— 断线重连只重放聚合事件（最终真相），
	// 避免重放 delta 与聚合内容重复追加。
	var unsubs []func()
	var seq int64
	appendDeltaSub := func(evtType sdk.EventType) {
		unsub := h.sdk.Subscribe(evtType, func(evt *sdk.Event) {
			data, _ := json.Marshal(evt)
			seq++
			id := fmt.Sprintf("%d-%d", evt.Timestamp, seq)
			sendSSE(writeCh, id, string(evt.Type), string(data))
		})
		unsubs = append(unsubs, unsub)
	}
	appendDeltaSub(sdk.EventReasoningDelta)
	appendDeltaSub(sdk.EventContentDelta)

	for _, t := range subTypes {
		t2 := t
		unsub := h.sdk.Subscribe(sdk.EventType(t2), func(evt *sdk.Event) {
			if evt.Type == sdk.EventToolCall {
				toolName, _ := evt.Payload["tool"].(string)
				log.Printf("[SSE] received tool_call event: tool=%s", toolName)
			}
			data, _ := json.Marshal(evt)
			seq++
			id := fmt.Sprintf("%d-%d", evt.Timestamp, seq)
			// 写入环状缓冲区，供断线重连重放
			if h.sseEvents != nil {
				h.sseEvents.Append(id, string(evt.Type), data)
			}
			sendSSE(writeCh, id, string(evt.Type), string(data))
			if evt.Type == sdk.EventToolCall {
				toolName, _ := evt.Payload["tool"].(string)
				log.Printf("[SSE] wrote tool_call to writeCh: tool=%s", toolName)
			}
		})
		unsubs = append(unsubs, unsub)
	}
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
		<-writerDone // 等 writer 退出，保证 handler 返回后无残余写入
	}()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// 心跳直接写 w 并 flush（绕过 writeCh，事件密集/队列满时也能保活长连接，
			// 避免远程 nginx 网关因长时间无字节而 504/半开）。
			if _, err := fmt.Fprintf(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

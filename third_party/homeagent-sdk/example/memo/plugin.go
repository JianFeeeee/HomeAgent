package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// Todo 待办条目：会被主动提醒
type Todo struct {
	ID        int64  `json:"id"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
	Done      bool   `json:"done"`
}

// Memo 备忘录条目：纯记事，不主动提醒
type Memo struct {
	ID        int64  `json:"id"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	mu       sync.RWMutex
	todos    []Todo
	nextTID  int64
	memos    []Memo
	nextMID  int64
	todoPath string
	memoPath string
	stopCh   chan struct{}
	tp       string
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.tp = p.name + "_"

	dataDirVal, err := s.Settings().GetCore("core.daemon.data_dir")
	if err != nil || dataDirVal == "" {
		dataDirVal = "."
	}
	dir := filepath.Join(fmt.Sprint(dataDirVal), p.name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[%s] mkdir data dir %s: %v", p.name, dir, err)
	}
	p.todoPath = filepath.Join(dir, "todos.json")
	p.memoPath = filepath.Join(dir, "memos.json")
	p.loadTodos()
	p.loadMemos()

	// 卸载（删除）时清理数据文件；重载不触发
	s.RegisterOnRemoveHandler(p.cleanupData)

	// ── 待办（会被主动提醒）──
	s.RegisterTool(p.tp+"todo_add", sdk.ToolDef{
		Name:        p.tp + "todo_add",
		Description: "添加一条待办事项。待办会被主动提醒，完成后请及时用 todo_complete 标记。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content": map[string]interface{}{"type": "string", "description": "待办内容"},
			},
			"required": []string{"content"},
		},
	}, p.handleTodoAdd)

	s.RegisterTool(p.tp+"todo_complete", sdk.ToolDef{
		Name:        p.tp + "todo_complete",
		Description: "将指定ID的待办标记为已完成（不再提醒）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "integer", "description": "待办ID"},
			},
			"required": []string{"id"},
		},
	}, p.handleTodoComplete)

	s.RegisterTool(p.tp+"todo_list", sdk.ToolDef{
		Name:        p.tp + "todo_list",
		Description: "列出所有未完成的待办事项，包含ID、内容和创建时间。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleTodoList)

	s.RegisterTool(p.tp+"todo_delete", sdk.ToolDef{
		Name:        p.tp + "todo_delete",
		Description: "删除指定ID的待办事项（包括已完成的）。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "integer", "description": "待办ID"},
			},
			"required": []string{"id"},
		},
	}, p.handleTodoDelete)

	// ── 备忘（纯记事，不提醒）──
	s.RegisterTool(p.tp+"memo_create", sdk.ToolDef{
		Name:        p.tp + "memo_create",
		Description: "创建一条备忘录。备忘录是纯记事（备注）用途，不会主动提醒，内容应包含完整信息供后续查阅。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content": map[string]interface{}{"type": "string", "description": "备忘录内容"},
			},
			"required": []string{"content"},
		},
	}, p.handleMemoCreate)

	s.RegisterTool(p.tp+"memo_list", sdk.ToolDef{
		Name:        p.tp + "memo_list",
		Description: "列出所有备忘录，包含ID、内容和创建时间。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleMemoList)

	s.RegisterTool(p.tp+"memo_delete", sdk.ToolDef{
		Name:        p.tp + "memo_delete",
		Description: "删除指定ID的备忘录。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "integer", "description": "备忘录ID"},
			},
			"required": []string{"id"},
		},
	}, p.handleMemoDelete)

	// 待办提醒：预动作注入未完成条数 + 周期主动提醒（备忘录不参与）
	s.RegisterStage(sdk.StagePreAction, p.stagePreAction)
	go p.periodicCheck()

	log.Printf("[%s] started, todos=%s memos=%s", p.name, p.todoPath, p.memoPath)
	return nil
}

func (p *Plugin) Stop() error {
	close(p.stopCh)
	p.saveTodos()
	p.saveMemos()
	log.Printf("[%s] stopped", p.name)
	return nil
}

func (p *Plugin) loadTodos() {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.todoPath)
	if err != nil {
		p.todos = []Todo{}
		p.nextTID = 1
		return
	}
	var store struct {
		Todos  []Todo `json:"todos"`
		NextID int64  `json:"next_id"`
	}
	if json.Unmarshal(data, &store) != nil {
		p.todos = []Todo{}
		p.nextTID = 1
		return
	}
	p.todos = store.Todos
	p.nextTID = store.NextID
	if p.todos == nil {
		p.todos = []Todo{}
	}
	if p.nextTID < 1 {
		p.nextTID = 1
	}
}

func (p *Plugin) loadMemos() {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.memoPath)
	if err != nil {
		p.memos = []Memo{}
		p.nextMID = 1
		return
	}
	var store struct {
		Memos  []Memo `json:"memos"`
		NextID int64  `json:"next_id"`
	}
	if json.Unmarshal(data, &store) != nil {
		p.memos = []Memo{}
		p.nextMID = 1
		return
	}
	p.memos = store.Memos
	p.nextMID = store.NextID
	if p.memos == nil {
		p.memos = []Memo{}
	}
	if p.nextMID < 1 {
		p.nextMID = 1
	}
}

func (p *Plugin) saveTodos() {
	p.mu.RLock()
	data, _ := json.MarshalIndent(map[string]interface{}{
		"todos":   p.todos,
		"next_id": p.nextTID,
	}, "", "  ")
	p.mu.RUnlock()
	atomicWriteJSON(p.todoPath, data)
}

func (p *Plugin) saveMemos() {
	p.mu.RLock()
	data, _ := json.MarshalIndent(map[string]interface{}{
		"memos":   p.memos,
		"next_id": p.nextMID,
	}, "", "  ")
	p.mu.RUnlock()
	atomicWriteJSON(p.memoPath, data)
}

// ── 待办：未完成计数与提醒 ──

func (p *Plugin) pendingTodoCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, t := range p.todos {
		if !t.Done {
			n++
		}
	}
	return n
}

func (p *Plugin) pendingTodos() []Todo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []Todo
	for _, t := range p.todos {
		if !t.Done {
			out = append(out, t)
		}
	}
	return out
}

// stagePreAction 仅在待办未完成时注入上下文提示（备忘录不提示）
func (p *Plugin) stagePreAction(ctx *sdk.StageContext) error {
	n := p.pendingTodoCount()
	if n == 0 {
		return nil
	}
	ctx.Lock()
	ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": fmt.Sprintf("目前有%d条待办未完成，调用%s todo_list 工具读取具体内容", n, p.tp),
	})
	ctx.Unlock()
	return nil
}

// periodicCheck 周期主动提醒未完成待办（备忘录不提醒）
func (p *Plugin) periodicCheck() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			n := p.pendingTodoCount()
			if n == 0 {
				continue
			}
			if p.sdk != nil {
				// NoMemory：这是定时提醒，不是记忆内容。
				p.sdk.InjectInterruptTextOpts(p.name, p.name,
					fmt.Sprintf("注意，你还有%d条待办未完成，请检查", n), sdk.InjectOptions{NoMemory: true})
			}
		}
	}
}

// ── 待办工具 ──

func (p *Plugin) handleTodoAdd(args map[string]interface{}) (interface{}, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return errorResult("content is required"), nil
	}

	p.mu.Lock()
	todo := Todo{
		ID:        p.nextTID,
		Content:   content,
		CreatedAt: time.Now().Unix(),
		Done:      false,
	}
	p.nextTID++
	p.todos = append(p.todos, todo)
	p.mu.Unlock()
	p.saveTodos()

	return map[string]interface{}{
		"content": fmt.Sprintf("待办已添加 (ID: %d)", todo.ID),
		"id":      todo.ID,
	}, nil
}

func (p *Plugin) handleTodoComplete(args map[string]interface{}) (interface{}, error) {
	id, ok := args["id"].(float64)
	if !ok {
		return errorResult("id is required"), nil
	}

	p.mu.Lock()
	found := false
	for i := range p.todos {
		if p.todos[i].ID == int64(id) && !p.todos[i].Done {
			p.todos[i].Done = true
			found = true
			break
		}
	}
	p.mu.Unlock()

	if !found {
		return errorResult(fmt.Sprintf("未找到未完成的待办 ID: %d", int64(id))), nil
	}
	p.saveTodos()

	return map[string]interface{}{
		"content": fmt.Sprintf("待办 %d 已标记为完成", int64(id)),
	}, nil
}

func (p *Plugin) handleTodoList(args map[string]interface{}) (interface{}, error) {
	todos := p.pendingTodos()
	if len(todos) == 0 {
		return map[string]interface{}{
			"content": "暂无未完成的待办",
		}, nil
	}

	var sb strings.Builder
	for i, t := range todos {
		ts := time.Unix(t.CreatedAt, 0).Format("01-02 15:04")
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("%d. [ID:%d] %s — %s", i+1, t.ID, t.Content, ts))
	}

	return map[string]interface{}{
		"content": sb.String(),
		"count":   len(todos),
	}, nil
}

func (p *Plugin) handleTodoDelete(args map[string]interface{}) (interface{}, error) {
	id, ok := args["id"].(float64)
	if !ok {
		return errorResult("id is required"), nil
	}

	p.mu.Lock()
	found := false
	for i := range p.todos {
		if p.todos[i].ID == int64(id) {
			p.todos = append(p.todos[:i], p.todos[i+1:]...)
			found = true
			break
		}
	}
	p.mu.Unlock()

	if !found {
		return errorResult(fmt.Sprintf("未找到待办 ID: %d", int64(id))), nil
	}
	p.saveTodos()

	return map[string]interface{}{
		"content": fmt.Sprintf("待办 %d 已删除", int64(id)),
	}, nil
}

// ── 备忘工具 ──

func (p *Plugin) handleMemoCreate(args map[string]interface{}) (interface{}, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return errorResult("content is required"), nil
	}

	p.mu.Lock()
	memo := Memo{
		ID:        p.nextMID,
		Content:   content,
		CreatedAt: time.Now().Unix(),
	}
	p.nextMID++
	p.memos = append(p.memos, memo)
	p.mu.Unlock()
	p.saveMemos()

	return map[string]interface{}{
		"content": fmt.Sprintf("备忘录已创建 (ID: %d)", memo.ID),
		"id":      memo.ID,
	}, nil
}

func (p *Plugin) handleMemoDelete(args map[string]interface{}) (interface{}, error) {
	id, ok := args["id"].(float64)
	if !ok {
		return errorResult("id is required"), nil
	}

	p.mu.Lock()
	found := false
	for i := range p.memos {
		if p.memos[i].ID == int64(id) {
			p.memos = append(p.memos[:i], p.memos[i+1:]...)
			found = true
			break
		}
	}
	p.mu.Unlock()

	if !found {
		return errorResult(fmt.Sprintf("未找到备忘录 ID: %d", int64(id))), nil
	}
	p.saveMemos()

	return map[string]interface{}{
		"content": fmt.Sprintf("备忘录 %d 已删除", int64(id)),
	}, nil
}

func (p *Plugin) handleMemoList(args map[string]interface{}) (interface{}, error) {
	p.mu.RLock()
	memos := append([]Memo{}, p.memos...)
	p.mu.RUnlock()

	if len(memos) == 0 {
		return map[string]interface{}{
			"content": "暂无备忘录",
		}, nil
	}

	var sb strings.Builder
	for i, m := range memos {
		ts := time.Unix(m.CreatedAt, 0).Format("01-02 15:04")
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("%d. [ID:%d] %s — %s", i+1, m.ID, m.Content, ts))
	}

	return map[string]interface{}{
		"content": sb.String(),
		"count":   len(memos),
	}, nil
}

func errorResult(msg string) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": msg,
	}
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name, stopCh: make(chan struct{})}, nil
}

// cleanupData 卸载时清理数据文件（待办 + 备忘）
func (p *Plugin) cleanupData() {
	if p.todoPath != "" {
		os.Remove(p.todoPath)
	}
	if p.memoPath != "" {
		os.Remove(p.memoPath)
	}
}

// atomicWriteJSON 原子写 JSON：先写临时文件再 rename，避免进程崩溃截断数据文件。
func atomicWriteJSON(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

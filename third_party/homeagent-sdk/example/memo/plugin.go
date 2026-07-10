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

type Memo struct {
	ID        int64  `json:"id"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
	Done      bool   `json:"done"`
}

type Plugin struct {
	name     string
	sdk      *sdk.PluginSDK
	mu       sync.RWMutex
	memos    []Memo
	nextID   int64
	filePath string
	stopCh   chan struct{}
	tp       string
}

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s
	p.tp = p.name + "_"
	p.stopCh = make(chan struct{})

	dataDirVal, err := s.Settings().GetCore("core.daemon.data_dir")
	if err != nil || dataDirVal == "" {
		dataDirVal = "."
	}
	p.filePath = filepath.Join(fmt.Sprint(dataDirVal), "memos.json")
	p.load()

	s.RegisterTool(p.tp+"create", sdk.ToolDef{
		Name:        p.tp + "create",
		Description: "创建一条备忘条目。备忘内容应包含具体事项的完整描述。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content": map[string]interface{}{"type": "string", "description": "备忘内容"},
			},
			"required": []string{"content"},
		},
	}, p.handleCreate)

	s.RegisterTool(p.tp+"complete", sdk.ToolDef{
		Name:        p.tp + "complete",
		Description: "将指定ID的备忘标记为已完成。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "integer", "description": "备忘ID"},
			},
			"required": []string{"id"},
		},
	}, p.handleComplete)

	s.RegisterTool(p.tp+"list", sdk.ToolDef{
		Name:        p.tp + "list",
		Description: "列出所有未完成的备忘条目，包含ID、内容和创建时间。",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleList)

	s.RegisterStage(sdk.StagePreAction, p.stagePreAction)

	go p.periodicCheck()

	log.Printf("[%s] started, path=%s", p.name, p.filePath)
	return nil
}

func (p *Plugin) Stop() error {
	close(p.stopCh)
	p.save()
	log.Printf("[%s] stopped", p.name)
	return nil
}

func (p *Plugin) load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.filePath)
	if err != nil {
		p.memos = nil
		p.nextID = 1
		return
	}
	var store struct {
		Memos  []Memo `json:"memos"`
		NextID int64  `json:"next_id"`
	}
	if json.Unmarshal(data, &store) != nil {
		p.memos = nil
		p.nextID = 1
		return
	}
	p.memos = store.Memos
	p.nextID = store.NextID
	if p.memos == nil {
		p.memos = []Memo{}
	}
	if p.nextID < 1 {
		p.nextID = 1
	}
}

func (p *Plugin) save() {
	data, _ := json.MarshalIndent(map[string]interface{}{
		"memos":   p.memos,
		"next_id": p.nextID,
	}, "", "  ")
	os.WriteFile(p.filePath, data, 0644)
}

func (p *Plugin) pendingCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, m := range p.memos {
		if !m.Done {
			n++
		}
	}
	return n
}

func (p *Plugin) pendingMemos() []Memo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []Memo
	for _, m := range p.memos {
		if !m.Done {
			out = append(out, m)
		}
	}
	return out
}

func (p *Plugin) stagePreAction(ctx *sdk.StageContext) error {
	n := p.pendingCount()
	if n == 0 {
		return nil
	}
	ctx.Lock()
	ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": fmt.Sprintf("目前有%d条备忘未完成，调用%slist工具读取具体内容", n, p.tp),
	})
	ctx.Unlock()
	return nil
}

func (p *Plugin) periodicCheck() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			n := p.pendingCount()
			if n == 0 {
				continue
			}
			if p.sdk != nil {
				p.sdk.InjectInterruptText(p.name, p.name,
					fmt.Sprintf("注意，你还有%d条备忘未标记完成，请检查", n))
			}
		}
	}
}

func (p *Plugin) handleCreate(args map[string]interface{}) (interface{}, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return errorResult("content is required"), nil
	}

	p.mu.Lock()
	memo := Memo{
		ID:        p.nextID,
		Content:   content,
		CreatedAt: time.Now().Unix(),
		Done:      false,
	}
	p.nextID++
	p.memos = append(p.memos, memo)
	p.mu.Unlock()
	p.save()

	return map[string]interface{}{
		"content": fmt.Sprintf("备忘已创建 (ID: %d)", memo.ID),
		"id":      memo.ID,
	}, nil
}

func (p *Plugin) handleComplete(args map[string]interface{}) (interface{}, error) {
	id, ok := args["id"].(float64)
	if !ok {
		return errorResult("id is required"), nil
	}

	p.mu.Lock()
	found := false
	for i := range p.memos {
		if p.memos[i].ID == int64(id) && !p.memos[i].Done {
			p.memos[i].Done = true
			found = true
			break
		}
	}
	p.mu.Unlock()

	if !found {
		return errorResult(fmt.Sprintf("未找到未完成的备忘 ID: %d", int64(id))), nil
	}
	p.save()

	return map[string]interface{}{
		"content": fmt.Sprintf("备忘 %d 已标记为完成", int64(id)),
	}, nil
}

func (p *Plugin) handleList(args map[string]interface{}) (interface{}, error) {
	memos := p.pendingMemos()
	if len(memos) == 0 {
		return map[string]interface{}{
			"content": "暂无未完成的备忘",
		}, nil
	}

	var sb strings.Builder
	for i, m := range memos {
		t := time.Unix(m.CreatedAt, 0).Format("01-02 15:04")
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("%d. [ID:%d] %s — %s", i+1, m.ID, m.Content, t))
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

package timer

import (
	"fmt"
	"log"
	"sync"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
	plugin.RegisterFactory("timer", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name   string
	mu     sync.Mutex
	wg     sync.WaitGroup
	stopCh chan struct{}
}

type timerTask struct {
	id      int
	dur     time.Duration
	message string
	doneAt  time.Time
	s       *sdk.PluginSDK
}

func New(name string) *Plugin {
	return &Plugin{name: name, stopCh: make(chan struct{})}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("timer_set", sdk.ToolDef{
		Name:        "timer_set",
		Description: "设置一个定时提醒。倒计时结束后通过中断通道通知 agent。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"duration": map[string]interface{}{
					"type":        "string",
					"description": "持续时间，例如 5s, 2m, 1h",
				},
				"message": map[string]interface{}{
					"type":        "string",
					"description": "提醒内容",
				},
			},
			"required": []string{"duration", "message"},
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		durStr, _ := args["duration"].(string)
		message, _ := args["message"].(string)
		if durStr == "" {
			return map[string]interface{}{"error": "duration is required"}, nil
		}
		if message == "" {
			return map[string]interface{}{"error": "message is required"}, nil
		}

		dur, err := time.ParseDuration(durStr)
		if err != nil {
			return map[string]interface{}{"error": fmt.Sprintf("invalid duration %q: %v", durStr, err)}, nil
		}

		p.mu.Lock()
		p.wg.Add(1)
		p.mu.Unlock()

		go func() {
			defer p.wg.Done()
			select {
			case <-time.After(dur):
				log.Printf("[timer] firing: %s (%s later)", message, dur)
				s.InjectInterruptText("timer", "timer", fmt.Sprintf("timer: %s", message))
			case <-p.stopCh:
				log.Printf("[timer] cancelled: %s", message)
			}
		}()

		doneAt := time.Now().Add(dur)
		return map[string]interface{}{
			"status":   "timer_set",
			"duration": durStr,
			"message":  message,
			"done_at":  doneAt.Format(time.RFC3339),
		}, nil
	})

	return nil
}

func (p *Plugin) Stop() error {
	close(p.stopCh)
	p.wg.Wait()
	return nil
}

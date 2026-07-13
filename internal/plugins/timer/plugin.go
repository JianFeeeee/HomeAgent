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
	plugin.RegisterPluginMeta("timer", "定时任务", "Timer")
	plugin.RegisterFactory("timer", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name   string
	mu     sync.Mutex
	wg     sync.WaitGroup
	stopCh chan struct{}
	maxDur time.Duration
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
	p.maxDur = 24 * time.Hour
	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "max_duration", Type: "string", DisplayName: "最大定时时长",
		Description: "允许设置的最大定时时长，例如 24h, 7d, 1h（默认 24h）",
		Default:     "24h",
	})
	if v, _ := s.Settings().Get("max_duration"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				p.maxDur = d
			}
		}
	}

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
		if dur > p.maxDur {
			return map[string]interface{}{"error": fmt.Sprintf("duration %v exceeds max %v", dur, p.maxDur)}, nil
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

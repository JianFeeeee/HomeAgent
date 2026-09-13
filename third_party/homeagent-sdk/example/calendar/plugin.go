package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

const (
	RepeatNone        = "none"
	RepeatDaily       = "daily"
	RepeatWeekday     = "weekday"
	RepeatWeekly      = "weekly"
	RepeatBiweekly    = "biweekly"
	RepeatMonthly     = "monthly"
	RepeatYearly      = "yearly"
	RepeatLunarYearly = "lunar_yearly"
)

type CalendarEvent struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	StartTime  string  `json:"start_time"`
	EndTime    string  `json:"end_time,omitempty"`
	AllDay     bool    `json:"all_day,omitempty"`
	Location   string  `json:"location,omitempty"`
	Note       string  `json:"note,omitempty"`
	Reminds    []int   `json:"reminds,omitempty"`
	RemindAt   []int64 `json:"remind_at,omitempty"`
	Repeat     string  `json:"repeat,omitempty"`
	ParentID   string  `json:"parent_id,omitempty"`
	Lunar      bool    `json:"lunar,omitempty"`
	LunarMonth int     `json:"lunar_month,omitempty"`
	LunarDay   int     `json:"lunar_day,omitempty"`
}

type Plugin struct {
	name         string
	sdk          *sdk.PluginSDK
	dataDir      string
	mu           sync.RWMutex
	events       []CalendarEvent
	nextEventID  int
	stopCh       chan struct{}
	wg           sync.WaitGroup
	remindTicker *time.Ticker
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name, stopCh: make(chan struct{})}, nil
}

func (p *Plugin) Name() string { return p.name }

func readCfg[T string | int64 | float64](s sdk.SettingsAPI, key string, fallback T) T {
	v, err := s.Get(key)
	if err == nil && v != nil {
		if sv, ok := v.(string); ok && sv != "" {
			switch any(fallback).(type) {
			case string:
				return any(sv).(T)
			case int64:
				if n, err := strconv.ParseInt(sv, 10, 64); err == nil {
					return any(n).(T)
				}
			case float64:
				if n, err := strconv.ParseFloat(sv, 64); err == nil {
					return any(n).(T)
				}
			}
		}
	}
	v2, err2 := s.GetCore("plugin." + "calendar" + "." + key)
	if err2 == nil && v2 != nil {
		if sv, ok := v2.(string); ok && sv != "" {
			switch any(fallback).(type) {
			case string:
				return any(sv).(T)
			case int64:
				if n, err := strconv.ParseInt(sv, 10, 64); err == nil {
					return any(n).(T)
				}
			case float64:
				if n, err := strconv.ParseFloat(sv, 64); err == nil {
					return any(n).(T)
				}
			}
		}
	}
	return fallback
}

func readArg[T string | int64 | float64](args map[string]interface{}, key string, fallback T) T {
	v, ok := args[key]
	if !ok || v == nil {
		return fallback
	}
	switch any(fallback).(type) {
	case string:
		if s, ok := v.(string); ok {
			return any(s).(T)
		}
	case int64:
		switch n := v.(type) {
		case float64:
			return any(int64(n)).(T)
		case int64:
			return any(n).(T)
		case string:
			if i, err := strconv.ParseInt(n, 10, 64); err == nil {
				return any(i).(T)
			}
		}
	case float64:
		switch n := v.(type) {
		case float64:
			return any(n).(T)
		case int64:
			return any(float64(n)).(T)
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return any(f).(T)
			}
		}
	}
	return fallback
}

func readArgBool(args map[string]interface{}, key string) bool {
	if v, ok := args[key]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
		if s, ok := v.(string); ok {
			return s == "1" || strings.EqualFold(s, "true")
		}
	}
	return false
}

// --- Time Helpers ---

var shortWeekday = map[time.Weekday]string{
	time.Monday: "一", time.Tuesday: "二", time.Wednesday: "三",
	time.Thursday: "四", time.Friday: "五", time.Saturday: "六", time.Sunday: "日",
}

func parseEventTime(s string) (time.Time, bool, bool) {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err == nil {
		return t, false, true
	}
	t, err = time.ParseInLocation("2006-01-02", s, time.Local)
	if err == nil {
		return t, true, true
	}
	return time.Time{}, false, false
}

// --- Lunar Calendar Engine ---

var lunarInfo = []int{
	0x04bd8, 0x04ae0, 0x0a570, 0x054d5, 0x0d260, 0x0d950, 0x16554, 0x056a0, 0x09ad0, 0x055d2,
	0x04ae0, 0x0a5b6, 0x0a4d0, 0x0d250, 0x1d255, 0x0b540, 0x0d6a0, 0x0ada2, 0x095b0, 0x14977,
	0x04970, 0x0a4b0, 0x0b4b5, 0x06a50, 0x06d40, 0x1ab54, 0x02b60, 0x09570, 0x052f2, 0x04970,
	0x06566, 0x0d4a0, 0x0ea50, 0x06e95, 0x05ad0, 0x02b60, 0x186e3, 0x092e0, 0x1c8d7, 0x0c950,
	0x0d4a0, 0x1d8a6, 0x0b550, 0x056a0, 0x1a5b4, 0x025d0, 0x092d0, 0x0d2b2, 0x0a950, 0x0b557,
	0x06ca0, 0x0b550, 0x15355, 0x04da0, 0x0a5b0, 0x14573, 0x052b0, 0x0a9a8, 0x0e950, 0x06aa0,
	0x0aea6, 0x0ab50, 0x04b60, 0x0aae4, 0x0a570, 0x05260, 0x0f263, 0x0d950, 0x05b57, 0x056a0,
	0x096d0, 0x04dd5, 0x04ad0, 0x0a4d0, 0x0d4d4, 0x0d250, 0x0d558, 0x0b540, 0x0b6a0, 0x195a6,
	0x095b0, 0x049b0, 0x0a974, 0x0a4b0, 0x0b27a, 0x06a50, 0x06d40, 0x0af46, 0x0ab60, 0x09570,
	0x04af5, 0x04970, 0x064b0, 0x074a3, 0x0ea50, 0x06b58, 0x05ac0, 0x0ab60, 0x096d5, 0x092e0,
	0x0c960, 0x0d954, 0x0d4a0, 0x0da50, 0x07552, 0x056a0, 0x0abb7, 0x025d0, 0x092d0, 0x0cab5,
	0x0a950, 0x0b4a0, 0x0baa4, 0x0ad50, 0x055d9, 0x04ba0, 0x0a5b0, 0x15176, 0x052b0, 0x0a930,
	0x07954, 0x06aa0, 0x0ad50, 0x05b52, 0x04b60, 0x0a6e6, 0x0a4e0, 0x0d260, 0x0ea65, 0x0d530,
	0x05aa0, 0x076a3, 0x096d0, 0x04afb, 0x04ad0, 0x0a4d0, 0x1d0b6, 0x0d250, 0x0d520, 0x0dd45,
	0x0b5a0, 0x056d0, 0x055b2, 0x049b0, 0x0a577, 0x0a4b0, 0x0aa50, 0x1b255, 0x06d20, 0x0ada0,
	0x14b63, 0x09370, 0x049f8, 0x04970, 0x064b0, 0x168a6, 0x0ea50, 0x06aa0, 0x1a6c4, 0x0aae0,
	0x092e0, 0x0d2e3, 0x0c960, 0x0d557, 0x0d4a0, 0x0da50, 0x05d55, 0x056a0, 0x0a6d0, 0x055d4,
	0x052d0, 0x0a9b8, 0x0a950, 0x0b4a0, 0x0b6a6, 0x0ad50, 0x055a0, 0x0aba4, 0x0a5b0, 0x052b0,
	0x0b273, 0x06930, 0x07337, 0x06aa0, 0x0ad50, 0x14b55, 0x04b60, 0x0a570, 0x054e4, 0x0d160,
	0x0e968, 0x0d520, 0x0daa0, 0x16aa6, 0x056d0, 0x04ae0, 0x0a9d4, 0x0a4d0, 0x0d150, 0x0f252,
	0x0d520,
}

func daysInLunarYear(year int) int {
	if year < 1900 || year > 2100 {
		return 365
	}
	y := lunarInfo[year-1900]
	sum := 0
	for i := 0x8000; i > 0x8; i >>= 1 {
		if y&i > 0 {
			sum += 30
		} else {
			sum += 29
		}
	}
	return sum + leapDays(year)
}

func leapMonth(year int) int {
	if year < 1900 || year > 2100 {
		return 0
	}
	return lunarInfo[year-1900] & 0xf
}

func leapDays(year int) int {
	if year < 1900 || year > 2100 {
		return 0
	}
	if leapMonth(year) == 0 {
		return 0
	}
	if lunarInfo[year-1900]&0x10000 > 0 {
		return 30
	}
	return 29
}

func monthDays(year, month int) int {
	if year < 1900 || year > 2100 || month < 1 || month > 12 {
		return 30
	}
	if lunarInfo[year-1900]&(0x10000>>month) > 0 {
		return 30
	}
	return 29
}

var baseSolar = func() time.Time { t, _ := time.ParseInLocation("2006-01-02", "1900-01-31", time.Local); return t }()

func lunarToSolar(year, month, day int) (time.Time, bool) {
	if year < 1900 || year > 2100 || month < 1 || month > 12 || day < 1 || day > 30 {
		return time.Time{}, false
	}
	offset := 0
	for y := 1900; y < year; y++ {
		offset += daysInLunarYear(y)
	}
	lm := leapMonth(year)
	_ = lm
	for m := 1; m < month; m++ {
		offset += monthDays(year, m)
	}
	offset += day - 1
	solar := baseSolar.AddDate(0, 0, offset)
	return solar, true
}

func nextLunarYearly(targetMonth, targetDay int, after time.Time) (time.Time, bool) {
	afterYear := after.Year()
	for y := afterYear; y <= afterYear+2; y++ {
		t, ok := lunarToSolar(y, targetMonth, targetDay)
		if !ok {
			continue
		}
		if t.After(after) {
			return t, true
		}
	}
	return time.Time{}, false
}

// --- Plugin Lifecycle ---

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	// 入站通道：本插件用 "calendar" 通道注入输入（见 Inject* 调用），
	// 输入侧必须显式登记 —— 否则"把该 inputch 划给驻留子"会报 `inputch 未注册`。
	_ = s.RegisterInputChannel("calendar", sdk.ChannelDef{NoMemory: true})
	dataDirVal, err := s.Settings().GetCore("core.daemon.data_dir")
	if err != nil || dataDirVal == "" {
		dataDirVal = "."
	}
	p.dataDir = filepath.Join(fmt.Sprint(dataDirVal), "calendar")
	if err := os.MkdirAll(p.dataDir, 0755); err != nil {
		fmt.Printf("[%s] mkdir %s: %v\n", p.name, p.dataDir, err)
	}
	p.loadEvents()

	// 持久化交由 stop handler：内核会在调用 Stop() 之前执行，
	// 避免 Stop() 阶段以陈旧内存写回导致已删除事件复活。
	s.RegisterStopHandler(p.saveEvents)
	// 删除清理：卸载插件时移除本地事件数据文件（删除专用回调，重载不触发）。
	s.RegisterOnRemoveHandler(p.cleanupData)

	tp := p.name + "_"

	s.RegisterTool(tp+"event_add", sdk.ToolDef{
		Name: tp + "event_add", Description: "Add a calendar event. Time: YYYY-MM-DD HH:MM or YYYY-MM-DD for all-day.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"title":         map[string]interface{}{"type": "string", "description": "Event title"},
				"start_time":    map[string]interface{}{"type": "string", "description": "Start time (YYYY-MM-DD HH:MM or YYYY-MM-DD)"},
				"end_time":      map[string]interface{}{"type": "string", "description": "End time (optional)"},
				"location":      map[string]interface{}{"type": "string", "description": "Location (optional)"},
				"note":          map[string]interface{}{"type": "string", "description": "Notes (optional)"},
				"remind_before": map[string]interface{}{"type": "string", "description": "Reminder minutes before event. Multiple: comma-separated, e.g. '15,60,1440' for 15min + 1hr + 1day before. 0 or empty = no reminder."},
				"repeat":        map[string]interface{}{"type": "string", "description": "Repeat: none, daily, weekday, weekly, biweekly, monthly, yearly, lunar_yearly"},
				"lunar":         map[string]interface{}{"type": "boolean", "description": "Whether the date is lunar calendar. If true, repeat=lunar_yearly by default. Also set lunar_month and lunar_day."},
				"lunar_month":   map[string]interface{}{"type": "integer", "description": "Lunar month (1-12), required when lunar=true"},
				"lunar_day":     map[string]interface{}{"type": "integer", "description": "Lunar day (1-30), required when lunar=true"},
			},
			"required": []string{"title", "start_time"},
		},
	}, p.handleEventAdd)

	s.RegisterTool(tp+"event_list", sdk.ToolDef{
		Name: tp + "event_list", Description: "List upcoming events. Shows date, time, repeat pattern.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"days": map[string]interface{}{"type": "integer", "description": "Days ahead (default 7, max 365)"},
			},
		},
	}, p.handleEventList)

	s.RegisterTool(tp+"event_delete", sdk.ToolDef{
		Name: tp + "event_delete", Description: "Delete an event by ID. Deletes this and all future recurrences.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "Event ID"},
			},
			"required": []string{"id"},
		},
	}, p.handleEventDelete)

	s.RegisterTool(tp+"event_update", sdk.ToolDef{
		Name: tp + "event_update", Description: "Update an event. Only provided fields change. Resets reminder state.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":            map[string]interface{}{"type": "string", "description": "Event ID"},
				"title":         map[string]interface{}{"type": "string", "description": "New title"},
				"start_time":    map[string]interface{}{"type": "string", "description": "New start time"},
				"end_time":      map[string]interface{}{"type": "string", "description": "New end time"},
				"location":      map[string]interface{}{"type": "string", "description": "New location"},
				"note":          map[string]interface{}{"type": "string", "description": "New notes"},
				"remind_before": map[string]interface{}{"type": "string", "description": "New reminder minutes (comma-separated)"},
				"repeat":        map[string]interface{}{"type": "string", "description": "New repeat type"},
				"lunar":         map[string]interface{}{"type": "boolean", "description": "Whether lunar calendar"},
				"lunar_month":   map[string]interface{}{"type": "integer", "description": "Lunar month 1-12"},
				"lunar_day":     map[string]interface{}{"type": "integer", "description": "Lunar day 1-30"},
			},
			"required": []string{"id"},
		},
	}, p.handleEventUpdate)

	s.RegisterTool(tp+"today", sdk.ToolDef{
		Name: tp + "today", Description: "Show today's events with countdown.",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleToday)

	s.RegisterTool(tp+"week", sdk.ToolDef{
		Name: tp + "week", Description: "Show this week's events grouped by day.",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, p.handleWeek)

	s.RegisterTool(tp+"month", sdk.ToolDef{
		Name: tp + "month", Description: "Show a month calendar grid with event dots.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"year":  map[string]interface{}{"type": "integer", "description": "Year (default: current)"},
				"month": map[string]interface{}{"type": "integer", "description": "Month 1-12 (default: current)"},
			},
		},
	}, p.handleMonth)

	s.RegisterTool(tp+"search", sdk.ToolDef{
		Name: tp + "search", Description: "Search events by keyword in title, location, or notes.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"keyword": map[string]interface{}{"type": "string", "description": "Search keyword"},
			},
			"required": []string{"keyword"},
		},
	}, p.handleSearch)

	p.remindTicker = time.NewTicker(30 * time.Second)
	p.wg.Add(1)
	go p.remindLoop()

	fmt.Printf("[%s] started (%d events)\n", p.name, len(p.events))
	return nil
}

func (p *Plugin) Stop() error {
	p.remindTicker.Stop()
	close(p.stopCh)
	p.wg.Wait()
	fmt.Printf("[%s] stopped\n", p.name)
	return nil
}

// --- Reminder Loop ---

func (p *Plugin) remindLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.remindTicker.C:
			p.checkReminders()
		case <-p.stopCh:
			return
		}
	}
}

func (p *Plugin) checkReminders() {
	now := time.Now()

	p.mu.Lock()

	changed := false
	var injectMsgs []string

	for i := range p.events {
		e := &p.events[i]
		evtTime, allDay, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if allDay || evtTime.Before(now) {
			continue
		}

		for ri, remindMin := range e.Reminds {
			if remindMin <= 0 {
				continue
			}
			if ri < len(e.RemindAt) && e.RemindAt[ri] > 0 {
				continue
			}
			remindAt := evtTime.Add(-time.Duration(remindMin) * time.Minute)
			if !now.After(remindAt) && !now.Equal(remindAt) {
				continue
			}
			if len(e.RemindAt) <= ri {
				e.RemindAt = append(e.RemindAt, make([]int64, ri+1-len(e.RemindAt))...)
			}
			e.RemindAt[ri] = remindAt.Unix()
			changed = true
			timeUntil := evtTime.Sub(now).Round(time.Minute)
			msg := fmt.Sprintf("⏰ 提醒: %s (%s)", e.Title, e.StartTime)
			if timeUntil > 0 {
				msg += fmt.Sprintf(" (还有%s)", timeUntil)
			}
			if len(e.Reminds) > 1 {
				msg += fmt.Sprintf(" [第%d次提醒]", ri+1)
			}
			if e.Location != "" {
				msg += fmt.Sprintf("\n📍 %s", e.Location)
			}
			if e.Note != "" {
				msg += fmt.Sprintf("\n📝 %s", e.Note)
			}
			injectMsgs = append(injectMsgs, msg)
		}
	}

	newEvents := []CalendarEvent{}
	for i := range p.events {
		e := &p.events[i]
		evtTime, _, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if !now.After(evtTime) {
			continue
		}
		if e.Repeat == "" || e.Repeat == RepeatNone {
			continue
		}
		next := p.nextOccurrence(*e, evtTime)
		if next != nil {
			pid := e.ID
			if e.ParentID != "" {
				pid = e.ParentID
			}
			next.ParentID = pid
			dup := false
			for _, ev := range p.events {
				if ev.ID != e.ID && ev.ParentID == pid && ev.StartTime == next.StartTime {
					dup = true
					break
				}
			}
			if !dup {
				newEvents = append(newEvents, *next)
				changed = true
			}
		}
	}
	if len(newEvents) > 0 {
		p.events = append(p.events, newEvents...)
	}

	p.cleanupPastEvents()
	if changed {
		p.saveEventsLocked()
	}
	p.mu.Unlock()

	for _, msg := range injectMsgs {
		// NoMemory：日程到点提醒，不是记忆内容。
		p.sdk.InjectInterruptTextOpts("calendar", "calendar", msg, sdk.InjectOptions{NoMemory: true})
	}
}

func (p *Plugin) nextOccurrence(e CalendarEvent, evtTime time.Time) *CalendarEvent {
	var next time.Time
	switch e.Repeat {
	case RepeatDaily:
		next = evtTime.AddDate(0, 0, 1)
	case RepeatWeekday:
		next = evtTime.AddDate(0, 0, 1)
		for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
			next = next.AddDate(0, 0, 1)
		}
	case RepeatWeekly:
		next = evtTime.AddDate(0, 0, 7)
	case RepeatBiweekly:
		next = evtTime.AddDate(0, 0, 14)
	case RepeatMonthly:
		next = evtTime.AddDate(0, 1, 0)
	case RepeatYearly:
		next = evtTime.AddDate(1, 0, 0)
	case RepeatLunarYearly:
		if e.LunarMonth > 0 && e.LunarDay > 0 {
			t, ok := nextLunarYearly(e.LunarMonth, e.LunarDay, evtTime)
			if ok {
				next = t
			} else {
				return nil
			}
		} else {
			return nil
		}
	default:
		return nil
	}

	timeStr := next.Format("2006-01-02 15:04")
	if e.AllDay {
		timeStr = next.Format("2006-01-02")
	}

	reminds := make([]int, len(e.Reminds))
	copy(reminds, e.Reminds)

	return &CalendarEvent{
		ID:        fmt.Sprintf("evt_%d_%d", next.Unix(), p.nextEventID),
		Title:     e.Title,
		StartTime: timeStr,
		EndTime:   e.EndTime,
		AllDay:    e.AllDay,
		Location:  e.Location,
		Note:      e.Note,
		Reminds:   reminds,
		Repeat:    e.Repeat,
		ParentID:  e.ParentID,
	}
}

func (p *Plugin) cleanupPastEvents() {
	now := time.Now()
	keep := []CalendarEvent{}
	for _, e := range p.events {
		evtTime, _, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if !now.After(evtTime) {
			keep = append(keep, e)
			continue
		}
		_ = e // 过时重复事件不再保留：next 已由 nextOccurrence 追加
	}
	p.events = keep
}

// --- Persistence ---

func (p *Plugin) eventsFile() string {
	return filepath.Join(p.dataDir, "events.json")
}

// cleanupData 删除插件时清理本地持久化数据文件。
func (p *Plugin) cleanupData() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.Remove(p.eventsFile()); err != nil && !os.IsNotExist(err) {
		fmt.Printf("[calendar] onRemove cleanup: %v\n", err)
	} else {
		fmt.Printf("[calendar] onRemove removed %s\n", p.eventsFile())
	}
}

func (p *Plugin) loadEvents() {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := os.ReadFile(p.eventsFile())
	if err != nil {
		p.events = nil
		p.nextEventID = 1
		return
	}
	var data struct {
		Events      []CalendarEvent `json:"events"`
		NextEventID int             `json:"next_id"`
	}
	if json.Unmarshal(b, &data) != nil {
		p.events = nil
		p.nextEventID = 1
		return
	}
	p.events = data.Events
	p.nextEventID = data.NextEventID
	if p.nextEventID < 1 {
		p.nextEventID = 1
	}
	if p.events == nil {
		p.events = []CalendarEvent{}
	}
}

func (p *Plugin) saveEvents() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	p.saveEventsLocked()
}

func (p *Plugin) saveEventsLocked() {
	data := struct {
		Events      []CalendarEvent `json:"events"`
		NextEventID int             `json:"next_id"`
	}{
		Events:      p.events,
		NextEventID: p.nextEventID,
	}
	b, _ := json.MarshalIndent(data, "", "  ")
	atomicWriteJSON(p.eventsFile(), b)
}

// --- Helper: parse remind_before ---

func parseReminds(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return nil
	}
	parts := strings.Split(s, ",")
	vals := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v <= 0 {
			continue
		}
		vals = append(vals, v)
	}
	sort.Ints(vals)
	return vals
}

// --- Helper: format event duration ---

func formatTimeUntil(t time.Time) string {
	now := time.Now()
	if t.Before(now) {
		return "已开始"
	}
	d := t.Sub(now)
	if d < time.Hour {
		m := int(d.Minutes())
		return fmt.Sprintf("还有%d分钟", m)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m > 0 {
			return fmt.Sprintf("还有%d小时%d分", h, m)
		}
		return fmt.Sprintf("还有%d小时", h)
	}
	d2 := int(d.Hours() / 24)
	return fmt.Sprintf("还有%d天", d2)
}

// --- Tool: event_add ---

func (p *Plugin) handleEventAdd(args map[string]interface{}) (interface{}, error) {
	title := readArg(args, "title", "")
	startTime := readArg(args, "start_time", "")
	if title == "" || startTime == "" {
		return map[string]interface{}{"isError": true, "content": "title and start_time are required"}, nil
	}

	parsedStart, allDay, ok := parseEventTime(startTime)
	if !ok {
		return map[string]interface{}{"isError": true, "content": "Invalid start_time. Use YYYY-MM-DD HH:MM or YYYY-MM-DD."}, nil
	}

	endTime := readArg(args, "end_time", "")
	if endTime != "" {
		if _, _, ok := parseEventTime(endTime); !ok {
			return map[string]interface{}{"isError": true, "content": "Invalid end_time."}, nil
		}
	}

	location := readArg(args, "location", "")
	note := readArg(args, "note", "")
	remindStr := readArg(args, "remind_before", "")
	reminds := parseReminds(remindStr)
	lunar := readArgBool(args, "lunar")
	lunarMonth := int(readArg(args, "lunar_month", int64(0)))
	lunarDay := int(readArg(args, "lunar_day", int64(0)))

	repeat := readArg(args, "repeat", RepeatNone)
	if lunar && repeat == RepeatNone {
		repeat = RepeatLunarYearly
	}
	switch repeat {
	case RepeatNone, RepeatDaily, RepeatWeekday, RepeatWeekly, RepeatBiweekly, RepeatMonthly, RepeatYearly, RepeatLunarYearly:
	default:
		repeat = RepeatNone
	}

	if lunar && (lunarMonth < 1 || lunarMonth > 12 || lunarDay < 1 || lunarDay > 30) {
		return map[string]interface{}{"isError": true, "content": "lunar_month (1-12) and lunar_day (1-30) required when lunar=true"}, nil
	}

	event := CalendarEvent{
		ID:         fmt.Sprintf("evt_%d_%d", parsedStart.Unix(), p.nextEventID),
		Title:      title,
		StartTime:  startTime,
		EndTime:    endTime,
		AllDay:     allDay,
		Location:   location,
		Note:       note,
		Reminds:    reminds,
		Repeat:     repeat,
		Lunar:      lunar,
		LunarMonth: lunarMonth,
		LunarDay:   lunarDay,
	}

	p.mu.Lock()
	p.events = append(p.events, event)
	p.nextEventID++
	p.mu.Unlock()
	p.saveEvents()

	detail := fmt.Sprintf("Event added: %s (ID: %s)", title, event.ID)
	if len(reminds) > 0 {
		parts := make([]string, len(reminds))
		for i, r := range reminds {
			parts[i] = fmt.Sprintf("%dmin", r)
		}
		detail += fmt.Sprintf(" | 提醒: %s", strings.Join(parts, ", "))
	}
	if repeat != RepeatNone {
		detail += " | 重复: " + repeat
	}
	return map[string]interface{}{"content": detail}, nil
}

// --- Tool: event_list ---

func (p *Plugin) handleEventList(args map[string]interface{}) (interface{}, error) {
	days := int(readArg(args, "days", int64(7)))
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}

	now := time.Now()
	cutoff := now.AddDate(0, 0, days)

	p.mu.RLock()
	upcoming := make([]CalendarEvent, 0)
	for _, e := range p.events {
		evtTime, _, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if evtTime.Before(cutoff) && evtTime.After(now.Add(-24*time.Hour)) {
			upcoming = append(upcoming, e)
		}
	}
	p.mu.RUnlock()

	sort.Slice(upcoming, func(i, j int) bool {
		return upcoming[i].StartTime < upcoming[j].StartTime
	})

	if len(upcoming) == 0 {
		return map[string]interface{}{"content": fmt.Sprintf("No events in the next %d days.", days)}, nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("📋 Events (%d):", len(upcoming)))
	for _, e := range upcoming {
		timeStr := e.StartTime
		if e.EndTime != "" {
			timeStr += " → " + e.EndTime
		}
		extra := ""
		if e.Location != "" {
			extra += " 📍" + e.Location
		}
		if len(e.Reminds) > 0 {
			parts := make([]string, len(e.Reminds))
			for i, r := range e.Reminds {
				parts[i] = fmt.Sprintf("%d′", r)
			}
			extra += " 🔔" + strings.Join(parts, ",")
		}
		if e.Lunar {
			extra += fmt.Sprintf(" 🌙%d-%d", e.LunarMonth, e.LunarDay)
		}
		if e.Repeat != "" && e.Repeat != RepeatNone {
			extra += " 🔄" + e.Repeat
		}
		if e.Note != "" {
			extra += " 📝" + e.Note
		}
		lines = append(lines, fmt.Sprintf("  [%s] %s%s", timeStr, e.Title, extra))
	}

	return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
}

// --- Tool: event_delete ---

func (p *Plugin) handleEventDelete(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	if id == "" {
		return map[string]interface{}{"isError": true, "content": "Event ID is required"}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	found := false
	remaining := []CalendarEvent{}
	for _, e := range p.events {
		if e.ID == id {
			found = true
			continue
		}
		pid := e.ParentID
		if pid == "" {
			pid = e.ID
		}
		if pid == id {
			continue
		}
		remaining = append(remaining, e)
	}
	if !found {
		return map[string]interface{}{"isError": true, "content": "Event not found: " + id}, nil
	}
	p.events = remaining
	p.saveEventsLocked()
	return map[string]interface{}{"content": "Deleted event and all recurrences: " + id}, nil
}

// --- Tool: event_update ---

func (p *Plugin) handleEventUpdate(args map[string]interface{}) (interface{}, error) {
	id := readArg(args, "id", "")
	if id == "" {
		return map[string]interface{}{"isError": true, "content": "Event ID is required"}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range p.events {
		if p.events[i].ID != id {
			continue
		}
		e := &p.events[i]

		if v := readArg(args, "title", ""); v != "" {
			e.Title = v
		}
		if v := readArg(args, "start_time", ""); v != "" {
			if _, allDay, ok := parseEventTime(v); ok {
				e.StartTime = v
				e.AllDay = allDay
			}
		}
		if v := readArg(args, "end_time", ""); v != "" {
			if _, _, ok := parseEventTime(v); ok {
				e.EndTime = v
			}
		}
		if v := readArg(args, "location", ""); v != "" {
			e.Location = v
		}
		if v := readArg(args, "note", ""); v != "" {
			e.Note = v
		}
		if v := readArg(args, "remind_before", ""); v != "" {
			e.Reminds = parseReminds(v)
		}
		if v := readArg(args, "repeat", ""); v != "" {
			switch v {
			case RepeatNone, RepeatDaily, RepeatWeekday, RepeatWeekly, RepeatBiweekly, RepeatMonthly, RepeatYearly, RepeatLunarYearly:
				e.Repeat = v
			}
		}
		if v, ok := args["lunar"]; ok && v != nil {
			if b, ok := v.(bool); ok {
				e.Lunar = b
			} else if s, ok := v.(string); ok {
				e.Lunar = s == "1" || strings.EqualFold(s, "true")
			}
		}
		if v := readArg(args, "lunar_month", int64(0)); v > 0 {
			e.LunarMonth = int(v)
		}
		if v := readArg(args, "lunar_day", int64(0)); v > 0 {
			e.LunarDay = int(v)
		}
		e.RemindAt = nil

		p.saveEventsLocked()
		return map[string]interface{}{"content": "Event updated: " + e.Title}, nil
	}

	return map[string]interface{}{"isError": true, "content": "Event not found: " + id}, nil
}

// --- Tool: today ---

func (p *Plugin) handleToday(args map[string]interface{}) (interface{}, error) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayEnd := todayStart.AddDate(0, 0, 1)

	p.mu.RLock()
	events := make([]CalendarEvent, 0)
	for _, e := range p.events {
		evtTime, _, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if evtTime.After(todayStart.Add(-time.Hour)) && evtTime.Before(todayEnd) {
			events = append(events, e)
		}
	}
	p.mu.RUnlock()

	sort.Slice(events, func(i, j int) bool {
		return events[i].StartTime < events[j].StartTime
	})

	dateStr := now.Format("2006-01-02")
	weekday := shortWeekday[now.Weekday()]
	lines := []string{fmt.Sprintf("📅 %s 周%s — 今天", dateStr, weekday)}

	if len(events) == 0 {
		lines = append(lines, "  今天没有事件")
		return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
	}

	for _, e := range events {
		evtTime, _, _ := parseEventTime(e.StartTime)
		timeStr := e.StartTime
		if now.Format("2006-01-02") == evtTime.Format("2006-01-02") {
			timeStr = evtTime.Format("15:04")
		}
		countdown := formatTimeUntil(evtTime)
		detail := fmt.Sprintf("  %s — %s (%s)", timeStr, e.Title, countdown)
		if e.AllDay {
			detail = fmt.Sprintf("  🌙 %s (全天)", e.Title)
		}
		if e.Location != "" {
			detail += " 📍" + e.Location
		}
		lines = append(lines, detail)
	}

	return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
}

// --- Tool: week ---

func (p *Plugin) handleWeek(args map[string]interface{}) (interface{}, error) {
	now := time.Now()
	weekStart := now.AddDate(0, 0, -int(now.Weekday()-time.Monday))
	if now.Weekday() == time.Sunday {
		weekStart = now.AddDate(0, 0, -6)
	}
	weekEnd := weekStart.AddDate(0, 0, 7)

	p.mu.RLock()
	dayEvents := make(map[string][]CalendarEvent)
	for _, e := range p.events {
		evtTime, _, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if evtTime.After(weekStart.Add(-time.Hour)) && evtTime.Before(weekEnd) {
			dayKey := evtTime.Format("2006-01-02")
			dayEvents[dayKey] = append(dayEvents[dayKey], e)
		}
	}
	p.mu.RUnlock()

	for k := range dayEvents {
		sort.Slice(dayEvents[k], func(i, j int) bool {
			return dayEvents[k][i].StartTime < dayEvents[k][j].StartTime
		})
	}

	lines := []string{fmt.Sprintf("📅 %s ～ %s", weekStart.Format("01-02"), weekEnd.AddDate(0, 0, -1).Format("01-02"))}
	eventCount := 0
	for i := 0; i < 7; i++ {
		d := weekStart.AddDate(0, 0, i)
		dayKey := d.Format("2006-01-02")
		wd := shortWeekday[d.Weekday()]
		prefix := "  "
		if d.Format("2006-01-02") == now.Format("2006-01-02") {
			prefix = "▶"
		}
		line := fmt.Sprintf("%s %s %s", prefix, d.Format("01-02"), wd)
		if evts, ok := dayEvents[dayKey]; ok && len(evts) > 0 {
			titles := make([]string, len(evts))
			for i, e := range evts {
				timeStr := e.StartTime
				if !e.AllDay {
					timeStr = parseTimeShort(e.StartTime)
				} else {
					timeStr = "全天"
				}
				titles[i] = fmt.Sprintf("%s %s", timeStr, e.Title)
				eventCount++
			}
			line += " " + strings.Join(titles, ", ")
		}
		lines = append(lines, line)
	}
	if eventCount == 0 {
		lines = append(lines, "  本周没有事件")
	}

	return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
}

func parseTimeShort(s string) string {
	t, _, ok := parseEventTime(s)
	if !ok {
		return s
	}
	return t.Format("15:04")
}

// --- Tool: month ---

func (p *Plugin) handleMonth(args map[string]interface{}) (interface{}, error) {
	now := time.Now()
	year := int(readArg(args, "year", int64(now.Year())))
	month := int(readArg(args, "month", int64(now.Month())))
	if month < 1 || month > 12 {
		month = int(now.Month())
	}

	firstDay := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, now.Location())
	lastDay := firstDay.AddDate(0, 1, -1)
	daysInMonth := lastDay.Day()
	startWeekday := int(firstDay.Weekday())
	if startWeekday == 0 {
		startWeekday = 7
	}

	p.mu.RLock()
	daySet := make(map[int]bool)
	for _, e := range p.events {
		evtTime, _, ok := parseEventTime(e.StartTime)
		if !ok {
			continue
		}
		if evtTime.Year() == year && evtTime.Month() == time.Month(month) {
			daySet[evtTime.Day()] = true
		}
	}
	p.mu.RUnlock()

	monthName := firstDay.Format("January")
	lines := []string{fmt.Sprintf("📅 %d年%d月 (%s)", year, month, monthName)}
	lines = append(lines, "  一  二  三  四  五  六  日")
	lines = append(lines, "")

	row := " "
	for i := 1; i < startWeekday; i++ {
		row += "    "
	}
	for d := 1; d <= daysInMonth; d++ {
		mark := " "
		if daySet[d] {
			mark = "•"
		}
		row += fmt.Sprintf(" %2d%s", d, mark)
		wd := startWeekday - 1 + d
		if wd%7 == 0 || d == daysInMonth {
			lines = append(lines, row)
			row = " "
		}
	}

	count := 0
	for d := range daySet {
		count++
		_ = d
	}
	lines = append(lines, fmt.Sprintf("\n本月 %d 天有事件", count))
	return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
}

// --- Tool: search ---

func (p *Plugin) handleSearch(args map[string]interface{}) (interface{}, error) {
	keyword := strings.ToLower(readArg(args, "keyword", ""))
	if keyword == "" {
		return map[string]interface{}{"isError": true, "content": "keyword is required"}, nil
	}

	p.mu.RLock()
	results := make([]CalendarEvent, 0)
	for _, e := range p.events {
		if strings.Contains(strings.ToLower(e.Title), keyword) ||
			strings.Contains(strings.ToLower(e.Location), keyword) ||
			strings.Contains(strings.ToLower(e.Note), keyword) {
			results = append(results, e)
		}
	}
	p.mu.RUnlock()

	sort.Slice(results, func(i, j int) bool {
		return results[i].StartTime < results[j].StartTime
	})

	if len(results) == 0 {
		return map[string]interface{}{"content": fmt.Sprintf("No events match: %s", keyword)}, nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("🔍 Found %d events for \"%s\":", len(results), keyword))
	for _, e := range results {
		lines = append(lines, fmt.Sprintf("  [%s] %s (ID: %s)", e.StartTime, e.Title, e.ID))
	}
	return map[string]interface{}{"content": strings.Join(lines, "\n")}, nil
}

// atomicWriteJSON 原子写 JSON：先写临时文件再 rename，避免进程崩溃截断数据文件。
func atomicWriteJSON(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

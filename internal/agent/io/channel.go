package io

import (
	"fmt"
	"log"
	"sync"
	"time"
)

type DeviceType int

const (
	DeviceInput  DeviceType = 0
	DeviceOutput DeviceType = 1
	DeviceIO     DeviceType = 2
)

// OutputCapability 定义通道支持的输出格式
type OutputCapability int

const (
	CapText      OutputCapability = 1 << iota // 文本
	CapFile                                   // 文件
	CapImage                                  // 图片
	CapAudio                                  // 音频
	CapStructured                             // 结构化数据（JSON/卡片）
)

func (c OutputCapability) Supports(cap OutputCapability) bool {
	return c&cap != 0
}

func (c OutputCapability) String() string {
	var flags []string
	if c&CapText != 0 {
		flags = append(flags, "text")
	}
	if c&CapFile != 0 {
		flags = append(flags, "file")
	}
	if c&CapImage != 0 {
		flags = append(flags, "image")
	}
	if c&CapAudio != 0 {
		flags = append(flags, "audio")
	}
	if c&CapStructured != 0 {
		flags = append(flags, "structured")
	}
	return fmt.Sprintf("%v", flags)
}

type Device interface {
	Name() string
	Type() DeviceType
	Description() string
	Tools() []ToolDef
	Execute(tool string, args map[string]interface{}) (interface{}, error)
	Start() error
	Stop() error
	OutputCapabilities() OutputCapability
}

type ToolHandler func(args map[string]interface{}) (interface{}, error)

type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
	Handler     ToolHandler            `json:"-"` // 可选：插件工具的直接处理器，Device 通过 Execute() 分发
}

type InputEvent struct {
	RequestID     string                 `json:"request_id"`
	Source        string                 `json:"source"`
	Type          string                 `json:"type"`
	Payload       map[string]interface{} `json:"payload"`
	ResponseCh    chan<- *OutputEvent    `json:"-"`
	OutputChannel string                 `json:"output_channel"` // 默认输出通道（不传则等于 Source）
}

type OutputEvent struct {
	RequestID     string                 `json:"request_id"`
	Target        string                 `json:"target"`
	Type          string                 `json:"type"`
	Payload       map[string]interface{} `json:"payload"`
	Done          bool                   `json:"done,omitempty"`
	OutputChannel string                 `json:"output_channel"` // 路由到此通道
}

type IOManager struct {
	mu           sync.RWMutex
	devices      map[string]Device
	inputCh      chan *InputEvent
	interruptCh  chan *InputEvent
	outputCh     chan *OutputEvent
	nextReqID    int64
}

func NewIOManager() *IOManager {
	return &IOManager{
		devices:     make(map[string]Device),
		inputCh:     make(chan *InputEvent, 256),
		interruptCh: make(chan *InputEvent, 64),
		outputCh:    make(chan *OutputEvent, 256),
	}
}

func (m *IOManager) UnregisterDevice(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.devices, name)
}

func (m *IOManager) nextRequestID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextReqID++
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), m.nextReqID)
}

// AtomicSwapDevices 原子化替换全部 IO 设备
// 1. 新设备必须在调用前已完成 Start()
// 2. 调用后旧设备立即摘除，新请求走向新设备
// 3. 返回旧设备列表，由调用方负责 Stop()
func (m *IOManager) AtomicSwapDevices(newDevices map[string]Device) map[string]Device {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldDevices := m.devices
	m.devices = newDevices

	return oldDevices
}

func (m *IOManager) RegisterDevice(dev Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.devices[dev.Name()]; ok {
		return fmt.Errorf("device %s already registered", dev.Name())
	}
	m.devices[dev.Name()] = dev
	return nil
}

func (m *IOManager) StartAll() error {
	m.mu.RLock()
	devices := make([]Device, 0, len(m.devices))
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}
	m.mu.RUnlock()

	for _, dev := range devices {
		if err := dev.Start(); err != nil {
			return fmt.Errorf("start device %s: %w", dev.Name(), err)
		}
	}
	return nil
}

func (m *IOManager) StopAll() {
	m.mu.RLock()
	devices := make([]Device, 0, len(m.devices))
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}
	m.mu.RUnlock()

	for _, dev := range devices {
		if err := dev.Stop(); err != nil {
			log.Printf("[io] stop device %s error: %v", dev.Name(), err)
		}
	}
}

func (m *IOManager) InjectInput(source string, eventType string, payload map[string]interface{}) {
	m.inputCh <- &InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		OutputChannel: source,
	}
}

func (m *IOManager) InjectInputSync(source string, eventType string, payload map[string]interface{}) *OutputEvent {
	ch := make(chan *OutputEvent, 1)
	m.inputCh <- &InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		ResponseCh:    ch,
		OutputChannel: source,
	}
	return <-ch
}

// InjectInputTo 注入输入事件并指定输出通道
func (m *IOManager) InjectInputTo(source, outputChannel, eventType string, payload map[string]interface{}) {
	m.inputCh <- &InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		OutputChannel: outputChannel,
	}
}

// InjectInputSyncTo 注入输入事件（同步等待）并指定输出通道
func (m *IOManager) InjectInputSyncTo(source, outputChannel, eventType string, payload map[string]interface{}) *OutputEvent {
	ch := make(chan *OutputEvent, 1)
	m.inputCh <- &InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          eventType,
		Payload:       payload,
		ResponseCh:    ch,
		OutputChannel: outputChannel,
	}
	return <-ch
}

func (m *IOManager) InjectText(source string, text string) {
	m.InjectInput(source, "text", map[string]interface{}{
		"content": text,
	})
}

func (m *IOManager) InjectTextSync(source string, text string) *OutputEvent {
	return m.InjectInputSync(source, "text", map[string]interface{}{
		"content": text,
	})
}

// InjectTextTo 注入文本输入并指定输出通道
func (m *IOManager) InjectTextTo(source, outputChannel, text string) {
	m.InjectInputTo(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

// InjectInterrupt 向中断通道发送输入
func (m *IOManager) InjectInterrupt(source, channel string, payload map[string]interface{}) {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	evtType, _ := payload["type"].(string)
	m.interruptCh <- &InputEvent{
		RequestID:     m.nextRequestID(),
		Source:        source,
		Type:          evtType,
		Payload:       payload,
		OutputChannel: channel,
	}
}

func (m *IOManager) InjectInterruptText(source, channel, text string) {
	m.InjectInterrupt(source, channel, map[string]interface{}{
		"type":    "text",
		"content": text,
	})
}

func (m *IOManager) InputInterruptChan() <-chan *InputEvent { return m.interruptCh }

// InjectTextSyncTo 注入文本输入（同步等待）并指定输出通道
func (m *IOManager) InjectTextSyncTo(source, outputChannel, text string) *OutputEvent {
	return m.InjectInputSyncTo(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

func (m *IOManager) EmitOutput(target string, outputType string, payload map[string]interface{}) {
	m.outputCh <- &OutputEvent{
		RequestID: "",
		Target:    target,
		Type:      outputType,
		Payload:   payload,
		Done:      true,
	}
}

// EmitOutputTo 通过指定输出通道发送
func (m *IOManager) EmitOutputTo(target, outputChannel, outputType string, payload map[string]interface{}) {
	m.outputCh <- &OutputEvent{
		RequestID:     "",
		Target:        target,
		Type:          outputType,
		Payload:       payload,
		Done:          true,
		OutputChannel: outputChannel,
	}
}

func (m *IOManager) EmitText(target string, text string) {
	m.EmitOutput(target, "text", map[string]interface{}{
		"content": text,
	})
}

// EmitTextTo 通过指定输出通道发送文本
func (m *IOManager) EmitTextTo(target, outputChannel, text string) {
	m.EmitOutputTo(target, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

func (m *IOManager) InputChan() <-chan *InputEvent  { return m.inputCh }
func (m *IOManager) OutputChan() <-chan *OutputEvent { return m.outputCh }

func (m *IOManager) GetAllTools() []ToolDef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []ToolDef
	for _, dev := range m.devices {
		tools = append(tools, dev.Tools()...)
	}
	return tools
}

func (m *IOManager) ExecuteTool(name string, args map[string]interface{}) (interface{}, error) {
	m.mu.RLock()
	type nameDevice struct {
		name string
		dev  Device
	}
	var candidates []nameDevice
	for _, dev := range m.devices {
		for _, t := range dev.Tools() {
			if t.Name == name {
				candidates = append(candidates, nameDevice{name: dev.Name(), dev: dev})
				break
			}
		}
	}
	m.mu.RUnlock()

	if len(candidates) == 0 {
		return nil, fmt.Errorf("tool %s not found", name)
	}
	return candidates[0].dev.Execute(name, args)
}

func (m *IOManager) ListDevices() []Device {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		list = append(list, d)
	}
	return list
}

// ChannelInfo 返回 IOManager 中已注册的所有通道信息
type ChannelInfo struct {
	Name        string           `json:"name"`
	Type        DeviceType       `json:"type"`
	Description string           `json:"description"`
	Tools       []ToolDef        `json:"tools"`
	OutputCaps  OutputCapability `json:"output_capabilities"`
}

func (m *IOManager) ListChannels() []ChannelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var list []ChannelInfo
	for _, dev := range m.devices {
		list = append(list, ChannelInfo{
			Name:        dev.Name(),
			Type:        dev.Type(),
			Description: dev.Description(),
			Tools:       dev.Tools(),
			OutputCaps:  dev.OutputCapabilities(),
		})
	}
	return list
}

func (m *IOManager) GetChannelCapabilities(channel string) OutputCapability {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if dev, ok := m.devices[channel]; ok {
		return dev.OutputCapabilities()
	}
	return 0
}

// Microphone
type Microphone struct {
	name       string
	sampleRate int
	io         *IOManager
}

func NewMicrophone(name string, sampleRate int, io *IOManager) *Microphone {
	return &Microphone{name: name, sampleRate: sampleRate, io: io}
}

func (d *Microphone) Name() string             { return d.name }
func (d *Microphone) Type() DeviceType         { return DeviceInput }
func (d *Microphone) OutputCapabilities() OutputCapability { return 0 } // 纯输入
func (d *Microphone) Description() string { return fmt.Sprintf("麦克风 (%s, %dHz)", d.name, d.sampleRate) }
func (d *Microphone) Start() error        { return nil }
func (d *Microphone) Stop() error         { return nil }

func (d *Microphone) Tools() []ToolDef {
	return []ToolDef{{
		Name:        d.name + "_capture",
		Description: fmt.Sprintf("从 %s 录制音频", d.name),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"duration": map[string]interface{}{"type": "number", "description": "录制时长（秒）", "default": 3},
			},
		},
	}}
}

func (d *Microphone) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "status": "recorded", "format": "wav", "sample_rate": d.sampleRate}, nil
}

// Speaker
type Speaker struct {
	name string
	io   *IOManager
}

func NewSpeaker(name string, io *IOManager) *Speaker {
	return &Speaker{name: name, io: io}
}

func (d *Speaker) Name() string        { return d.name }
func (d *Speaker) Type() DeviceType    { return DeviceOutput }
func (d *Speaker) OutputCapabilities() OutputCapability { return CapText | CapAudio }
func (d *Speaker) Description() string { return fmt.Sprintf("扬声器 (%s)", d.name) }
func (d *Speaker) Start() error        { return nil }
func (d *Speaker) Stop() error         { return nil }

func (d *Speaker) Tools() []ToolDef {
	return []ToolDef{{
		Name:        d.name + "_speak",
		Description: fmt.Sprintf("通过 %s 播放语音", d.name),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{"type": "string", "description": "播放文本"},
			},
			"required": []string{"text"},
		},
	}}
}

func (d *Speaker) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	text, _ := args["text"].(string)
	return map[string]interface{}{"device": d.name, "status": "playing", "text": text}, nil
}

// Camera
type Camera struct {
	name string
	io   *IOManager
}

func NewCamera(name string, io *IOManager) *Camera {
	return &Camera{name: name, io: io}
}

func (d *Camera) Name() string        { return d.name }
func (d *Camera) Type() DeviceType    { return DeviceInput }
func (d *Camera) OutputCapabilities() OutputCapability { return CapImage } // 可返回图片
func (d *Camera) Description() string { return fmt.Sprintf("摄像头 (%s)", d.name) }
func (d *Camera) Start() error        { return nil }
func (d *Camera) Stop() error         { return nil }

func (d *Camera) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        d.name + "_capture",
			Description: fmt.Sprintf("使用 %s 拍照", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"quality": map[string]interface{}{"type": "integer", "description": "质量1-100", "default": 90},
				},
			},
		},
		{
			Name:        d.name + "_stream",
			Description: fmt.Sprintf("控制 %s 视频流", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{"type": "string", "enum": []interface{}{"start", "stop"}},
				},
				"required": []string{"action"},
			},
		},
	}
}

func (d *Camera) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "status": "captured"}, nil
}

// RobotArm
type RobotArm struct {
	name string
	io   *IOManager
}

func NewRobotArm(name string, io *IOManager) *RobotArm {
	return &RobotArm{name: name, io: io}
}

func (d *RobotArm) Name() string        { return d.name }
func (d *RobotArm) Type() DeviceType    { return DeviceIO }
func (d *RobotArm) OutputCapabilities() OutputCapability { return CapStructured }
func (d *RobotArm) Description() string { return fmt.Sprintf("机械臂 (%s)", d.name) }
func (d *RobotArm) Start() error        { return nil }
func (d *RobotArm) Stop() error         { return nil }

func (d *RobotArm) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        d.name + "_move",
			Description: fmt.Sprintf("移动 %s 到坐标", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"x": map[string]interface{}{"type": "number", "description": "X 轴"},
					"y": map[string]interface{}{"type": "number", "description": "Y 轴"},
					"z": map[string]interface{}{"type": "number", "description": "Z 轴"},
				},
				"required": []string{"x", "y", "z"},
			},
		},
		{
			Name:        d.name + "_grip",
			Description: fmt.Sprintf("控制 %s 夹爪", d.name),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{"type": "string", "enum": []interface{}{"open", "close"}},
				},
				"required": []string{"action"},
			},
		},
	}
}

func (d *RobotArm) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "tool": tool, "status": "executed"}, nil
}

// GPIODevice
type GPIODevice struct {
	name string
	pins []int
	io   *IOManager
}

func NewGPIODevice(name string, pins []int, io *IOManager) *GPIODevice {
	return &GPIODevice{name: name, pins: pins, io: io}
}

func (d *GPIODevice) Name() string        { return d.name }
func (d *GPIODevice) Type() DeviceType    { return DeviceIO }
func (d *GPIODevice) OutputCapabilities() OutputCapability { return CapStructured }
func (d *GPIODevice) Description() string { return "GPIO 通用引脚" }
func (d *GPIODevice) Start() error        { return nil }
func (d *GPIODevice) Stop() error         { return nil }

func (d *GPIODevice) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        d.name + "_gpio_write",
			Description: "设置引脚电平",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin":   map[string]interface{}{"type": "integer"},
					"value": map[string]interface{}{"type": "integer", "enum": []interface{}{0, 1}},
				},
				"required": []string{"pin", "value"},
			},
		},
		{
			Name:        d.name + "_gpio_read",
			Description: "读取引脚电平",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"pin"},
			},
		},
	}
}

func (d *GPIODevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"device": d.name, "tool": tool, "status": "ok"}, nil
}

package io

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// ChannelDef 描述通道在记忆计算层的行为，与 ToolDef.NoMemory/Cleaner 语义一致。
type ChannelDef = pubsdk.ChannelDef

type DeviceType int

const (
	DeviceInput  DeviceType = 0
	DeviceOutput DeviceType = 1
	DeviceIO     DeviceType = 2
)

// OutputCapability 定义通道支持的输出格式
type OutputCapability int

const (
	CapText       OutputCapability = 1 << iota // 文本
	CapFile                                    // 文件
	CapImage                                   // 图片
	CapAudio                                   // 音频
	CapStructured                              // 结构化数据（JSON/卡片）
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
	ChannelDef() ChannelDef
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
	mu            sync.RWMutex
	devices       map[string]Device
	inputCh       chan *InputEvent
	interruptCh   chan *InputEvent
	outputCh      chan *OutputEvent
	nextReqID     int64
	inputChannels map[string]ChannelDef

	// toolBlocks：插件工具注入多模态内容块，process.go 在下一条 tool message 时消费。
	// 用 interface{}[] 避免 import api.ContentBlock 导致的循环依赖。
	toolBlocksMu      sync.Mutex
	toolPendingBlocks []interface{}
}

func NewIOManager() *IOManager {
	return &IOManager{
		devices:       make(map[string]Device),
		inputCh:       make(chan *InputEvent, 256),
		interruptCh:   make(chan *InputEvent, 64),
		outputCh:      make(chan *OutputEvent, 256),
		inputChannels: make(map[string]ChannelDef),
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

func (m *IOManager) GetDevice(name string) Device {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.devices[name]
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

// InjectOptions 声明一次注入在记忆层与上下文层的表现。
//
// 零值 = 记入记忆 + 不裁剪上下文，与历史的三参数注入方法完全一致。
// 别名到公共 SDK 而非另建一套：内置插件与外部插件必须用同一套结构，
// 否则内核要认两种类型，而漏认会静默丢失标志位。
type InjectOptions = pubsdk.InjectOptions

// applyInjectOpts 把注入标志位写进事件 payload。
//
// 只在非零时写：零值与旧 payload 逐字节一致，事件订阅方与旧内核
// （不认识这两个键）都不会受影响。
//
// 为什么不把标志位当独立参数传到底：eventloop 与各注入路径都按 payload 取字段
// （no_memory 本来就是这么走的），payload 是这里唯一已有的携带面。
func applyInjectOpts(payload map[string]interface{}, opts InjectOptions) {
	if opts.NoMemory {
		payload["no_memory"] = true
	}
	if opts.ContextPolicy != "" {
		payload["context_policy"] = opts.ContextPolicy
	}
	if opts.CleanerName != "" {
		payload["cleaner_name"] = opts.CleanerName
	}
	// priority 只对中断注入有意义；排队路径会忽略它（内核侧只读不写）。
	if opts.Priority != "" {
		payload["priority"] = opts.Priority
	}
}

func (m *IOManager) InjectInputOpts(source, eventType string, payload map[string]interface{}, opts InjectOptions) {
	applyInjectOpts(payload, opts)
	m.InjectInput(source, eventType, payload)
}

func (m *IOManager) InjectInputToOpts(source, outputChannel, eventType string, payload map[string]interface{}, opts InjectOptions) {
	applyInjectOpts(payload, opts)
	m.InjectInputTo(source, outputChannel, eventType, payload)
}

func (m *IOManager) InjectInputSyncToOpts(source, outputChannel, eventType string, payload map[string]interface{}, opts InjectOptions) *OutputEvent {
	applyInjectOpts(payload, opts)
	return m.InjectInputSyncTo(source, outputChannel, eventType, payload)
}

func (m *IOManager) InjectInterruptOpts(source, channel string, payload map[string]interface{}, opts InjectOptions) {
	applyInjectOpts(payload, opts)
	m.InjectInterrupt(source, channel, payload)
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

// InjectTextNoMemoryTo 注入文本输入（不产生记忆）并指定输出通道
func (m *IOManager) InjectTextNoMemoryTo(source, outputChannel, text string) {
	m.InjectInputTo(source, outputChannel, "text", map[string]interface{}{
		"content":   text,
		"no_memory": true,
	})
}

// InjectTextSyncNoMemoryTo 注入文本输入（同步等待，不产生记忆）并指定输出通道
func (m *IOManager) InjectTextSyncNoMemoryTo(source, outputChannel, text string) *OutputEvent {
	return m.InjectInputSyncTo(source, outputChannel, "text", map[string]interface{}{
		"content":   text,
		"no_memory": true,
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
	m.InjectInterruptTextOpts(source, channel, text, InjectOptions{})
}

// InjectInterruptTextOpts 注入中断文本，并声明本次注入的记忆/裁剪行为。
//
// 中断也允许声明 ContextPolicyPrune：中断同样携带内容进入上下文。
func (m *IOManager) InjectInterruptTextOpts(source, channel, text string, opts InjectOptions) {
	m.InjectInterruptOpts(source, channel, map[string]interface{}{
		"type":    "text",
		"content": text,
	}, opts)
}

// InjectTextOpts 注入排队文本，并声明本次注入的记忆/裁剪行为。
func (m *IOManager) InjectTextOpts(source, channel, text string, opts InjectOptions) {
	m.InjectInputToOpts(source, channel, "text", map[string]interface{}{
		"content": text,
	}, opts)
}

// InjectTextSyncOpts 同步注入文本并声明记忆/裁剪行为。
func (m *IOManager) InjectTextSyncOpts(source, outputChannel, text string, opts InjectOptions) *OutputEvent {
	return m.InjectInputSyncToOpts(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	}, opts)
}

func (m *IOManager) InputInterruptChan() <-chan *InputEvent { return m.interruptCh }

// InjectTextSyncTo 注入文本输入（同步等待）并指定输出通道
func (m *IOManager) InjectTextSyncTo(source, outputChannel, text string) *OutputEvent {
	return m.InjectInputSyncTo(source, outputChannel, "text", map[string]interface{}{
		"content": text,
	})
}

// ---- 带标志位的注入（记忆/裁剪行为由调用点声明）----

// InjectInputMediaOpts 注入带媒体块的输入，并声明记忆/裁剪行为。
func (m *IOManager) InjectInputMediaOpts(source, outputChannel, text string, blocks []pubsdk.ContentBlock, opts InjectOptions) {
	m.InjectInputToOpts(source, outputChannel, "text", map[string]interface{}{
		"content":      text,
		"media_blocks": blocks,
	}, opts)
}

// InjectInputMediaSyncOpts 注入带媒体块的输入并同步等待回复，同时声明记忆/裁剪行为。
func (m *IOManager) InjectInputMediaSyncOpts(source, outputChannel, text string, blocks []pubsdk.ContentBlock, opts InjectOptions) *OutputEvent {
	return m.InjectInputSyncToOpts(source, outputChannel, "text", map[string]interface{}{
		"content":      text,
		"media_blocks": blocks,
	}, opts)
}

// InjectInterruptMediaOpts 注入带媒体块的中断，并声明记忆/裁剪行为。
func (m *IOManager) InjectInterruptMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts InjectOptions) {
	m.InjectInterruptOpts(source, channel, map[string]interface{}{
		"type":         "text",
		"content":      text,
		"media_blocks": blocks,
	}, opts)
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

func (m *IOManager) InputChan() <-chan *InputEvent   { return m.inputCh }
func (m *IOManager) OutputChan() <-chan *OutputEvent { return m.outputCh }

// RegisterInputChannel 注册输入通道的记忆行为
func (m *IOManager) RegisterInputChannel(name string, def ChannelDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputChannels[name] = def
}

// UnregisterInputChannel 注销输入通道
func (m *IOManager) UnregisterInputChannel(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inputChannels, name)
}

// GetInputChannelDef 查询输入通道的记忆行为定义
func (m *IOManager) GetInputChannelDef(name string) (ChannelDef, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	def, ok := m.inputChannels[name]
	return def, ok
}

func (m *IOManager) GetAllTools() []ToolDef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []ToolDef
	for _, dev := range m.devices {
		tools = append(tools, dev.Tools()...)
	}
	return tools
}

func (m *IOManager) ExecuteTool(name string, args map[string]interface{}) (ret interface{}, err error) {
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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[io] tool %s execute panic: %v\n%s", name, r, debug.Stack())
			err = fmt.Errorf("tool %s execute panic: %v", name, r)
		}
	}()
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

func (d *Microphone) Name() string                         { return d.name }
func (d *Microphone) Type() DeviceType                     { return DeviceInput }
func (d *Microphone) OutputCapabilities() OutputCapability { return 0 } // 纯输入
func (d *Microphone) Description() string {
	return fmt.Sprintf("麦克风 (%s, %dHz)", d.name, d.sampleRate)
}
func (d *Microphone) Start() error           { return nil }
func (d *Microphone) Stop() error            { return nil }
func (d *Microphone) ChannelDef() ChannelDef { return ChannelDef{} }

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

func (d *Speaker) Name() string                         { return d.name }
func (d *Speaker) Type() DeviceType                     { return DeviceOutput }
func (d *Speaker) OutputCapabilities() OutputCapability { return CapText | CapAudio }
func (d *Speaker) Description() string                  { return fmt.Sprintf("扬声器 (%s)", d.name) }
func (d *Speaker) Start() error                         { return nil }
func (d *Speaker) Stop() error                          { return nil }
func (d *Speaker) ChannelDef() ChannelDef               { return ChannelDef{} }

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

func (d *Camera) Name() string                         { return d.name }
func (d *Camera) Type() DeviceType                     { return DeviceInput }
func (d *Camera) OutputCapabilities() OutputCapability { return CapImage } // 可返回图片
func (d *Camera) Description() string                  { return fmt.Sprintf("摄像头 (%s)", d.name) }
func (d *Camera) Start() error                         { return nil }
func (d *Camera) Stop() error                          { return nil }
func (d *Camera) ChannelDef() ChannelDef               { return ChannelDef{} }

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

func (d *RobotArm) Name() string                         { return d.name }
func (d *RobotArm) Type() DeviceType                     { return DeviceIO }
func (d *RobotArm) OutputCapabilities() OutputCapability { return CapStructured }
func (d *RobotArm) Description() string                  { return fmt.Sprintf("机械臂 (%s)", d.name) }
func (d *RobotArm) Start() error                         { return nil }
func (d *RobotArm) Stop() error                          { return nil }
func (d *RobotArm) ChannelDef() ChannelDef               { return ChannelDef{} }

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

func (d *GPIODevice) Name() string                         { return d.name }
func (d *GPIODevice) Type() DeviceType                     { return DeviceIO }
func (d *GPIODevice) OutputCapabilities() OutputCapability { return CapStructured }
func (d *GPIODevice) Description() string                  { return "GPIO 通用引脚" }
func (d *GPIODevice) Start() error                         { return nil }
func (d *GPIODevice) Stop() error                          { return nil }
func (d *GPIODevice) ChannelDef() ChannelDef               { return ChannelDef{} }

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

// SetToolBlocks 插件工具调用时注入多模态内容块（image_url/audio_url 等），
// 下一条 tool message 追加这些块到 content 数组（OpenAI 多模态格式）。
func (m *IOManager) SetToolBlocks(blocks []interface{}) {
	m.toolBlocksMu.Lock()
	m.toolPendingBlocks = blocks
	m.toolBlocksMu.Unlock()
}

// ConsumeToolBlocks 返回并清空 pending blocks，process.go 在 append tool message 时调用。
func (m *IOManager) ConsumeToolBlocks() []interface{} {
	m.toolBlocksMu.Lock()
	blocks := m.toolPendingBlocks
	m.toolPendingBlocks = nil
	m.toolBlocksMu.Unlock()
	return blocks
}

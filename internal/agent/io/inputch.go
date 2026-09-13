package io

// inputch 是**最基本的输入路由单位**（设计见 docs/zh/resident-subagent-design.md §4）。
//
// 一个插件可以注册多个 inputch；每个 inputch 是彼此独立的路由单位：
// 可以被划给不同的 agent、可以分别限额。中断输入与排队输入**两类都从 inputch 进出**，
// 而"中断 vs 排队"是每条输入自己的类别 —— 不是 inputch 的属性。
//
// 本文件只放**登记表**（谁是注册者、划给了谁、容量多少、记忆策略是什么）。
// 路由本身发生在进内核之前：投递方决定"这条输入投给哪个 inputch"。

import (
	"errors"
	"sort"
	"sync"
)

var (
	// ErrInputChannelUnknown 表示引用了未注册的 inputch。
	ErrInputChannelUnknown = errors.New("inputch 未注册")
	// ErrInputChannelNameEmpty 表示 inputch 名为空。
	ErrInputChannelNameEmpty = errors.New("inputch 名不能为空")
)

// InputChannel 是一个 inputch 的完整登记记录。
type InputChannel struct {
	// Name 是路由单位 id（全局唯一，如 "qq"、"webui"、"qq/device-2"）。
	Name string `json:"name"`
	// Plugin 是注册它的插件名（"插件可注册多个 inputch"，归属可追溯）。
	Plugin string `json:"plugin,omitempty"`
	// Owner 是**被划给的 agent id**（"" = 未分配，归根 agent/内核默认）。
	Owner string `json:"owner,omitempty"`
	// Capacity 是该 inputch 的队列容量（0 = 用内核默认值）。
	Capacity int `json:"capacity,omitempty"`
	// Output 是该 inputch 的默认回程输出通道（"" = 由来源/调用方决定）。
	//
	// 注意：这不是"内核路由"—— 输出仍然是 agent 的主动调用；这里只是登记
	// "这个 inputch 的回复默认该往哪个 outputch 走"的映射依据。
	Output string `json:"output,omitempty"`
	// Def 是记忆/上下文策略（沿用 ChannelDef：NoMemory / Cleaner / ContextPolicy）。
	// Cleaner 是函数，故本字段不可序列化（json:"-"）。
	Def ChannelDef `json:"-"`
}

// ChannelRegistry 是 inputch 的登记表。
//
// 它被设计成**可共享对象**（`*ChannelRegistry`）：根 agent 与它的驻留子共用同一份，
// 这样"划入/授权"才有意义；每个 IOManager 默认自带一份（向后兼容）。
type ChannelRegistry struct {
	mu       sync.RWMutex
	channels map[string]InputChannel
	// outputTargets 把**输出通道**解析成"目标 agent 的哪个 inputch"。
	// 这是"输出可寻址到具体 agent"的依据（子→父、父→指定子）。
	outputTargets map[string]OutputTarget
}

// OutputTarget 是一个输出通道的投递目标。
type OutputTarget struct {
	// AgentID 是目标 agent（"" = 本 agent / 由传输层通道 device 自行处理）。
	AgentID string `json:"agent_id,omitempty"`
	// InputCh 是目标 agent 上接收它的 inputch（"" = 与输出通道同名）。
	InputCh string `json:"inputch,omitempty"`
}

// NewChannelRegistry 构造一个空的 inputch 登记表。
func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels:      make(map[string]InputChannel),
		outputTargets: make(map[string]OutputTarget),
	}
}

// Register 登记/更新一个 inputch。
//
// 重复登记（插件重载）时**保留已有的 Owner/Capacity/Output**，只更新
// Plugin 与 Def —— 否则一次插件重载就会把父 agent 做的划分抹掉。
func (r *ChannelRegistry) Register(ch InputChannel) error {
	if ch.Name == "" {
		return ErrInputChannelNameEmpty
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channels == nil {
		r.channels = make(map[string]InputChannel)
	}
	if old, ok := r.channels[ch.Name]; ok {
		if ch.Owner == "" {
			ch.Owner = old.Owner
		}
		if ch.Capacity == 0 {
			ch.Capacity = old.Capacity
		}
		if ch.Output == "" {
			ch.Output = old.Output
		}
		if ch.Plugin == "" {
			ch.Plugin = old.Plugin
		}
	}
	r.channels[ch.Name] = ch
	return nil
}

// Unregister 注销一个 inputch。
func (r *ChannelRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.channels, name)
}

// Lookup 查询一个 inputch。
func (r *ChannelRegistry) Lookup(name string) (InputChannel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[name]
	return ch, ok
}

// Assign 把一个 inputch **划给**某个 agent（可同时给定容量）。
//
// 语义（默认取值，见设计文档 R6）：**读写授权**，不转移所有权 ——
// 登记表仍记录 Plugin（谁注册的）与 Owner（划给了谁）两件事。
func (r *ChannelRegistry) Assign(name, agentID string, capacity int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.channels[name]
	if !ok {
		return ErrInputChannelUnknown
	}
	ch.Owner = agentID
	if capacity > 0 {
		ch.Capacity = capacity
	}
	r.channels[name] = ch
	return nil
}

// List 返回全部已注册 inputch（按名字稳定排序）。
func (r *ChannelRegistry) List() []InputChannel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]InputChannel, 0, len(r.channels))
	for _, ch := range r.channels {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListByOwner 返回划给某个 agent 的 inputch（owner == "" 时返回**未分配**的）。
func (r *ChannelRegistry) ListByOwner(agentID string) []InputChannel {
	var out []InputChannel
	for _, ch := range r.List() {
		if ch.Owner == agentID {
			out = append(out, ch)
		}
	}
	return out
}

// BindOutputTarget 登记"输出通道 → 目标 agent 的 inputch"的解析。
//
// 例：父把子用的输出通道 "to-child-1" 绑到 (child-1, "sub/in")，
// 于是子经该通道发出的消息会投进 child-1 的 sub/in。
func (r *ChannelRegistry) BindOutputTarget(output, agentID, inputCh string) error {
	if output == "" {
		return errors.New("输出通道名不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outputTargets == nil {
		r.outputTargets = make(map[string]OutputTarget)
	}
	r.outputTargets[output] = OutputTarget{AgentID: agentID, InputCh: inputCh}
	return nil
}

// ResolveOutputTarget 解析一个输出通道的目标；未登记时 ok=false
// （意味着由传输层通道自行处理，如 qq/webui 这类 device 通道）。
func (r *ChannelRegistry) ResolveOutputTarget(output string) (OutputTarget, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.outputTargets[output]
	return t, ok
}

// ListOutputTargets 返回全部已登记的目标解析（按输出通道名排序）。
func (r *ChannelRegistry) ListOutputTargets() map[string]OutputTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]OutputTarget, len(r.outputTargets))
	for k, v := range r.outputTargets {
		out[k] = v
	}
	return out
}

// Count 返回已注册 inputch 数量。
func (r *ChannelRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.channels)
}

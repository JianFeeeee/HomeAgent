package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	pubsdk "gitcode.com/JianFeeeee/homeagent-sdk/sdk"
)

// Plugin 是 registry 可加载的子进程插件，与内置插件同构的启停接口。
//
// 生命周期：
//
//	New()        创建（尚未 spawn）
//	Start(core)  spawn 子进程 → 握手（传共享段 fd）→ plugin.init → plugin.start
//	             （plugin.start 期间插件反向注册工具/阶段/通道）
//	Stop()       plugin.stop → 宽限期 → 必要时 Kill
//	Close()      强制结束（registry 卸载/重载路径）
//
// **共享段不属于 Plugin**：它属于 Host，被全部子进程插件共享。
// 若每插件一段，「内核 ctx → 段 → 插件改 → 回读 ctx」在多插件下会退化成
// 副本模型，lost update 原样复现（§8.4）。
type Plugin struct {
	name   string
	bin    string
	dir    string
	config map[string]interface{}

	host    *Host
	proc    *Process
	handler *coreHandler

	// env 追加到子进程环境变量（测试用；生产由 registry 按需设置）。
	env []string

	// onCrash 由 registry 注入，把进程退出喂给 plugin_health.recordCrash（§2.3）。
	onCrash func(name string, err error)

	// caps 是 manifest 声明的能力集（§3.8 权限梯度）。
	caps *capabilitySet

	stopOnce sync.Once

	// stopping 标记「本次退出是内核主动发起的」，用于压掉 onCrash。
	//
	// 必要性：Stop() 宽限期超时与 Close() 都走 Process.Kill()，
	// 而 Kill 产生的 `signal: killed` 是非 nil 的 waitErr——若不区分，
	// 重载/禁用/卸载这些**内核自己发起**的停止会被 handleExit 当成崩溃上报，
	// 触发一轮多余的自动重启（重载路径下等于把刚装好的插件又推倒一次）。
	stopping atomic.Bool
}

// New 创建子进程插件（不启动进程）。
//
// host 必须是全部子进程插件共用的实例（由 registry 创建一次）。
// New 创建子进程插件（不启动进程）。
//
// host 必须是全部子进程插件共用的实例（由 registry 创建一次）。
// capabilities 来自 manifest 的 capabilities 字段；为空时不限制（存量插件向后兼容）。
func New(name, bin, dir string, config map[string]interface{}, host *Host, onCrash func(string, error), capabilities ...string) *Plugin {
	return &Plugin{
		name:    name,
		bin:     bin,
		dir:     dir,
		config:  config,
		host:    host,
		onCrash: onCrash,
		caps:    newCapabilitySet(capabilities),
	}
}

// Name 实现 sdk.Plugin。
func (p *Plugin) Name() string { return p.name }

// PID 返回子进程号；未启动或已退出返回 0。
// 供 pluginmgr 呈现「插件实际在跑哪个进程」。
func (p *Plugin) PID() int {
	if p.proc == nil {
		return 0
	}
	if !p.Alive() {
		return 0
	}
	return p.proc.PID()
}

// Alive 报告子进程是否仍存活。
func (p *Plugin) Alive() bool {
	if p.proc == nil {
		return false
	}
	select {
	case <-p.proc.Exited():
		return false
	default:
		return true
	}
}

// Start 启动子进程并完成注册。
//
// core 是内核为该插件构建的能力面（internal/sdk.PluginSDK 天然满足 CoreSDK）。
func (p *Plugin) Start(core CoreSDK) error {
	if p.host == nil {
		return fmt.Errorf("proc: %s 缺少共享段 Host", p.name)
	}

	p.handler = &coreHandler{
		sdk:     core,
		name:    p.name,
		host:    p.host,
		locks:   p.host.locks,
		evtRing: p.host.evtSubscriber,
		caps:    p.caps,
	}
	// 反向调用闭包：注册回调时捕获，运行期经 RPC 打到插件进程。
	p.handler.invokeTool = p.invokeTool
	p.handler.invokeCleaner = p.invokeCleaner
	p.handler.invokeStageFn = p.invokeStage
	p.handler.invokeOutput = p.invokeOutput

	proc, err := Spawn(p.name, p.bin, Options{
		Dir: p.dir,
		// 共享段的传递机制按平台不同（shmpass_*.go）：
		// Unix 经 ExtraFiles 传继承 fd（ 3=StageContext, 4=事件环, 5=通知）；
		// Windows 无 fd 继承语义，改用命名内核对象，名字经环境变量传入。
		Env:         append(p.env, p.host.procEnvForShm()...),
		ExtraFiles:  p.host.procExtraFilesForShm(),
		ShmSize:     p.host.shmSize,
		EvtRingSize: evtTotalSize,
		Handler:     p.handler.Handle,
		OnExit:      p.handleExit,
		Supervisor:  p.host.Supervisor(),
	})
	if err != nil {
		return err
	}
	p.proc = proc

	// plugin.init：构造插件实例
	if _, err := proc.Call(MethodPluginInit, PluginInitParams{
		Name:   p.name,
		Config: p.config,
	}); err != nil {
		proc.Kill()
		return fmt.Errorf("proc: %s plugin.init 失败: %w", p.name, err)
	}

	// plugin.start：插件在此期间反向注册工具/阶段/通道
	if _, err := proc.Call(MethodPluginStart, nil); err != nil {
		proc.Kill()
		return fmt.Errorf("proc: %s plugin.start 失败: %w", p.name, err)
	}
	return nil
}

// Stop 优雅停止（实现 sdk.Plugin）。
func (p *Plugin) Stop() error {
	p.stopping.Store(true)
	var err error
	p.stopOnce.Do(func() {
		if p.proc != nil {
			err = p.proc.Stop()
		}
	})
	return err
}

// Close 强制结束子进程。
//
// **这里是真 kill + wait**——对比 cabi 路径的 Close 只做 dlclose，
// 而 dlclose 对 Go c-shared 是 no-op（§1.1，热重载静默失效的根因）。
func (p *Plugin) Close() error {
	p.stopping.Store(true)
	var err error
	p.stopOnce.Do(func() {
		if p.proc != nil {
			err = p.proc.Kill()
		}
	})
	return err
}

// handleExit 在子进程退出时把信号喂给 plugin_health（§2.3 逻辑复用），
// 并释放该插件可能持有的 stage 锁。
//
// 后者是"锁仲裁回内核"的自愈价值：持锁者死亡不会导致全局死锁，
// 无需 robust pthread_mutex（实验 9）。
func (p *Plugin) handleExit(name string, err error) {
	if p.host != nil && p.host.ForceReleaseLock(name) {
		log.Printf("[proc] %s 退出，内核已释放其持有的 stage 锁", name)
	}
	// 内核主动停止（Stop/Close，含宽限期超时后的 Kill）不算崩溃：
	// 否则重载/禁用/卸载都会误触发自动重启。
	if p.stopping.Load() {
		return
	}
	if err != nil && p.onCrash != nil {
		p.onCrash(name, err)
	}
}

// ---- 内核 → 插件的反向调用 ----

func (p *Plugin) invokeTool(name string, args map[string]interface{}) (interface{}, error) {
	if p.proc == nil {
		return nil, ErrProcessExited
	}
	raw, err := p.proc.Call(MethodToolInvoke, ToolInvokeParams{Name: name, Args: args})
	if err != nil {
		return nil, err
	}
	var res ToolInvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("proc: %s 工具 %s 应答解析失败: %w", p.name, name, err)
	}
	return res.Result, nil
}

// invokeCleaner 在插件进程内执行工具或通道注册时提供的 Cleaner 函数。
func (p *Plugin) invokeCleaner(scope, name, text string) (string, error) {
	if p.proc == nil {
		return "", ErrProcessExited
	}
	raw, err := p.proc.Call(MethodCleanerInvoke, CleanerInvokeParams{
		Scope: scope,
		Name:  name,
		Text:  text,
	})
	if err != nil {
		return "", err
	}
	var res CleanerInvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("proc: %s %s Cleaner %s 应答解析失败: %w", p.name, scope, name, err)
	}
	return res.Text, nil
}

func (p *Plugin) invokeStage(ctx context.Context, stage string, seq uint64) error {
	if p.proc == nil {
		return ErrProcessExited
	}
	raw, err := p.proc.CallContext(ctx, MethodStageInvoke, StageInvokeParams{
		Stage: stage,
		Seq:   seq,
	})
	if err != nil {
		return err
	}
	var res StageInvokeResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("proc: %s stage %s 应答解析失败: %w", p.name, stage, err)
		}
	}
	if res.DirtyFields > 0 {
		log.Printf("[proc] %s stage %s 改写了 %d 个字段", p.name, stage, res.DirtyFields)
	}
	return nil
}

// invokeOutput 经插件输出通道发送，**同步等待真实结果**。
//
// 这是 §9.4 的根治：C ABI 下 cgo 不可嵌套，只能异步 fire-and-forget，
// 导致 output_send 永远返回 {status:queued} + err=nil，模型永远以为发送成功
// （现网 7 天内 2 次消息实际发不出）。进程模型下 RPC 天然可等应答。
func (p *Plugin) invokeOutput(channel string, args map[string]interface{}) (interface{}, error) {
	if p.proc == nil {
		return nil, ErrProcessExited
	}
	raw, err := p.proc.Call(MethodOutputInvoke, OutputInvokeParams{
		Channel: channel,
		Args:    args,
	})
	if err != nil {
		return nil, err // 真实失败上报，模型可感知并重试
	}
	if len(raw) == 0 {
		return map[string]interface{}{"status": "sent"}, nil
	}
	var res map[string]interface{}
	if err := json.Unmarshal(raw, &res); err != nil {
		return map[string]interface{}{"status": "sent"}, nil
	}
	if _, ok := res["status"]; !ok {
		res["status"] = "sent"
	}
	return res, nil
}

// 编译期确认 Plugin 具备 registry 需要的启停形状。
var _ interface {
	Name() string
	Stop() error
	Close() error
} = (*Plugin)(nil)

// 引用一下公开 SDK，确保本文件的类型假设与它同版本。
var _ = pubsdk.StageScopeGlobal

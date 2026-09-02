//go:build linux || darwin

package plugin

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin/proc"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// tryLoadProc 只做静态校验（供双通道探测与测试），不构造插件实体。
//
// 真正加载走 Registry.loadProc：子进程插件需要共享段 Host，
// 而 Host 必须是**全部 .bin 插件共用的那一个**，只能由 Registry 持有。
//
// 返回 nil,nil 表示目录中没有 plugin.bin（交由后续探测通道）；
// 找到二进制但不可用时返回明确错误——不静默回退到 cabi。
func tryLoadProc(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	_, err := validateProcBinary(dir, name)
	return nil, err
}

// 子进程插件加载（plugin.bin）——外部插件多进程化的加载入口。
//
// 设计依据：docs/zh/架构迁移评估.md §3（stdio JSON-RPC 控制面 + shm 数据面 + eventfd 通知面）
// 实施计划：docs/zh/plugin-migration-plan.md Part 2/3

// validateProcBinary 校验 plugin.bin 是否存在且可执行。
// 返回 ("", nil) 表示该目录不是 proc 插件。
func validateProcBinary(dir, name string) (string, error) {
	binPath := filepath.Join(dir, binEntry)
	st, err := os.Stat(binPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("proc plugin %s: 检查 %s: %w", name, binEntry, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("proc plugin %s: %s 是目录，不是可执行文件", name, binEntry)
	}
	if st.Mode()&0o111 == 0 {
		// 常见于经 zip/hmap 分发丢失权限位——给出可直接执行的修复指令
		return "", fmt.Errorf("proc plugin %s: %s 缺少可执行权限（chmod +x %s）",
			name, binEntry, binPath)
	}
	return binPath, nil
}

// loadProc 构造子进程插件实体（不 spawn）。
//
// 共享段 Host 在此惰性创建：**全部 .bin 插件共用一块段**。
// 若每插件一段，多插件同阶段并发时会退化成副本模型，
// lost update 原样复现（§8.4 实测 35.8~36.8%）。
func (r *Registry) loadProc(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	binPath, err := validateProcBinary(dir, name)
	if err != nil {
		return nil, err
	}
	if binPath == "" {
		return nil, nil
	}

	host, err := r.ensureProcHost()
	if err != nil {
		return nil, fmt.Errorf("proc plugin %s: %w", name, err)
	}

	return procPluginAdapter{Plugin: proc.New(name, binPath, dir, config, host, r.onProcCrash)}, nil
}

// ensureProcHost 惰性创建共享段 Host（全进程唯一）。
func (r *Registry) ensureProcHost() (*proc.Host, error) {
	r.procHostMu.Lock()
	defer r.procHostMu.Unlock()
	if r.procHost != nil {
		return r.procHost, nil
	}
	host, err := proc.NewHost()
	if err != nil {
		return nil, err
	}
	r.procHost = host

	// 事件环适配层：Bus 发布 → 写 EvtRing slot → eventfd 通知子进程
	if r.evBus != nil {
		er := NewEventRing(host.EvtRing(), int(host.Evtfd().Fd()), r.evBus)
		host.SetEvtSubscriber(er)
		log.Printf("[plugin] 事件环已创建（Bus → EvtRing → eventfd）")
	}

	log.Printf("[plugin] 共享段已创建（全部子进程插件共用一块，%d KB）", host.ShmSize()/1024)
	return host, nil
}

// closeProcHost 释放共享段（仅在内核关停时调用）。
func (r *Registry) closeProcHost() {
	r.procHostMu.Lock()
	defer r.procHostMu.Unlock()
	if r.procHost == nil {
		return
	}
	if err := r.procHost.Close(); err != nil {
		log.Printf("[plugin] 关闭共享段: %v", err)
	}
	r.procHost = nil
}

// onProcCrash 在子进程插件异常退出时回调。
//
// **崩溃隔离**：子进程死亡只影响自己，homed 继续服务——对比 C ABI 下
// 插件 panic 直接带崩整个进程（§1.2，现网已发生）。
//
// 崩溃计数/冷却/自愈复用既有 plugin_health（§2.3），本函数只负责把
// 进程退出这一事实转成事件通知；具体重载策略由 agent 侧决定。
func (r *Registry) onProcCrash(name string, err error) {
	log.Printf("[plugin] 子进程插件 %s 异常退出: %v（homed 未受影响）", name, err)
	if r.evBus == nil {
		return
	}
	// 不在此处直接重载：重载需要 registry 锁，而本回调可能在
	// 持锁路径的 goroutine 中触发，直接调用会死锁。
	r.evBus.Publish(&events.Event{
		Type:   events.EventSystem,
		Source: "plugin",
		Payload: map[string]interface{}{
			"event":  "plugin_crashed",
			"plugin": name,
			"error":  err.Error(),
		},
		Timestamp: time.Now().Unix(),
	})
}

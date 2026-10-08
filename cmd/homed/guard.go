//go:build linux

// L0/L1/L2 三层回退恢复机制的核心：父守护 guard（Linux 专属）。
//
// 依赖 Linux 设施：syscall.Reboot / /etc 网络基线 / overlayfs 离线回滚 /
// unix socket IPC / systemctl 重启。Windows 等平台不编译本文件，
// 由 guard_windows.go 提供 no-op 桩，保证 cmd/homed 跨平台可构建。
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/ipc"
	"github.com/JianFeeeee/HomeAgent/internal/recovery"
	"github.com/JianFeeeee/HomeAgent/internal/system"
	"github.com/JianFeeeee/HomeAgent/internal/tracker"
	"gopkg.in/yaml.v3"
)

// guardConfig 父守护配置：独立于 config.db，避开被改坏的配置数据库。
type guardConfig struct {
	MaxRestarts       int              `yaml:"max_restarts"`       // 连续 normal 崩溃重试上限
	HeartbeatTimeout  string           `yaml:"heartbeat_timeout"`  // 心跳多久未更新判定卡死（如 30s）
	HeartbeatInterval string           `yaml:"heartbeat_interval"` // 期待 worker 心跳频率（仅日志）
	LLMSnapshot       string           `yaml:"llm_snapshot"`       // LLM 配置基线文件，为空用 <data>/llm_snapshot.json
	FailbackEnabled   bool             `yaml:"failback_enabled"`   // 崩溃超限后是否进入受限 failback 启动
	LastResort        string           `yaml:"last_resort"`        // restart_app | reboot
	RestartCommand    []string         `yaml:"restart_command"`    // last_resort=restart_app 时的命令（如 systemctl restart homeagent）
	RebootGrace       string           `yaml:"reboot_grace"`       // last_resort=reboot 前的缓冲
	LLM               llmConfig        `yaml:"llm"`                // rescue 源（锚定 IP，绕开被破坏的 DNS/代理）
	Recovery          recoveryConfig   `yaml:"recovery"`           // failback 恢复参数（N 轮有界）
	LastResortCfg     lastResortConfig `yaml:"last_resort_cfg"`    // 最后手段细节（snapshot_before 等）
	System            systemConfig     `yaml:"system"`             // 发行版/部署路径适配

	heartbeatTimeout  time.Duration
	heartbeatInterval time.Duration
	rebootGrace       time.Duration
}

type llmConfig struct {
	Sources []recovery.RescueSource `yaml:"sources"`
}

type recoveryConfig struct {
	MaxAttempts    int      `yaml:"max_attempts"`    // failback 恢复尝试轮数上限
	AttemptTimeout string   `yaml:"attempt_timeout"` // 单轮恢复超时
	KnowledgeBase  string   `yaml:"knowledge_base"`  // 恢复知识库目录
	TriggerPrompt  string   `yaml:"trigger_prompt"`  // 恢复 agent 的系统提示词（未知/混合分支）
	Plugins        []string `yaml:"plugins"`         // failback 插件集（缺省用 core.agent.failback_plugins）
	attemptTimeout time.Duration
}

type lastResortConfig struct {
	SnapshotBefore bool `yaml:"snapshot_before"` // 最后手段前是否回滚 agentfs 到最近快照
}

// systemConfig 发行版/部署路径适配：把 L0 写前留档保护范围与网络基线文件列表
// 从默认的 Linux 路径换成当前部署实际路径，避免硬编码失效。
type systemConfig struct {
	ProtectedPaths []string `yaml:"protected_paths"` // L0 写前留档保护前缀（默认 /etc/）
	NetworkPaths   []string `yaml:"network_paths"`   // 网络基线文件（默认 resolv.conf/hosts/environment）
}

// applySystemConfig 应用发行版路径适配，供 guard 与 failback worker 共用。
func applySystemConfig(cfg *guardConfig) {
	if len(cfg.System.ProtectedPaths) > 0 {
		system.SetProtectedPaths(cfg.System.ProtectedPaths)
	}
	if len(cfg.System.NetworkPaths) > 0 {
		system.SetNetworkPaths(cfg.System.NetworkPaths)
	}
}

func defaultGuardConfig(dataDir string) *guardConfig {
	return &guardConfig{
		MaxRestarts:       3,
		HeartbeatTimeout:  "30s",
		HeartbeatInterval: "5s",
		LLMSnapshot:       filepath.Join(dataDir, "llm_snapshot.json"),
		FailbackEnabled:   true,
		LastResort:        "restart_app",
		RestartCommand:    []string{"systemctl", "restart", "homeagent"},
		RebootGrace:       "10s",
		Recovery: recoveryConfig{
			MaxAttempts:    3,
			AttemptTimeout: "120s",
		},
		LastResortCfg: lastResortConfig{SnapshotBefore: true},
	}
}

func loadGuardConfig(dataDir string) *guardConfig {
	cfg := defaultGuardConfig(dataDir)
	path := filepath.Join(dataDir, "guard.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Printf("[guard] parse %s: %v, using defaults", path, err)
		return cfg
	}
	if cfg.LLMSnapshot == "" {
		cfg.LLMSnapshot = filepath.Join(dataDir, "llm_snapshot.json")
	}
	parseDur := func(s string, def time.Duration) time.Duration {
		if s == "" {
			return def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return def
		}
		return d
	}
	cfg.heartbeatTimeout = parseDur(cfg.HeartbeatTimeout, 30*time.Second)
	cfg.heartbeatInterval = parseDur(cfg.HeartbeatInterval, 5*time.Second)
	cfg.rebootGrace = parseDur(cfg.RebootGrace, 10*time.Second)
	cfg.Recovery.attemptTimeout = parseDur(cfg.Recovery.AttemptTimeout, 120*time.Second)
	if cfg.MaxRestarts < 1 {
		cfg.MaxRestarts = 1
	}
	if cfg.LastResort != "reboot" && cfg.LastResort != "restart_app" {
		cfg.LastResort = "restart_app"
	}
	if len(cfg.RestartCommand) == 0 {
		cfg.RestartCommand = []string{"systemctl", "restart", "homeagent"}
	}
	if cfg.Recovery.MaxAttempts < 1 {
		cfg.Recovery.MaxAttempts = 1
	}
	// 缺省 failback 插件集
	if len(cfg.Recovery.Plugins) == 0 {
		cfg.Recovery.Plugins = []string{"webui", "pluginmgr", "recoverydiag"}
	}
	return cfg
}

// runGuard 父守护主循环：拉起 worker（--role=agent），心跳探活 + waitpid，
// 崩溃后按序 重试→LLM 配置基线恢复→failback 受限启动（N 轮有界，每轮复检）→L2 最后手段。
func runGuard(dataDir string) {
	cfg := loadGuardConfig(dataDir)
	applySystemConfig(cfg)
	hbPath := filepath.Join(dataDir, "heartbeat")
	os.MkdirAll(filepath.Join(dataDir, "log"), 0755)

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("[guard] resolve executable: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("[guard] starting, data=%s max_restarts=%d hb_timeout=%v last_resort=%s failback_rounds=%d",
		dataDir, cfg.MaxRestarts, cfg.heartbeatTimeout, cfg.LastResort, cfg.Recovery.MaxAttempts)

	// 启动即捕获网络基线（L0：写前留档 + L1 还原依据），并给当前 worker 恢复代理环境
	captureNetworkBaseline(dataDir)

	failures := 0        // 连续 normal 崩溃次数
	failbackRound := 0   // 已进行的 failback 轮次
	failbackDone := false // 本轮 failback 是否已进入

	for {
		// 决定本次 boot 模式：normal 崩溃超限 → 进入 failback
		boot := "normal"
		if cfg.FailbackEnabled && !failbackDone && failures >= cfg.MaxRestarts {
			log.Printf("[guard] %d failures >= max %d, entering failback (Safe-Mode)", failures, cfg.MaxRestarts)
			// L1 前置：恢复 LLM 基线 + 还原 DNS/proxy + 写恢复任务
			restoreLLMBaseline(dataDir, cfg.LLMSnapshot)
			restoreNetworkBaseline(dataDir)
			failbackRound = 1
			failbackDone = true
			if err := writeRecoveryTask(dataDir, cfg, failbackRound); err != nil {
				log.Printf("[guard] write recovery task: %v", err)
			}
			boot = "failback"
		} else if failbackDone {
			boot = "failback"
		}

		// 恢复 agent 环境（代理变量）在 worker 拉起前注入
		if b, err := system.LoadNetworkBaseline(dataDir); err == nil {
			b.ApplyProxyEnv()
		}

		outcome, spawnErr := spawnWorkerOnce(exe, dataDir, boot, hbPath, cfg, sigCh)
		if spawnErr != nil {
			log.Printf("[guard] spawn worker: %v, last_resort=%s", spawnErr, cfg.LastResort)
			doLastResort(cfg)
			failures = 0
			failbackDone = false
			failbackRound = 0
			time.Sleep(2 * time.Second)
			continue
		}

		switch outcome {
		case workerCleanExit:
			log.Printf("[guard] worker exited cleanly, guard exiting")
			return
		case workerRestartRequested:
			log.Printf("[guard] worker requested restart, respawning")
			continue
		case workerRecovered:
			// failback 成功：重置失败轮次，交回主 agent（下一轮 normal boot）
			log.Printf("[guard] failback recovery succeeded, handing back to main agent")
			failures = 0
			failbackDone = false
			failbackRound = 0
			continue
		case workerCrash:
			// fallthrough below
		}
		failures++

		if failbackDone {
			// failback 轮次内崩溃：读结果判定是否成功
			if res, err := recovery.LoadResult(recovery.ResultPath(dataDir)); err == nil && res.Success {
				log.Printf("[guard] failback round %d result success: %s", failbackRound, res.Quote())
				failures = 0
				failbackDone = false
				failbackRound = 0
				continue
			}
			if failbackRound >= cfg.Recovery.MaxAttempts {
				log.Printf("[guard] failback exhausted after %d rounds, L2 last_resort=%s", failbackRound, cfg.LastResort)
				if cfg.LastResortCfg.SnapshotBefore {
					rollbackAgentFS(dataDir)
				}
				doLastResort(cfg)
				failures = 0
				failbackDone = false
				failbackRound = 0
				time.Sleep(2 * time.Second)
				continue
			}
			// 进入下一轮 failback
			failbackRound++
			log.Printf("[guard] failback round %d failed, next round %d (max %d)", failbackRound-1, failbackRound, cfg.Recovery.MaxAttempts)
			if err := writeRecoveryTask(dataDir, cfg, failbackRound); err != nil {
				log.Printf("[guard] write recovery task: %v", err)
			}
			continue
		}

		if failures >= cfg.MaxRestarts {
			log.Printf("[guard] %d normal failures, entering failback next loop", failures)
		}
	}
}

type workerOutcome int

const (
	workerCleanExit workerOutcome = iota
	workerCrash
	workerRestartRequested
	workerRecovered
)

// spawnWorkerOnce 拉起一个 worker 并监控到其退出。
// 返回 (outcome workerOutcome, spawnErr error)；spawnErr 非 nil 表示未能拉起进程。
func spawnWorkerOnce(exe, dataDir, boot, hbPath string, cfg *guardConfig, sigCh chan os.Signal) (workerOutcome, error) {
	cmd := exec.Command(exe, "--role=agent", "--data="+dataDir, "--boot="+boot)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return workerCrash, err
	}
	pid := cmd.Process.Pid
	log.Printf("[guard] worker up pid=%d boot=%s", pid, boot)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	hbTicker := time.NewTicker(cfg.heartbeatInterval)
	defer hbTicker.Stop()

	ipcClient := ipc.NewClient(dataDir)

	for {
		select {
		case sig := <-sigCh:
			log.Printf("[guard] signal %v, stopping worker", sig)
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				<-done
			}
			return workerCleanExit, nil
		case err := <-done:
			if err == nil {
				log.Printf("[guard] worker pid=%d exited cleanly", pid)
				return workerCleanExit, nil
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				switch ee.ExitCode() {
				case exitRestartRequested:
					log.Printf("[guard] worker pid=%d requested restart (exit %d), respawning without counting failure", pid, exitRestartRequested)
					return workerRestartRequested, nil
				case exitRecovered:
					log.Printf("[guard] worker pid=%d recovered main agent (exit %d)", pid, exitRecovered)
					return workerRecovered, nil
				case exitRecoveryFailed:
					log.Printf("[guard] worker pid=%d failback attempt failed (exit %d)", pid, exitRecoveryFailed)
					return workerCrash, nil
				}
			}
			log.Printf("[guard] worker pid=%d crashed: %v", pid, err)
			return workerCrash, nil
		case <-hbTicker.C:
			// IPC PING/ACK 存活判定（带自诊断上报），失败回退文件心跳 mtime
			if st, perr := ipcClient.Ping(cfg.heartbeatTimeout); perr != nil {
				if heartbeatStale(hbPath, cfg.heartbeatTimeout) {
					log.Printf("[guard] heartbeat stale for pid=%d (ipc: %v), treating as hang, SIGKILL", pid, perr)
					cmd.Process.Kill()
					<-done
					return workerCrash, nil
				}
			} else if st != nil && st.LastDiag != "" {
				log.Printf("[guard] worker pid=%d self-diagnosis: %s", pid, st.LastDiag)
			}
		}
	}
}

func heartbeatStale(path string, timeout time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		// 无心跳文件：worker 刚启动或异常；宽限处理
		return false
	}
	return time.Since(fi.ModTime()) > timeout
}

// restoreLLMBaseline 从文件基线恢复 core.llm.* 配置（存在才恢复）。
func restoreLLMBaseline(dataDir, snapPath string) {
	if _, err := os.Stat(snapPath); err != nil {
		log.Printf("[guard] no llm snapshot baseline at %s, skip restore", snapPath)
		return
	}
	snap, err := internalConfig.LoadLLMSnapshot(snapPath)
	if err != nil {
		log.Printf("[guard] load llm snapshot %s: %v", snapPath, err)
		return
	}
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(dataDir, "config.db"))
	defer cfgReg.Close()
	if err := cfgReg.RestoreCoreLLM(snap); err != nil {
		log.Printf("[guard] restore llm baseline: %v", err)
		return
	}
	log.Printf("[guard] restored %d llm keys from %s", len(snap), snapPath)
}

// captureNetworkBaseline L0：启动即捕获网络基线（DNS/hosts/代理），供 failback 还原。
func captureNetworkBaseline(dataDir string) {
	base, err := system.CaptureNetwork()
	if err != nil {
		log.Printf("[guard] capture network baseline: %v", err)
		return
	}
	if err := system.SaveNetworkBaseline(dataDir, base); err != nil {
		log.Printf("[guard] save network baseline: %v", err)
		return
	}
	log.Printf("[guard] network baseline captured: %s", base.Summary())
}

// restoreNetworkBaseline L1：把 resolv.conf/hosts 还原到基线（DNS/proxy 小修命中即停）。
func restoreNetworkBaseline(dataDir string) {
	base, err := system.LoadNetworkBaseline(dataDir)
	if err != nil {
		log.Printf("[guard] no network baseline, skip DNS/proxy restore: %v", err)
		return
	}
	changed, err := base.RestoreFiles()
	if err != nil {
		log.Printf("[guard] restore network baseline: %v", err)
		return
	}
	if len(changed) > 0 {
		log.Printf("[guard] restored network files: %v", changed)
	} else {
		log.Printf("[guard] network baseline already consistent")
	}
}

// writeRecoveryTask 写本轮 failback 恢复任务文件。
func writeRecoveryTask(dataDir string, cfg *guardConfig, attempt int) error {
	task := &recovery.Task{
		Attempt:        attempt,
		MaxAttempts:    cfg.Recovery.MaxAttempts,
		AttemptTimeout: cfg.Recovery.AttemptTimeout,
		TriggerPrompt:  cfg.Recovery.TriggerPrompt,
		KnowledgeBase:  cfg.Recovery.KnowledgeBase,
		PluginList:     cfg.Recovery.Plugins,
	}
	for _, s := range cfg.LLM.Sources {
		if s.Name == "rescue" || s.Name != "" {
			task.RescueSource = s
			break
		}
	}
	return recovery.SaveTask(recovery.TaskPath(dataDir), task)
}

// rollbackAgentFS L2：guard 在 worker 离线时回滚 agentfs 最近快照（read changesets 逆应用）。
func rollbackAgentFS(dataDir string) {
	workDir := filepath.Join(dataDir, "agentfs")
	trk := tracker.NewOfflineTracker(dataDir, workDir)
	n, err := trk.RollbackFromDisk()
	if err != nil {
		log.Printf("[guard] agentfs rollback: %v", err)
		return
	}
	log.Printf("[guard] agentfs rolled back %d change sets", n)
}

func doLastResort(cfg *guardConfig) {
	switch cfg.LastResort {
	case "reboot":
		log.Printf("[guard] last_resort=reboot, sync + reboot in %v", cfg.rebootGrace)
		time.Sleep(cfg.rebootGrace)
		syscall.Sync()
		if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
			log.Printf("[guard] reboot failed (likely no privilege): %v", err)
		}
	case "restart_app":
		cmd := exec.Command(cfg.RestartCommand[0], cfg.RestartCommand[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("[guard] restart_app command %s: %v", strings.Join(cfg.RestartCommand, " "), err)
		}
	}
}

var _ = fmt.Sprintf

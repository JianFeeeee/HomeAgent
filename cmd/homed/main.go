package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agentPkg "gitcode.com/JianFeeeee/HomeAgent/internal/agent"
	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/ipc"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	logpkg "gitcode.com/JianFeeeee/HomeAgent/internal/log"
	luapkg "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/pipeline"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/social"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/meta"
	"gitcode.com/JianFeeeee/HomeAgent/internal/nlp"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	_ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins"
	_ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/clawhubadapter"
	cli "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/cli"
	_ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/healthcheck"
	_ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/pluginmgr"
	_ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/webui"
	"gitcode.com/JianFeeeee/HomeAgent/internal/recovery"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

func main() {
	dataDir := flag.String("data", "", "data directory (default: auto-detect next to binary)")
	httpAddr := flag.String("webui", "", "webui listen address (default: webui.listen_addr from config)")
	cliSocket := flag.String("socket", "", "cli unix socket path (default: <data>/cli.sock)")
	role := flag.String("role", "agent", "process role: guard (父守护) | agent (工作进程)")
	boot := flag.String("boot", "normal", "agent boot mode: normal | failback (受限启动，仅 failback 插件集)")
	flag.Parse()

	// 父守护模式：只负责拉起/守护 worker，不初始化 agent 内核
	if *role == "guard" {
		runGuard(resolveDataDir(*dataDir))
		return
	}

	log.Printf("[homed] role=agent boot=%s", *boot)

	if *dataDir == "" {
		*dataDir = resolveDataDir(*dataDir)
	}

	if *cliSocket == "" {
		*cliSocket = filepath.Join(*dataDir, "cli.sock")
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// 文件日志：同时输出到控制台和 data/log/ 目录
	logDir := filepath.Join(*dataDir, "log")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("[homed] warning: cannot create log dir: %v", err)
	} else {
		logPath := filepath.Join(logDir, fmt.Sprintf("homed_%s.log", time.Now().Format("2006-01-02_15-04-05")))
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("[homed] warning: cannot open log file: %v", err)
		} else {
			log.SetOutput(io.MultiWriter(os.Stderr, logFile))
			log.Printf("[homed] logging to %s", logPath)
		}
	}

	log.Printf("[homed] starting %s", meta.FullVersion())

	agentWorkDir := filepath.Join(*dataDir, "agentfs")
	dirs := []string{
		*dataDir,
		filepath.Join(*dataDir, "snapshots"),
		filepath.Join(*dataDir, "plugins"),
		filepath.Join(*dataDir, "changesets"),
		filepath.Join(*dataDir, "memory"),
		filepath.Join(*dataDir, "memory", "raw"),
		filepath.Join(*dataDir, "adapters"),
		agentWorkDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("create dir %s: %v", d, err)
		}
	}

	// ========================================================================
	// 基础设施层：记忆、技能
	// ========================================================================

	memDB, err := memory.NewGraphDB(filepath.Join(*dataDir, "memory", "graph.db"))
	if err != nil {
		log.Printf("[homed] warning: memory init failed: %v", err)
		memDB = nil
	} else {
		log.Printf("[homed] graph memory initialized")
	}
	if memDB != nil {
		defer memDB.Close()
	}

	memIdx := memory.NewIndexer(memDB)
	memIdx.Sync() // 启动时立即同步，避免前30分钟空窗
	socialStore := social.New(memDB)

	distiller := pipeline.NewDistiller(memDB, *dataDir, pipeline.DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	if memDB != nil {
		distiller.Start()
		defer distiller.Stop()
	}

	// ========================================================================
	// 配置中心（SQLite 持久化，唯一配置源）
	// ========================================================================

	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(*dataDir, "config.db"))
	defer cfgReg.Close()
	cfgReg.SeedDefaults(*dataDir)
	// LLM 配置写前留档（config_set 写 core.llm.* 前自动快照），guard 恢复用基线
	cfgReg.SetLLMSnapshotFile(filepath.Join(*dataDir, "llm_snapshot.json"))
	cfg := cfgReg.ToConfig()

	// 共享词嵌入：蒸馏提取（Phase 3 TransE 验证）与 Agent 上下文复用同一实例，
	// 避免同一模型被二次加载（约 200k×300 维 ≈ 数百 MB 内存）。
	embedder := memory.NewStaticEmbedder(strings.Split(cfgReg.GetString("core.agent.embedding_model_path", ""), ",")...)
	distiller.SetEmbedder(embedder)

	// ========================================================================
	// Lua VM（LLM 协议适配）
	// ========================================================================

	luaVM := luapkg.NewVM(filepath.Join(cfg.Daemon.DataDir, "adapters"))
	if err := luaVM.Start(); err != nil {
		log.Printf("[homed] warning: lua vm init failed: %v", err)
	} else {
		defer luaVM.Stop()
	}

	// ========================================================================
	// 守护管理（代理生命周期管理）
	// ========================================================================

	sup := supervisor.New(cfg)
	if err := sup.Start(); err != nil {
		log.Fatalf("start supervisor: %v", err)
	}

	// ========================================================================
	// 变更追踪（overlayfs）
	// ========================================================================

	trk := tracker.NewTracker(cfg.Daemon.DataDir, agentWorkDir,
		tracker.WithKeepChangesets(100),
		tracker.WithMaxChangesetAge(30*24*time.Hour),
	)
	if err := trk.Init(); err != nil {
		log.Printf("[homed] warning: tracker init: %v", err)
	} else {
		if err := trk.Start(); err != nil {
			log.Printf("[homed] warning: tracker mount overlay: %v (non-fatal: no overlayfs support?)", err)
		} else {
			log.Printf("[homed] change tracker active at %s", trk.MergeDir())
		}
	}

	// ========================================================================
	// 内核 API：IOManager（IO 抽象层） + EventBus（事件总线）
	// 所有插件通过这两个通道与核心交互
	// ========================================================================

	iom := agentIO.NewIOManager()
	evBus := events.NewBus()
	log.Printf("[homed] kernel API ready: IOManager + EventBus")

	// ========================================================================
	// 文本记忆 + 记忆蒸馏管线
	// ========================================================================

	textMem := text.New(filepath.Join(cfg.Daemon.DataDir, "memory", "text"))
	if err := textMem.Start(); err != nil {
		log.Printf("[homed] warning: text memory start: %v", err)
	} else {
		defer textMem.Stop()
		log.Printf("[homed] text memory active at %s", filepath.Join(cfg.Daemon.DataDir, "memory", "text"))
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-iom.OutputChan():
				if !ok {
					return
				}
				if evt.Target == "memory" && evt.Type == "memory_candidate" {
					source, _ := evt.Payload["source"].(string)
					input, _ := evt.Payload["input"].(string)
					response, _ := evt.Payload["response"].(string)
					toolsUsed, _ := evt.Payload["tools_used"].([]string)
					toolResults, _ := evt.Payload["tool_results"].([]interface{})
					agentID, _ := evt.Payload["agent_id"].(string)

					if input != "" && textMem != nil {
						te := text.Event{
							Timestamp: time.Now().Unix(),
							Source:    source,
							Input:     input,
							Response:  response,
							ToolsUsed: toolsUsed,
							AgentID:   agentID,
						}
						if err := textMem.Append(te); err != nil {
							log.Printf("[homed] text memory append: %v", err)
						}
					}

					if input != "" && memDB != nil {
						distiller.Append("agent", "user", input)
					}
					if response != "" && memDB != nil {
						distiller.Append("agent", "assistant", response)
					}

					// 工具输出接入蒸馏管线
					for _, tr := range toolResults {
						if trMap, ok := tr.(map[string]interface{}); ok {
							if text, ok := trMap["output"].(string); ok && text != "" && memDB != nil {
								distiller.Append("agent", "tool", text)
							}
						}
					}
				}
			}
		}
	}()

	// ========================================================================
	// LLM Provider 管理（多源，通过 Lua 适配器协议转换）
	// ========================================================================

	apiKey := cfg.LLM.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("LLM_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	baseAPIKey := apiKey

	providerMgr := agentAPI.NewProviderManager()
	for _, src := range cfg.LLM.Sources {
		if !agentAPI.IsValidSourceConfig(src.Name, src.BaseURL, src.Model, src.Adapter) {
			log.Printf("[homed] skip invalid llm source %q (base_url=%q model=%q adapter=%q)", src.Name, src.BaseURL, src.Model, src.Adapter)
			continue
		}
		key := src.APIKey
		if key == "" {
			key = baseAPIKey
		}
		luaProvider := agentAPI.NewLuaAdaptedProvider(agentAPI.BaseConfig{
			Model:         src.Model,
			BaseURL:       src.BaseURL,
			APIKey:        key,
			Temperature:   cfg.LLM.Temperature,
			MaxTokens:     cfg.LLM.MaxTokens,
			ContextWindow: src.ContextWindow,
		}, luaVM, src.Name, src.Adapter)
		providerMgr.Register(src.Name, luaProvider)
	}
	if cfg.LLM.Provider != "" {
		providerMgr.SetDefault(cfg.LLM.Provider)
	}
	provider := providerMgr.Default()

	// L1 failback：受限 worker 启动即跑恢复梯子（probe→还原DNS/proxy→还原config+ReloadFromConfig→probe），
	// 结果以退出码 exitRecovered=43 / exitRecoveryFailed=44 交回 guard，不进入主 agent 循环。
	if *boot == "failback" {
		runFailbackRecovery(*dataDir, cfgReg, luaVM, providerMgr, baseAPIKey)
	}

	// ========================================================================
	// 文档记忆 + 知识库
	// ========================================================================

	docStore := document.NewStore(filepath.Join(cfg.Daemon.DataDir, "memory", "documents"))
	if err := docStore.Start(); err != nil {
		log.Printf("[homed] warning: document store: %v", err)
	}

	ks := knowledge.NewStore(filepath.Join(cfg.Daemon.DataDir, "knowledge"))
	if err := ks.Start(); err != nil {
		log.Printf("[homed] warning: knowledge store: %v", err)
	} else {
		log.Printf("[homed] knowledge store active with %d items", len(ks.List()))
	}

	// ========================================================================
	// 人格设定
	// ========================================================================

	personalPath := filepath.Join(cfg.Daemon.DataDir, "personal", "personal.md")
	personality, err := agentPkg.LoadPersonality(personalPath)
	if err != nil {
		log.Printf("[homed] warning: load personality: %v", err)
	}
	if personality != nil && personality.Content != "" {
		log.Printf("[homed] personality loaded (%d bytes)", len(personality.Content))
	}

	// ========================================================================
	// 阶段管道（StageHost）+ 插件系统（Registry）
	// ========================================================================

	stageHost := agentCore.NewStageHost()

	pluginReg := plugin.NewRegistry()
	pluginReg.SetIOManager(iom)
	pluginReg.SetEventBus(evBus)
	pluginReg.SetMemory(memDB)
	pluginReg.SetTextMemory(textMem)
	pluginReg.SetDocStore(docStore)
	pluginReg.SetKnowledge(ks)
	pluginReg.SetProviderManager(providerMgr)
	pluginReg.SetConfigRegistry(cfgReg)
	pluginReg.SetPluginDir(cfg.Plugin.Dir)

	// Wire registration callbacks: plugins' RegisterTool/RegisterStage → StageHost
	pluginReg.SetToolRegistrar(func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		log.Printf("[homed] SetToolRegistrar registering tool: %s (plugin=%s)", name, def.Plugin)
		return stageHost.RegisterTool(name, def, handler)
	})
	pluginReg.SetStageRegistrar(func(stage sdk.Stage, handler sdk.StageHandler) {
		stageHost.RegisterStage(stage, handler)
	})
	pluginReg.SetAPIRegistrar(func(name string) error {
		return nil
	})
	pluginReg.SetToolCleaner(stageHost)

	// ========================================================================
	// Agent Core (需在插件加载前创建，因为插件 Configure 需要 StatusProvider)
	// ========================================================================

	defaultPrompt := `你是 HomeAgent，一个持续运行的个人管家。
你的每次回复会自动发送到当前输出通道（默认=输入源），无需额外工具。
如需切换回复通道，使用 output_set_channel。
如需异步发送消息或通知，使用 output_send 指定通道和内容。
使用 output_list_channels 查看可用通道及其能力。

可用工具列表会由系统自动传入，按需使用即可。以下是你尤其需要关注的几类工具：
- memory_* — 图记忆（长期记忆，记录和查询个人信息/事实）
- knowledge_* — 知识库（查阅预设知识文档）
- doc_* — 文档记忆（近期对话的存档，查询后自动清除）
- person_* — 人物特质与社交关系网
- llm_* — LLM 源管理（列出/切换模型提供商）
- output_* — 输出通道管理（切换/发送消息）
- timer_set — 设置定时提醒
- plgreload — 热重载插件
- spawn_child — 生成子 Agent 执行独立任务
- describe_image — 描述用户上传的图片
- transcribe_audio — 转写用户上传的音频
- ocr_image — 识别图片中的文字

当用户上传图片或音频时，系统会自动附着媒体内容。如果模型不支持直接处理多媒体，请使用上述工具。

回复你的真实想法，用自然语言与用户交流。`
	sysPrompt := cfgReg.GetString("core.agent.system_prompt", defaultPrompt)
	if sysPrompt == "" {
		sysPrompt = defaultPrompt
	}

	agent := agentCore.New(agentCore.AgentConfig{
		ID:                 "main",
		SystemPrompt:       sysPrompt,
		Provider:           provider,
		ProviderManager:    providerMgr,
		IO:                 iom,
		Memory:             memDB,
		Indexer:            memIdx,
		Tracker:            trk,
		DocStore:           docStore,
		Knowledge:          ks,
		SocialStore:        socialStore,
		TextMemory:         textMem,
		Personality:        personality,
		PluginReg:          pluginReg,
		PluginDir:          cfg.Plugin.Dir,
		DistillInterval:    cfgReg.GetDuration("core.agent.distill_interval", 30*time.Minute),
		ArchiveInterval:    cfgReg.GetDuration("core.agent.archive_interval", 60*time.Minute),
		ReviewInterval:     cfgReg.GetDuration("core.agent.review_interval", 120*time.Minute),
		MergeInterval:      cfgReg.GetDuration("core.agent.merge_interval", 120*time.Minute),
		ContextSavePath:    filepath.Join(cfg.Daemon.DataDir, "memory", "context.json"),
		EmbeddingModelPath: cfgReg.GetString("core.agent.embedding_model_path", ""),
		Embedder:           embedder,
		StageHost:          stageHost,
		EventBus:           evBus,
		ThinkingEnabled:    cfg.LLM.ThinkingEnabled,
		InputProcessing:    cfg.InputProcessing,
	})

	// 通过 Registry 将内核依赖注入每个插件的 PluginSDK（阶段6 将替换遗留的 util.Configure）
	pluginReg.SetLuaVM(luaVM)
	pluginReg.SetBaseAPIKey(baseAPIKey)
	pluginReg.SetSupervisor(supervisor.NewSDKAdapter(sup))
	pluginReg.SetTracker(trk)
	pluginReg.SetConfig(cfg)
	pluginReg.SetStageHost(stageHost)
	pluginReg.SetIndexer(memIdx)
	pluginReg.SetStatusProvider(agent)

	// 为内置插件注入内核依赖（各插件通过 init() 自注册工厂）
	cli.DefaultSocket = *cliSocket
	// webui 插件作为内置插件经 Registry 启动，读取自身 settings["addr"]（默认 :8080）。
	// 保留 CLI --webui 与 webui.listen_addr 配置对监听地址的覆盖。
	webuiListenAddr := *httpAddr
	if webuiListenAddr == "" {
		webuiListenAddr = cfgReg.GetString("webui.listen_addr", ":8080")
	}
	if ps := cfgReg.PluginConfig("webui"); ps != nil {
		if v, _ := ps.Get("addr"); v == nil {
			_ = ps.Set("addr", webuiListenAddr)
		}
	}

	// ========================================================================
	// 依存句法分析器（内嵌 ONNX 模型 / 规则引擎）
	// ========================================================================

	modelPath := cfgReg.GetString("core.agent.onnx_model_path", "")
	onnxParser, err := nlp.NewONNXParser(nlp.ONNXConfig{
		ModelPath: modelPath,
		DataDir:   filepath.Join(cfg.Daemon.DataDir, "nlp"),
	})
	if err != nil {
		log.Printf("[homed] warn: ONNX parser init: %v, using fallback", err)
	} else {
		nlp.SetDefaultParser(onnxParser)
		log.Printf("[homed] dep parser initialized (model: %s)", modelPath)
	}

	// Auto-create plugins directory (without hardcoding plugin names)
	os.MkdirAll(cfg.Plugin.Dir, 0755)

	// failback 受限启动：仅装载 failback 插件集（webfetch/files/cmd 为内核内置，
	// 此处仅控制外部插件，默认含 recoverydiag 以便直接在受限态产出恢复结论）
	if *boot == "failback" {
		list := cfgReg.GetString("core.agent.failback_plugins", "webui,pluginmgr,recoverydiag")
		// 优先使用 guard.yaml 经过 recovery 任务下发的插件集（guard 是 failback 权威）
		if task, terr := recovery.LoadTask(recovery.TaskPath(*dataDir)); terr == nil && len(task.Plugins()) > 0 {
			list = strings.Join(task.Plugins(), ",")
		}
		var names []string
		for _, s := range strings.Split(list, ",") {
			if s = strings.TrimSpace(s); s != "" {
				names = append(names, s)
			}
		}
		pluginReg.SetLoadAllowlist(names)
		log.Printf("[homed] failback boot: plugin allowlist = %v", names)
	}

	// Load all plugins — each scans its own dir and is loaded via factory or .so
	if err := pluginReg.Load(cfg.Plugin.Dir); err != nil {
		log.Printf("[homed] warning: load plugins: %v", err)
	}
	log.Printf("[homed] stage host ready with %d registered tools", stageHost.ToolCount())

	// 日志管理：层级压缩 + 保留策略
	logManager := logpkg.NewManager(logDir, cfgReg)
	go logManager.Start(ctx)
	defer logManager.Stop()

	agent.Start()
	defer agent.Stop()

	// PING/ACK 心跳服务：worker 监听 unix socket，guard 发 PING、worker 回 ACK
	// （含自诊断 kernel 状态快照），替换纯文件心跳。文件心跳保留作回退。
	ipcServer := ipc.NewServer(*dataDir, func() *ipc.Status {
		st := agent.GetKernelStatus()
		llmOK := st != nil && st.LLM.Available
		tools := 0
		if st != nil {
			tools = len(st.Tools)
		}
		uptime := int64(0)
		if st != nil {
			if d, err := time.ParseDuration(st.Uptime); err == nil {
				uptime = int64(d.Seconds())
			}
		}
		return &ipc.Status{
			PID:       os.Getpid(),
			Boot:      *boot,
			UptimeSec: uptime,
			LLMOK:     &llmOK,
			Tools:     tools,
			LastDiag:  lastDiagSummary(*dataDir),
		}
	})
	if err := ipcServer.Start(); err != nil {
		log.Printf("[homed] warning: ipc heartbeat server: %v", err)
	} else {
		defer ipcServer.Stop()
	}

	sup.SetTracker(trk)
	sup.RegisterAgent("main")

	// 真实存活源 + 重启通道：daemon 心跳语义由此修正（lastHB 只在确认存活时更新），
	// 重启动作不再空转——清理后以特殊退出码交给 guard/systemd 重建。
	restartCh := make(chan struct{}, 1)
	sup.SetHeartbeatSource(func(id types.AgentID) (time.Time, types.HealthStatus, error) {
		st := agent.GetKernelStatus()
		if st == nil {
			return time.Time{}, types.HealthDown, fmt.Errorf("no kernel status")
		}
		h := types.HealthHealthy
		if !st.LLM.Available {
			h = types.HealthDegraded
		}
		return time.Now(), h, nil
	})
	sup.SetRestartHandler(func(id types.AgentID) {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	})

	log.Printf("[homed] main agent started, model=%s base=%s sources=%d adapters=%d",
		cfg.LLM.Model, cfg.LLM.BaseURL, len(cfg.LLM.Sources), len(luaVM.ListAdapters()))
	log.Printf("[homed] kernel ready, waiting for plugin IO...")

	// ========================================================================
	// 等待退出信号
	// ========================================================================

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 心跳：每 5s 触碰 <data>/heartbeat，guard 据此判定工作进程是否存活/卡死
	hbPath := filepath.Join(*dataDir, "heartbeat")
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		writeHB := func() {
			if f, err := os.OpenFile(hbPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
				fmt.Fprintf(f, "t=%d\n", time.Now().Unix())
				f.Close()
			}
		}
		writeHB()
		for {
			select {
			case <-t.C:
				writeHB()
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	restartRequested := false
	select {
	case <-sigCh:
		log.Printf("[homed] shutting down...")
	case <-restartCh:
		restartRequested = true
		log.Printf("[homed] restart requested, shutting down cleanly then exiting with code %d", exitRestartRequested)
	}

	close(hbStop)
	pluginReg.StopAll()
	if trk != nil {
		trk.Stop()
	}
	if err := cfgReg.Flush(); err != nil {
		log.Printf("[homed] flush config: %v", err)
	}
	sup.Shutdown()
	log.Printf("[homed] stopped")

	if restartRequested {
		os.Exit(exitRestartRequested)
	}
}

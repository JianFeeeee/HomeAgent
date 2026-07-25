package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	agentPkg "gitcode.com/JianFeeeee/HomeAgent/internal/agent"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	logpkg "gitcode.com/JianFeeeee/HomeAgent/internal/log"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/meta"
	luapkg "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/pipeline"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/social"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	cli "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/cli"
	healthcheck "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/healthcheck"
	openclaw "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/clawhubadapter"
	pluginmgr "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/pluginmgr"
	webui "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/webui"
	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	_ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins"
)

func main() {
	dataDir := flag.String("data", "", "data directory (default: auto-detect next to binary)")
	httpAddr := flag.String("webui", "", "webui listen address (default: webui.listen_addr from config)")
	cliSocket := flag.String("socket", "", "cli unix socket path (default: <data>/cli.sock)")
	flag.Parse()

	if *dataDir == "" {
		exe, err := os.Executable()
		if err == nil {
			*dataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			if exe, err := exec.LookPath(os.Args[0]); err == nil {
				*dataDir = filepath.Join(filepath.Dir(exe), "data")
			} else {
				*dataDir = "./data"
			}
		}
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
		filepath.Join(*dataDir, "skills"),
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

	skMgr := skill.NewManager(filepath.Join(*dataDir, "skills"))
	if err := skMgr.Init(); err != nil {
		log.Printf("[homed] warning: skill init failed: %v", err)
	}

	// ========================================================================
	// 配置中心（SQLite 持久化，唯一配置源）
	// ========================================================================

	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(*dataDir, "config.db"))
	defer cfgReg.Close()
	cfgReg.SeedDefaults(*dataDir)
	cfg := cfgReg.ToConfig()

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
		key := src.APIKey
		if key == "" {
			key = baseAPIKey
		}
		luaProvider := agentAPI.NewLuaAdaptedProvider(agentAPI.BaseConfig{
			Model:       src.Model,
			BaseURL:     src.BaseURL,
			APIKey:      key,
			Temperature: cfg.LLM.Temperature,
			MaxTokens:   cfg.LLM.MaxTokens,
		}, luaVM, src.Adapter)
		providerMgr.Register(src.Name, luaProvider)
	}
	if cfg.LLM.Provider != "" {
		providerMgr.SetDefault(cfg.LLM.Provider)
	}
	provider := providerMgr.Default()

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
		ID:              "main",
		SystemPrompt:    sysPrompt,
		Provider:        provider,
		ProviderManager: providerMgr,
		IO:              iom,
		Memory:          memDB,
		Indexer:         memIdx,
		Skills:          skMgr,
		Tracker:         trk,
		DocStore:        docStore,
		Knowledge:       ks,
		SocialStore:     socialStore,
		TextMemory:      textMem,
		Personality:     personality,
		PluginReg:       pluginReg,
		PluginDir:       cfg.Plugin.Dir,
		ContextSavePath:    filepath.Join(cfg.Daemon.DataDir, "memory", "context.json"),
		EmbeddingModelPath: cfgReg.GetString("core.agent.embedding_model_path", ""),
		StageHost:          stageHost,
		EventBus:        evBus,
		ThinkingEnabled:  cfg.LLM.ThinkingEnabled,
		InputProcessing:  cfg.InputProcessing,
	})

	// 为内置插件注入内核依赖（各插件通过 init() 自注册工厂）
	cli.DefaultSocket = *cliSocket
	openclaw.SkillsDir = filepath.Join(cfg.Daemon.DataDir, "skills")
	webuiListenAddr := *httpAddr
	if webuiListenAddr == "" {
		webuiListenAddr = cfgReg.GetString("webui.listen_addr", ":8080")
	}
	webui.Configure(webuiListenAddr,
		sup, memDB, skMgr, luaVM, cfg, iom, textMem, ks, trk, cfgReg, pluginReg, evBus, agent,
		providerMgr, baseAPIKey,
	)
	healthcheck.Configure(stageHost, iom, pluginReg, memDB, ks, docStore, providerMgr, agent)

	// Wire pluginmgr dependencies
	pluginmgr.PluginDir = cfg.Plugin.Dir
	pluginmgr.Reg = pluginReg

	// CLI 插件结构化命令 — 直接注入内核依赖，不依赖 HTTP
	cli.Configure(pluginReg, cfgReg, agent, cfg.Plugin.Dir)

	// Auto-create plugins directory (without hardcoding plugin names)
	os.MkdirAll(cfg.Plugin.Dir, 0755)

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

	sup.SetTracker(trk)
	sup.RegisterAgent("main")

	log.Printf("[homed] main agent started, model=%s base=%s sources=%d adapters=%d",
		cfg.LLM.Model, cfg.LLM.BaseURL, len(cfg.LLM.Sources), len(luaVM.ListAdapters()))
	log.Printf("[homed] kernel ready, waiting for plugin IO...")

	// ========================================================================
	// 等待退出信号
	// ========================================================================

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("[homed] shutting down...")
	pluginReg.StopAll()
	if trk != nil {
		trk.Stop()
	}
	if err := cfgReg.Flush(); err != nil {
		log.Printf("[homed] flush config: %v", err)
	}
	sup.Shutdown()
	log.Printf("[homed] stopped")
}

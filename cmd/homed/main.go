package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "gitcode.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	agentPkg "gitcode.com/JianFeeeee/HomeAgent/internal/agent"
	"gitcode.com/JianFeeeee/HomeAgent/internal/api"
	"gitcode.com/JianFeeeee/HomeAgent/config"
	internalConfig "gitcode.com/JianFeeeee/HomeAgent/internal/config"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	luapkg "gitcode.com/JianFeeeee/HomeAgent/internal/lua"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/pipeline"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/onebot"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	"gitcode.com/JianFeeeee/HomeAgent/internal/skill"
	"gitcode.com/JianFeeeee/HomeAgent/internal/supervisor"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
)

func main() {
	configPath := flag.String("config", config.DefaultConfigPath, "path to config file")
	dataDir := flag.String("data", "/var/lib/homeagent", "data directory")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Printf("[homed] starting HomeAgent v0.1.0")

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	cfg.Daemon.DataDir = *dataDir

	agentWorkDir := filepath.Join(cfg.Daemon.DataDir, "agentfs")
	dirs := []string{
		cfg.Daemon.DataDir,
		filepath.Join(cfg.Daemon.DataDir, "snapshots"),
		filepath.Join(cfg.Daemon.DataDir, "skills"),
		filepath.Join(cfg.Daemon.DataDir, "plugins"),
		filepath.Join(cfg.Daemon.DataDir, "changesets"),
		filepath.Join(cfg.Daemon.DataDir, "memory"),
		filepath.Join(cfg.Daemon.DataDir, "memory", "raw"),
		filepath.Join(cfg.Daemon.DataDir, "adapters"),
		agentWorkDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("create dir %s: %v", d, err)
		}
	}

	// === Graph Memory ===
	memDB, err := memory.NewGraphDB(filepath.Join(cfg.Daemon.DataDir, "memory", "graph.db"))
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

	// === Memory Pipeline ===
	distiller := pipeline.NewDistiller(memDB, cfg.Daemon.DataDir, pipeline.DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	if memDB != nil {
		distiller.Start()
		defer distiller.Stop()
	}

	// === Skills ===
	skMgr := skill.NewManager(filepath.Join(cfg.Daemon.DataDir, "skills"))
	if err := skMgr.Init(); err != nil {
		log.Printf("[homed] warning: skill init failed: %v", err)
	}

	// === Plugin Registry (OpenClaw SKILL.md compatible) ===
	pluginReg := plugin.NewRegistry()
	// 注册内置原生插件工厂
	pluginReg.RegisterNative("qq", func(name string, config map[string]interface{}, iom *agentIO.IOManager) (agentIO.Device, error) {
		wsURL, _ := config["entry"].(string)
		if wsURL == "" {
			wsURL = "ws://127.0.0.1:6700"
		}
		accessToken, _ := config["access_token"].(string)
		return onebot.NewDevice(name, wsURL, accessToken, iom), nil
	})

	// === Config Registry (统一配置中心，供插件读写) ===
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(cfg.Daemon.DataDir, "settings.json"))
	cfgReg.Register("core.daemon.listen_addr", cfg.Daemon.ListenAddr)
	cfgReg.Register("core.daemon.data_dir", cfg.Daemon.DataDir)
	cfgReg.Register("core.daemon.heartbeat_interval", cfg.Daemon.HeartbeatInterval.String())
	cfgReg.Register("core.daemon.check_interval", cfg.Daemon.CheckInterval.String())
	cfgReg.Register("core.daemon.log_level", cfg.Daemon.LogLevel)
	cfgReg.Register("core.llm.provider", "lua_deepseek")
	cfgReg.Register("core.llm.model", cfg.LLM.Model)
	cfgReg.Register("core.llm.base_url", cfg.LLM.BaseURL)
	cfgReg.Register("core.llm.temperature", strconv.FormatFloat(cfg.LLM.Temperature, 'f', 2, 64))
	cfgReg.Register("core.llm.max_tokens", strconv.Itoa(cfg.LLM.MaxTokens))
	pluginReg.SetConfigRegistry(cfgReg)
	log.Printf("[homed] config registry active with %d keys", len(cfgReg.List("")))

	// === Supervisor ===
	sup := supervisor.New(cfg)
	if err := sup.Start(); err != nil {
		log.Fatalf("start supervisor: %v", err)
	}

	// === Lua VM ===
	luaVM := luapkg.NewVM(filepath.Join(cfg.Daemon.DataDir, "adapters"))
	if err := luaVM.Start(); err != nil {
		log.Printf("[homed] warning: lua vm init failed: %v", err)
	} else {
		defer luaVM.Stop()
	}

	// === IO Abstraction Layer (唯一输入路径) ===
	iom := agentIO.NewIOManager()

	// 插件绑定 IO 管理器 → 插件自动注册为 IO 设备
	pluginReg.SetIOManager(iom)
	// 首次加载插件
	if result, err := pluginReg.Reload(filepath.Join(cfg.Daemon.DataDir, "plugins")); err != nil {
		log.Printf("[homed] warning: load plugins: %v", err)
	} else {
		log.Printf("[homed] %s", result)
	}

	// === Text Memory (三层记忆: Context → Text → Graph) ===
	textMem := text.New(filepath.Join(cfg.Daemon.DataDir, "memory", "text"))
	if err := textMem.Start(); err != nil {
		log.Printf("[homed] warning: text memory start: %v", err)
	} else {
		defer textMem.Stop()
		log.Printf("[homed] text memory active at %s", filepath.Join(cfg.Daemon.DataDir, "memory", "text"))
	}

	// Wire IO output events → TextMemory + distiller → GraphMemory
	if distiller != nil {
		go func() {
			for evt := range iom.OutputChan() {
				if evt.Target == "memory" && evt.Type == "memory_candidate" {
					source, _ := evt.Payload["source"].(string)
					input, _ := evt.Payload["input"].(string)
					response, _ := evt.Payload["response"].(string)
					toolsUsed, _ := evt.Payload["tools_used"].([]string)
					agentID, _ := evt.Payload["agent_id"].(string)

					// 1. 写文本记忆（持久化原始日志）
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

					// 2. 喂蒸馏器（生成三元组 → 图记忆）
					if input != "" {
						distiller.Append("agent", "user", input)
					}
					if response != "" {
						distiller.Append("agent", "assistant", response)
					}
				}
			}
		}()
	}

	// === Change Tracker (overlayfs-based, 追踪所有修改) ===
	trk := tracker.NewTracker(cfg.Daemon.DataDir, agentWorkDir)
	if err := trk.Init(); err != nil {
		log.Printf("[homed] warning: tracker init: %v", err)
	} else {
		if err := trk.Start(); err != nil {
			log.Printf("[homed] warning: tracker mount overlay: %v (non-fatal: no overlayfs support?)", err)
		} else {
			log.Printf("[homed] change tracker active at %s", trk.MergeDir())
		}
	}

	// === API Provider (唯一输出路径) ===
	apiKey := cfg.LLM.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	provider := agentAPI.NewOpenAIProvider(agentAPI.BaseConfig{
		Model:       cfg.LLM.Model,
		BaseURL:     cfg.LLM.BaseURL,
		APIKey:      apiKey,
		Temperature: cfg.LLM.Temperature,
		MaxTokens:   cfg.LLM.MaxTokens,
	})

	// === Personality (固定人格内核) ===
	personalPath := filepath.Join(cfg.Daemon.DataDir, "personal", "personal.md")
	personality, err := agentPkg.LoadPersonality(personalPath)
	if err != nil {
		log.Printf("[homed] warning: load personality: %v", err)
	}
	if personality != nil && personality.Content != "" {
		log.Printf("[homed] personality loaded (%d bytes)", len(personality.Content))
	}

	// === Document Memory (第二层记忆：上下文→文档) ===
	docStore := document.NewStore(filepath.Join(cfg.Daemon.DataDir, "memory", "documents"))
	if err := docStore.Start(); err != nil {
		log.Printf("[homed] warning: document store: %v", err)
	}

	// === Knowledge Store (知识库) ===
	ks := knowledge.NewStore(filepath.Join(cfg.Daemon.DataDir, "knowledge"))
	if err := ks.Start(); err != nil {
		log.Printf("[homed] warning: knowledge store: %v", err)
	} else {
		log.Printf("[homed] knowledge store active with %d items", len(ks.List()))
	}

	// === Event Bus (系统事件总线) ===
	evBus := events.NewBus()
	log.Printf("[homed] event bus initialized")

	// === Stage Host (阶段管道编排) ===
	stageHost := agentCore.NewStageHost()
	stageHost.SyncFromRegistry(pluginReg)
	log.Printf("[homed] stage host initialized with %d plugin sdks", pluginReg.SDKPluginCount())

	// === Single Agent Core ===
	agent := agentCore.New(agentCore.AgentConfig{
		ID: "main",
		SystemPrompt: `你是 HomeAgent，一个持续运行的个人管家。
你的每次回复会自动发送到当前输出通道（默认=输入源），无需额外工具。
如需切换回复通道，使用 output_set_channel。
如需异步发送消息或通知，使用 output_send 指定通道和内容。
使用 output_list_channels 查看可用通道及其能力。

你有以下核心工具:
1. memory_recall — 查询图记忆（历史/个人信息）
2. memory_commit — 写入图记忆（记住新信息）
3. memory_introspect — 查看记忆统计
4. knowledge_search — 搜索知识库
5. doc_query — 查询文档记忆
6. doc_commit — 写入文档记忆
7. plgreload — 热重载插件

当用户问及个人信息或历史时，调用 memory_recall。
当用户告诉了你新的个人信息时，调用 memory_commit。
需要查询知识时使用 knowledge_search。
回复你的真实想法，用自然语言与用户交流。`,
		Provider:     provider,
		IO:           iom,
		Memory:       memDB,
		Indexer:      memIdx,
		Skills:       skMgr,
		Tracker:      trk,
		MaxToolTurns: 10,
		DocStore:     docStore,
		Knowledge:    ks,
		Personality:  personality,
		PluginReg:       pluginReg,
		PluginDir:       filepath.Join(cfg.Daemon.DataDir, "plugins"),
		ContextSavePath: filepath.Join(cfg.Daemon.DataDir, "memory", "context.json"),
		StageHost:       stageHost,
		EventBus:        evBus,
	})
	agent.Start()
	defer agent.Stop()

	// Wire supervisor with tracker + agent registration (after both exist)
	sup.SetTracker(trk)
	sup.RegisterAgent("main")

	log.Printf("[homed] main agent started, model=%s base=%s", cfg.LLM.Model, cfg.LLM.BaseURL)

	// === Built-in HTTP API & WebUI Plugin ===
	webui := api.NewWebUIPlugin(
		"webui", cfg.Daemon.ListenAddr,
		sup, memDB, skMgr, luaVM, cfg, iom, textMem, ks, trk, cfgReg, pluginReg,
	)
	iom.RegisterDevice(webui)
	webui.Start()
	defer webui.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("[homed] shutting down...")
	if trk != nil {
		trk.Stop()
	}
	if err := cfgReg.Flush(); err != nil {
		log.Printf("[homed] flush config: %v", err)
	}
	sup.Shutdown()
	log.Printf("[homed] stopped")
}



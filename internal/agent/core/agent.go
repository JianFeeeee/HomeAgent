package core

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	agentPkg "gitcode.com/JianFeeeee/HomeAgent/internal/agent"
	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
	"gitcode.com/JianFeeeee/HomeAgent/internal/events"
	"gitcode.com/JianFeeeee/HomeAgent/internal/knowledge"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/document"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/social"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
	"gitcode.com/JianFeeeee/HomeAgent/internal/tracker"
	"gitcode.com/JianFeeeee/HomeAgent/pkg/types"
)

// ContextEvent 和 RelevanceContext 定义在 context.go

// Agent — 单 agent，不区分会话/实例
type Agent struct {
	mu              sync.Mutex
	id              types.AgentID
	provider        agentAPI.Provider
	providerManager *agentAPI.ProviderManager
	io              *agentIO.IOManager
	memory          *memory.GraphDB
	indexer         *memory.Indexer
	tracker         *tracker.Tracker
	context         *RelevanceContext
	systemPrompt    string
	ctx             context.Context
	cancel          context.CancelFunc

	// 文档记忆（第二层）
	docStore *document.Store

	// 知识库
	knowledge *knowledge.Store

	// 人物特质与关系网
	social *social.SocialStore

	// 文本记忆（原始对话日志）
	textMem *text.Memory

	// 人格设定
	personality *agentPkg.Personality

	// 插件注册表（用于 plgreload）
	pluginReg *plugin.Registry
	pluginDir string

	// 定期心跳蒸馏
	distillInterval time.Duration

	// 三个独立心跳任务间隔
	archiveInterval time.Duration // 冷文档归档
	reviewInterval  time.Duration // 关系复审
	mergeInterval   time.Duration // 实体合并检测

	// 上下文裁剪：活跃上下文最大条数，超出按相关性裁剪
	maxContextSize int

	// 当前请求的输出通道（mutex 保护，process() 内独占）
	currentOutputChannel string

	// 阶段管道：插件消息流编辑
	stageHost    *StageHost
	eventBus     *events.Bus
	pluginHealth *pluginHealthTracker

	// 自循环输入通道：核心内部任务（记忆消歧、系统维护），不经过 IO 层
	selfInputCh chan string

	// 子任务异步执行
	childMu      sync.Mutex
	childNextID  int64
	childResults map[string]string

	// 高优先级打断通道：interceptLoop 注入，process() 在工具循环轮次间非阻塞读取
	interceptCh chan *agentIO.InputEvent

	// 进行中的 LLM 请求取消函数，interceptLoop 可调用以在请求中打断
	cancelLLM context.CancelFunc
	llmMu     sync.Mutex

	// 模型思考模式（thinking/reasoning）
	thinkingEnabled bool

	// 启动时间
	startTime time.Time

	// 当前轮次的非文本媒体数据（图片/音频），供 describe_image 等工具访问
	pendingMedia map[string]interface{}

	// 非文本输入处理配置
	inputCfg types.InputProcessingConfig

	// noMergeMarkets 记录被标记"禁止合并"的实体对，key="entityA||entityB"（字典序），
	// 每次 reorgGraph 扫描到对应实体对时计数减一，归零后自动移除。
	noMergeMarkers map[string]int
	noMergeMu      sync.Mutex

	// 词嵌入模型，用于实体语义相似度计算
	embedder *memory.StaticEmbedder
}

type AgentConfig struct {
	ID              types.AgentID
	SystemPrompt    string
	Provider        agentAPI.Provider
	ProviderManager *agentAPI.ProviderManager
	IO              *agentIO.IOManager
	Memory          *memory.GraphDB
	Indexer         *memory.Indexer
	Tracker         *tracker.Tracker

	DocStore           *document.Store
	Knowledge          *knowledge.Store
	SocialStore        *social.SocialStore
	TextMemory         *text.Memory
	Personality        *agentPkg.Personality
	PluginReg          *plugin.Registry
	PluginDir          string
	DistillInterval    time.Duration
	ArchiveInterval    time.Duration // 冷文档归档间隔（L2→L3），0 则使用 DistillInterval
	ReviewInterval     time.Duration // 关系复审间隔，0 则使用 DistillInterval
	MergeInterval      time.Duration // 实体合并检测间隔，0 则使用 DistillInterval
	MaxContextSize     int           // 活跃上下文最大条数，超出按相关性裁剪
	ContextSavePath    string        // 上下文持久化路径，空则不持久化
	EmbeddingModelPath string        // 预训练词嵌入模型路径（word2vec 文本格式），空则不使用
	Embedder           *memory.StaticEmbedder // 共享词嵌入实例；nil 时按 EmbeddingModelPath 自建
	StageHost          *StageHost
	EventBus           *events.Bus
	ThinkingEnabled    bool

	InputProcessing types.InputProcessingConfig // 非文本输入处理配置
}

func New(cfg AgentConfig) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.DistillInterval <= 0 {
		cfg.DistillInterval = 30 * time.Minute
	}
	if cfg.ArchiveInterval <= 0 {
		cfg.ArchiveInterval = cfg.DistillInterval
	}
	if cfg.ReviewInterval <= 0 {
		cfg.ReviewInterval = cfg.DistillInterval
	}
	if cfg.MergeInterval <= 0 {
		cfg.MergeInterval = cfg.DistillInterval
	}
	if cfg.MaxContextSize <= 0 {
		cfg.MaxContextSize = 30
	}

	embedder := cfg.Embedder
	if embedder == nil {
		embedder = memory.NewStaticEmbedder(strings.Split(cfg.EmbeddingModelPath, ",")...)
	}
	if cfg.DocStore != nil {
		cfg.DocStore.SetVectorizer(embedder)
		cfg.DocStore.ReindexWithVectorizer(embedder)
	}
	if cfg.Knowledge != nil {
		cfg.Knowledge.SetVectorizer(embedder)
		cfg.Knowledge.ReindexWithVectorizer(embedder)
	}

	rc := NewRelevanceContext(cfg.ContextSavePath, embedder)
	if cfg.StageHost != nil {
		rc.SetToolDefLookup(cfg.StageHost.ToolDef)
	}
	if cfg.IO != nil {
		rc.SetChannelDefLookup(cfg.IO.GetInputChannelDef)
	}

	return &Agent{
		id:              cfg.ID,
		startTime:       time.Now(),
		provider:        cfg.Provider,
		providerManager: cfg.ProviderManager,
		io:              cfg.IO,
		memory:          cfg.Memory,
		indexer:         cfg.Indexer,
		tracker:         cfg.Tracker,
		context:         rc,
		systemPrompt:    cfg.SystemPrompt,
		ctx:             ctx,
		cancel:          cancel,
		docStore:        cfg.DocStore,
		knowledge:       cfg.Knowledge,
		social:          cfg.SocialStore,
		textMem:         cfg.TextMemory,
		personality:     cfg.Personality,
		pluginReg:       cfg.PluginReg,
		pluginDir:       cfg.PluginDir,
		distillInterval: cfg.DistillInterval,
		archiveInterval: cfg.ArchiveInterval,
		reviewInterval:  cfg.ReviewInterval,
		mergeInterval:   cfg.MergeInterval,
		maxContextSize:  cfg.MaxContextSize,
		stageHost:       cfg.StageHost,
		eventBus:        cfg.EventBus,
		selfInputCh:     make(chan string, 64),
		childResults:    make(map[string]string),
		interceptCh:     make(chan *agentIO.InputEvent, 64),
		pluginHealth:    newPluginHealthTracker(),
		thinkingEnabled: cfg.ThinkingEnabled,
		inputCfg:        cfg.InputProcessing,
		embedder:        embedder,
		noMergeMarkers:  make(map[string]int),
	}
}

func (a *Agent) Start() {
	go a.eventLoop()
	go a.interceptLoop()
	go a.distillLoop()
	go a.archiveLoop()
	go a.mergeLoop()
	go a.reviewLoop()
	log.Printf("[agent] %s started, waiting for IO interrupts", a.id)
}

func (a *Agent) Stop() {
	a.cancel()
}

func (a *Agent) ID() types.AgentID { return a.id }

// SelfInputChan 返回自循环输入通道（只读，供内部测试验证）
func (a *Agent) SelfInputChan() <-chan string {
	return a.selfInputCh
}

// injectSelf 向自循环通道发送内部任务（记忆消歧、系统维护）
// 线程安全，不阻塞发送者（通道缓冲 64）
func (a *Agent) injectSelf(task string) {
	select {
	case a.selfInputCh <- task:
	default:
		log.Printf("[agent] self input channel full, dropping task: %s", truncateStr(task, 80))
	}
}

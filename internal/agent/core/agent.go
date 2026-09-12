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
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/media"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/social"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/text"
	"gitcode.com/JianFeeeee/HomeAgent/internal/memory/vector"
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

	// 媒体存储（内容寻址）：对话里出现的图片/音频按 sha256 落盘去重。
	// 它是记忆块的内容存储，不单独做生命周期管理：块的创建/迁移/删除
	// 由记忆系统本身决定。为 nil 时全部媒体接线静默跳过。
	mediaStore *media.Store

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

	// 自循环输入通道：核心内部任务（记忆消歧、系统维护、子 Agent 通知），
	// 不经过 IO 层。每条消息携带目标输出通道：
	//   "_consolidation_" = 记忆整理（无记忆路径，不写入上下文、不 emit 响应）
	//   其他 = 正常处理（写入上下文、emit 响应到该通道）
	selfInputCh chan selfInputMsg

	// 子任务异步执行
	childMu     sync.Mutex
	childNextID int64
	// childTasks 记录子任务状态：运行中 / 结果 / 是否已交付。
	//
	// 为什么保留结果而不是“读到即删”：完成通知会写进持久上下文
	// （formatMergedTimeline 每轮都重新注入），模型之后还会再查。若读到即删，
	// 第二次查询就得到“不存在或已过期”这个**永久失败信号**——模型据此认为
	// 任务未完成，会无限重试/汇报（实测单轮 35 次工具调用、持续 514 秒）。
	childTasks map[string]*childTaskState
	// childSeq 给完成的任务排个序，用于有界淘汰。
	childSeq int64

	// 高优先级打断通道：interceptLoop 注入，process() 在工具循环轮次间非阻塞读取
	interceptCh chan *agentIO.InputEvent

	// 进行中的 LLM 请求取消函数，interceptLoop 可调用以在请求中打断
	cancelLLM context.CancelFunc
	llmMu     sync.Mutex

	// 模型思考模式（thinking/reasoning）
	thinkingEnabled bool

	pendingToolBlocks []interface{} // 插件工具通过 SetToolBlocks 注入的多模态块（type agentAPI.ContentBlock），process.go 消费后追加到 tool message

	// 启动时间
	startTime time.Time

	// 当前轮次的非文本媒体数据（图片/音频），供 describe_image 等工具访问
	pendingMedia map[string]interface{}

	// pendingMediaDigests 累积本轮已落进 CAS 的媒体 digest。
	//
	// 需要缓存而不是当场挂到事件上：媒体在 process() 执行期间被捕获，
	// 而承载它的 ContextEvent 要等 process() 返回后才 Append——此刻还没有 owner_id。
	// 与 pendingMedia 同受 a.mu 保护。
	pendingMediaDigests []string

	// 当前输入是否为工具提醒/中断（以 system 角色注入，避免被当成用户消息）
	interruptInput bool

	// 非文本输入处理配置
	inputCfg types.InputProcessingConfig

	// noMergeMarkets 记录被标记"禁止合并"的实体对，key="entityA||entityB"（字典序），
	// 每次 reorgGraph 扫描到对应实体对时计数减一，归零后自动移除。
	noMergeMarkers map[string]int
	noMergeMu      sync.Mutex

	// 输入去重：防 webui/GUI 断线重连导致的消息重放
	// key=source+"|"+content, value=上次接收时间；短窗口内同内容丢弃
	lastInput   map[string]time.Time
	lastInputMu sync.Mutex

	// 词嵌入模型，用于实体语义相似度计算
	embedder *memory.StaticEmbedder

	// multimodalSpace 是统一多模态向量空间（可选）。实现可以是内嵌 ONNX，
	// 也可以是外部 API 客户端；两者共享同一套 L0/L2/L3 向量缓存与检索基础设施。
	multimodalSpace vector.MultimodalEmbedder

	// fusionCfg 控制文本路与视觉路的跨模态融合权重，可按模型实测结果配置。
	fusionCfg CrossModalFusionConfig

	// 技能索引提供者：由 skillmgr 插件实现，向 system prompt 注入轻量技能索引
	skillIndex SkillIndexProvider
}

// SkillIndexProvider 提供已加载技能的精炼索引，供 buildSystemPrompt 注入。
// 实现方（skillmgr）需线程安全并快速返回（每次 LLM 调用都会调用）。
type SkillIndexProvider interface {
	// SkillIndex 返回多行文本的技能索引，每行形如 "name vX.Y - description"；
	// 无技能时返回空串。
	SkillIndex() string
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
	MediaStore         *media.Store
	MultimodalSpace    vector.MultimodalEmbedder
	FusionCfg          CrossModalFusionConfig // 跨模态融合权重；零值用默认
	Personality        *agentPkg.Personality
	PluginReg          *plugin.Registry
	PluginDir          string
	DistillInterval    time.Duration
	ArchiveInterval    time.Duration          // 冷文档归档间隔（L2→L3），0 则使用 DistillInterval
	ReviewInterval     time.Duration          // 关系复审间隔，0 则使用 DistillInterval
	MergeInterval      time.Duration          // 实体合并检测间隔，0 则使用 DistillInterval
	MaxContextSize     int                    // 活跃上下文最大条数，超出按相关性裁剪
	ContextSavePath    string                 // 上下文持久化路径，空则不持久化
	EmbeddingModelPath string                 // 预训练词嵌入模型路径（word2vec 文本格式），空则不使用
	Embedder           *memory.StaticEmbedder // 共享词嵌入实例；nil 时按 EmbeddingModelPath 自建
	StageHost          *StageHost
	EventBus           *events.Bus
	ThinkingEnabled    bool

	SkillIndexProvider SkillIndexProvider

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
		// TF-IDF 内置为 fallback，无需外部注入
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
	// 注入稠密多模态向量空间（可选）：配置后文档检索、L0 相关性裁剪、
	// 跨模态检索全部共享同一向量空间，取代稀疏 fastText 语义路。
	// 未配置时退化到 TF-IDF/fastText 稀疏检索，保持既有行为。
	if cfg.MultimodalSpace != nil && cfg.MultimodalSpace.Loaded() {
		rc.SetDenseSpace(cfg.MultimodalSpace)
		if cfg.DocStore != nil {
			cfg.DocStore.SetDenseSpace(cfg.MultimodalSpace)
			cfg.DocStore.BuildDenseIndex(cfg.MultimodalSpace)
		}
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
		mediaStore:      cfg.MediaStore,
		personality:     cfg.Personality,
		pluginReg:       cfg.PluginReg,
		pluginDir:       cfg.PluginDir,
		distillInterval: cfg.DistillInterval,
		archiveInterval: cfg.ArchiveInterval,
		reviewInterval:  cfg.ReviewInterval,
		mergeInterval:   cfg.MergeInterval,
		maxContextSize:  cfg.MaxContextSize,
		stageHost:       cfg.StageHost,
		skillIndex:      cfg.SkillIndexProvider,
		eventBus:        cfg.EventBus,
		selfInputCh:     make(chan selfInputMsg, 64),
		childTasks:      make(map[string]*childTaskState),
		interceptCh:     make(chan *agentIO.InputEvent, 64),
		pluginHealth:    newPluginHealthTracker(),
		thinkingEnabled: cfg.ThinkingEnabled,
		inputCfg:        cfg.InputProcessing,
		embedder:        embedder,
		multimodalSpace: cfg.MultimodalSpace,
		fusionCfg:       cfg.FusionCfg,
		noMergeMarkers:  make(map[string]int),
		lastInput:       make(map[string]time.Time),
	}
}

// SetSkillIndexProvider 注入技能索引提供者（skillmgr 插件加载后由 main 接线）。
func (a *Agent) SetSkillIndexProvider(p SkillIndexProvider) { a.skillIndex = p }

func (a *Agent) Start() {
	go a.eventLoop()
	go a.interceptLoop()
	go a.distillLoop()
	go a.archiveLoop()
	go a.mergeLoop()
	go a.reviewLoop()
	a.reembedStaleMedia()
	a.migrateLegacyGraphMedia()
	log.Printf("[agent] %s started, waiting for IO interrupts", a.id)
}

func (a *Agent) Stop() {
	a.cancel()
}

func (a *Agent) ID() types.AgentID { return a.id }

// isDuplicateInput 判断是否为短窗口内的重复输入（防 webui/GUI 断线重连消息重放）。
// key=source+"|"+content；窗口内重复返回 true 并刷新时间戳（持续轰炸时保持拦截）。
const duplicateInputWindow = 10 * time.Second

func (a *Agent) isDuplicateInput(source, content string) bool {
	a.lastInputMu.Lock()
	defer a.lastInputMu.Unlock()
	now := time.Now()
	key := source + "|" + content
	if last, ok := a.lastInput[key]; ok && now.Sub(last) < duplicateInputWindow {
		a.lastInput[key] = now
		return true
	}
	a.lastInput[key] = now
	// 顺带清理过期项，防止 map 无限增长
	for k, t := range a.lastInput {
		if now.Sub(t) > duplicateInputWindow {
			delete(a.lastInput, k)
		}
	}
	return false
}

// IsDuplicateInput 导出包装，供测试验证去重行为。
func (a *Agent) IsDuplicateInput(source, content string) bool {
	return a.isDuplicateInput(source, content)
}

// SelfInputChan 返回自循环输入通道（只读，供内部测试验证）
func (a *Agent) SelfInputChan() <-chan selfInputMsg {
	return a.selfInputCh
}

// injectSelf 向自循环通道发送记忆整理类内部任务（无记忆路径）。
// 线程安全，不阻塞发送者（通道缓冲 64）。
func (a *Agent) injectSelf(task string) {
	a.injectSelfChannel(selfInputMsg{
		text:    task,
		channel: channelConsolidation,
	})
}

// injectSelfChannel 向自循环通道发送一条带目标通道标志的消息。
// channel == "_consolidation_" 走无记忆整理路径；其他值走正常处理路径。
func (a *Agent) injectSelfChannel(msg selfInputMsg) {
	select {
	case a.selfInputCh <- msg:
	default:
		log.Printf("[agent] self input channel full, dropping task: %s", truncateStr(msg.text, 80))
	}
}

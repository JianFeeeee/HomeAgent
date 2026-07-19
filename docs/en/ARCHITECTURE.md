**中文** | [English](../en/ARCHITECTURE.md)

# HomeAgent Architecture

The kernel performs zero IO; all external interaction comes from plugins.

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Message Processing Flow

### Full Pipeline

```
External input (via plugin InjectInput)
        │
        ▼
eventLoop() → processTextInput()
        │
        ├── on_input stage          Plugins can intercept/rewrite/short-circuit
        ├── Context.Append          Record to context window
        ├── Context.Prune           Low-relevance events archived to Document
        ├── buildMemoryContext()    Indexer recall → GraphDB BFS traversal
        │
        ├── pre_action stage        Plugins can inject system messages
        │
        ├── [Tool Loop] process()
        │   ├── buildSystemPrompt   Persona + Memory + Knowledge + Context
        │   ├── buildToolDefs       Built-in tools + Plugin tools
        │   ├── provider.Chat()     LLM call
        │   ├── post_action stage   Plugins see LLM output + tool list
        │   ├── Has tools?
        │   │   ├── before_toolcall Plugins can reject/modify params
        │   │   ├── executeToolCall Route to plugin/built-in
        │   │   ├── after_toolcall  Plugins can modify results
        │   │   └── → back to post_action
        │   └── No tools → exit loop
        │
        ├── Context.Append(response)
        ├── before_output stage     Plugins can modify final text
        ├── emitResponse()          Send via output_send
        └── after_output stage      Read-only, cleanup
```

Code: `internal/agent/core/agent.go` — `process()` is the main tool loop

### 7 Stage Hooks

| Stage | Trigger | Plugin Capabilities |
|-------|---------|---------------------|
| `on_input` | Message arrives at Agent, zero processing | Blacklist/rate-limit/short-circuit reply |
| `pre_action` | Context ready, before LLM call | Inject external data into context |
| `post_action` | LLM returns text + tool list | Sensitive word filter/forced redirect |
| `before_toolcall` | Before single tool execution | Audit/reject/modify params |
| `after_toolcall` | After single tool execution | Desensitize/sort results |
| `before_output` | Final text ready, before sending | Format adaptation |
| `after_output` | Already sent | Statistics/logging |

Code: `internal/agent/core/stages.go` — `StageHost` orchestration

### Loop Rules

`post_action → [before_toolcall → execute → after_toolcall] → post_action` forms the inner loop.
Exit conditions: LLM has no tool calls / all rejected / exceeded limit.

### Short-Circuit Rules

Setting `ctx.Response` at any stage jumps to `after_output`.

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Three-Layer Memory

### Memory Flow

```
① Context (Working Window)
   RelevanceContext — In-memory events[] + JSON persistence
   Append: Each input, CleanTemplateText → three-branch vector(textForVector)
           agent→Response, user→Input, cold_storage→Input+Response
           StaticEmbedder pretrained word embedding / TF-IDF fallback
   Prune:  StaticEmbedder CosineSimilarity, keep topK + last 10
       ├── Keep → timeline → chronologically sorted → system prompt
       └── Low score → Document layer archive (original timestamp)
   Save: 5s debounce write to disk

         ↓ Prune archive                       ↑ LLM active recall

② Document (File Memory)
   DocStore — JSON files + TF-IDF InvertedIndex
   Write: Prune archive / doc_commit / Graph snapshot (syncGraphToDocs)
   Read:
       ├── Auto-inject: Query(input, top3) → similarity summary → [Related Memory Docs] → system prompt (read-only)
       └── LLM active:  doc_query → Consume(read and delete)
                         → context.Append{Timestamp: d.CreatedAt, Source: "cold_storage"} per doc
                         → Docs written to context timeline with original timestamps, deleted from docStore
   Cold: FindColdDocs(72h, ≤2 accesses) → docToTriples → Graph

         ↓ Cold doc distillation               ↑ Auto recall

③ Graph (Graph Database)
   SQLite — entities + relations tables
   Write: memory_commit / cold doc distillation / Pipeline rule distillation / memory_merge
   Read:
       ├── Auto recall: Indexer.BuildContext(input)
       │     → CleanTemplateText → vector entity search + jieba keywords → SQLite LIKE + BFS depth=2
       │     → [Memory Index] → system prompt
       └── LLM active: memory_recall / memory_merge / memory_purge / memory_edit / memory_delete_entity
   Social: person_query / set_trait / relate (wraps GraphDB)

④ Distillation Pipeline (30min heartbeat)
   distillContext → window > 2×maxSize → force Prune
   syncGraphToDocs → Graph snapshot to Document (cross-layer searchable)
   reorgGraph:
       Step1: indexer.Sync — rebuild entity vector index
       Step2: docStore.Reindex — rebuild document vector index
       Step3: Cold docs → docToTriples → GraphDB.Commit
       Step4: Entity similarity (Bigram Jaccard > 0.75) → consolidation → LLM decides merge
       Step5: evaluateGraphQuality → LLM decides keep/delete

⑤ Pipeline Rule Distiller (every heartbeat)
   distillOnce → regex match personal info:
     我叫X / 我住在X / 我喜欢X / 我X岁 / 我的工作是X
     → triples → GraphDB.Commit
```

### Vectorization: Pretrained Word Embedding + TF-IDF Fallback

All vectorization unified under `StaticEmbedder` (`internal/memory/static_embedder.go`):

**Primary Strategy — Pretrained Word Embedding (aligned 300d)**
- Model sources: ConceptNet Numberbatch (77-language aligned) / fastText Chinese / fastText English
- Configured via `core.agent.embedding_model_path` (comma-separated multi-model)
- Path containing `numberbatch` → auto-download ConceptNet; `cc.zh.` → fastText Chinese; `cc.en.` → fastText English
- Falls back to ConceptNet by default if no match
- **Pre-processing**: `CleanTemplateText` strips QQ tool-call templates and timestamp noise
- **Three-branch vector source**: agent→Response, user→Input, cold_storage→Input+Response
- **TF-IDF fallback**: auto-fallback to bag-of-words TF-IDF if model download fails or not configured

| Location | File | Purpose | Algorithm |
|----------|------|---------|-----------|
| Context Prune | `context.go:155` | Trim low-relevance context events | VectorizeClean → CosineSimilarity(queryVec, evt.Vector) |
| DocStore Query | `document.go:206` | Recall from document memory | TF-IDF Vectorize → vec.Search |
| Indexer Entity Search | `indexer.go:96+111` | Recall from Graph | vector entity search + jieba keywords → SQLite LIKE + BFS |
| Entity Similarity Detection | `agent.go` | Detect similar entities in Graph | Bigram Jaccard (>0.75 → consolidation) |

### Context Layer

`internal/agent/core/context.go` — `RelevanceContext`
- Maintains recent event list, writes JSON on each Append/Prune to prevent data loss
- Pre-vectorization pipeline runs through `CleanTemplateText` to remove template noise
- Three-branch `textForVector`: agent events → Response, user events → Input, cold_storage → Input+Response
- Pretrained word embedding `StaticEmbedder` → CosineSimilarity, auto-fallback to TF-IDF if unavailable
- Protects last 10 events from eviction; excess candidates are sorted by relevance and archived to document memory
- Archived events retain original timestamps; on `doc_query` recall they re-insert into the context timeline at their original position

### Document Layer

`internal/memory/document/document.go` — `Store`
- Consume-on-read mode: deleted after `doc_query` retrieval
- Dual recall: char-bigram TF-IDF vector search + jieba keyword extraction

### Graph Layer

`internal/memory/graph.go` — `GraphDB`
- SQLite WAL mode, two tables (driver: mattn/go-sqlite3, CGo)
- `Commit(triples)` — UPSERT entities + INSERT relations
- `Recall(keywords, depth)` — Keyword LIKE search + BFS traversal

### Memory Tools (LLM-callable)

| Tool | Purpose |
|------|---------|
| `memory_recall` | Recall from Graph |
| `memory_commit` | Write triples to Graph |
| `memory_merge` | Merge two entity nodes |
| `memory_purge` | Delete entity node |
| `memory_edit` | Edit existing entity/relation |
| `memory_delete_entity` | Delete entity and all its relations |
| `memory_introspect` | View memory statistics |
| `doc_query` | Search from Document |
| `doc_commit` | Write to Document |

### Other Memory Layers

- **Social** (`internal/memory/social/social.go`) — Persona traits and relationship network, wraps GraphDB entity types
- **Text Memory** (`internal/memory/text/text.go`) — Raw conversation JSONL logs, rotation strategy
- **Memory Indexer** (`internal/memory/indexer.go`) — Entity vectorization + jieba keyword extraction, auto-inject into system prompt

### Distillation Pipeline

`internal/memory/pipeline/pipeline.go`
- 10-minute tick, 7-day retention
- Rule-based triple extraction (name / location / likes / age / job patterns)
- Writes to GraphDB

### Context Pruning

```
Heartbeat 30min:
  ├── distillContext()        — Distill current context
  ├── syncGraphToDocs()       — Graph → Document sync
  └── reorgGraph()
      ├── Indexer.Sync()
      ├── DocStore.Reindex()
      ├── Cold docs → Graph
      └── Entity conflicts → enqueueConsolidationTask()
                                │
                          selfInputCh → LLM decides merge/skip
```

Entity conflict detection heuristic (bigram Jaccard > 0.75), routed through `selfInputCh` internal channel, LLM makes the final merge decision.

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Knowledge Base

`internal/knowledge/knowledge.go`
- File directory `knowledge/<name>/content.md`
- Independent TF-IDF index, separate from memory system
- `knowledge_search` / `knowledge_create` / `knowledge_list`

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Provider & Lua Adapter Layer

```
Agent
  │
  ▼
Provider Interface (Name / Chat / ChatStream)
  │
  ├── OpenAIProvider   — Standard OpenAI API
  ├── OllamaProvider   — Local Ollama
  └── LuaAdaptedProvider (primary)
      ├── Serialize CompletionRequest → JSON
      ├── adapter.transform_request() → API format
      ├── HTTP request + adapter.headers
      ├── adapter.transform_response() → unified format
      └── Deserialize
```

Code: `internal/agent/api/provider.go`

ProviderManager manages multiple sources, fallback in registration order. Lua adapters at `internal/lua/adapters/`, each `.lua` script defines `transform_request` / `transform_response` / `transform_stream_chunk`.

VM built-ins: `json.encode` / `json.decode` / `log` / `http_get` / `http_post`.

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Plugin System

### Four Loading Methods

| Method | Registration Mechanism | Compilation | Usage |
|--------|----------------------|-------------|-------|
| Built-in | `init()` → `RegisterFactory` | `internal/plugins/` compiled into kernel | webui/cli/timer/mcp etc. |
| External `.so` | C ABI dynamic loading | `-buildmode=c-shared` + bridge | qq/files/web/memo etc. |
| Lua script plugin | Parse `main.lua` to register tools | No compilation, hot-reload | luaplugintest/testlua etc. |
| SKILL plugin | Parse `SKILL.md` | Markdown definition | OpenClaw compatible |

Built-in plugin registration: `internal/plugins/all.go` blank imports → each plugin `init()` → `Registry.Load()` scans directory to match factory.
External plugin loading: `internal/plugin/dynamic.go` → copy to SHA256 temp path (bypass `plugin.Open` path cache) → `Open` + `Lookup("NewPlugin")`.
Lua script plugin loading: `internal/lua/` → parse `main.lua` via Lua VM, call `start()` to register tools.

### PluginSDK Four Channels

```
Plugin ──→ Kernel

RegisterTool(name, fn)   ──→  buildToolDefs() / executeToolCall()
RegisterStage(stage, fn, scope...)  ──→  runStage() called at corresponding phase (scope: global / own-tools-only)
Subscribe(event, fn)      ──→  Publish() notify all subscribers
RegisterOutputChannel(name, caps, desc, handler) ──→ output_send__{name} tool generation
```

`internal/sdk/` bridges external SDK interface to kernel, defines complete PluginSDK:

```go
sdk.RegisterTool(name, def, handler)
sdk.RegisterStage(stage, handler, scope...)
sdk.Publish(event)
sdk.InjectInput(source, channel, payload)
sdk.InjectInterrupt(source, channel, payload)
sdk.Memory().Recall/Commit
sdk.Knowledge().Search/Create
sdk.Settings().Get/Set/List
sdk.RegisterOutputChannel("qq", sdk.CapText|sdk.CapAudio|sdk.CapImage, "QQ channel, see output_send__qq_help for details", handler)
```

### Plugin Interface

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Output Channel System

Each output channel generates two tools:

| Tool | Type | Purpose |
|------|------|---------|
| `output_send__{name}` | function | Accepts `payload` (content), `meta` (JSON routing metadata), `type` (enum) — routed to plugin handler |
| `output_send__{name}_help` | function | Returns the channel's meta format and type enum documentation |

Capability flags:

| Flag | Value | Meaning |
|------|-------|---------|
| CapText | 1 | Plain text |
| CapFile | 2 | File |
| CapImage | 4 | Image |
| CapAudio | 8 | Audio |
| CapStructured | 16 | Structured data |

System prompt injection: output gate rules, multi-call support, long message splitting.
Child agent permission: `output_send__` prefix tools are allowed.

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## EventAgentLLMChain Event

- Event type `agent_llm_chain` emitted after each LLM turn
- Contains the full LLM response (text + tool calls + reasoning)
- WebUI subscribes to this event via SSE for real-time display
- Plugins can subscribe via EventSubscriber (read-only for external plugins)

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Restricted External Plugin API

Layered architecture: internal plugins get full PluginSDK, external plugins get restricted SDK.

| API | Internal Plugin | External Plugin |
|-----|-----------------|-----------------|
| SocialAPI | Full read/write | Read-only (GetPerson / GetTrait / GetRelations / GetNetwork / ListPersons) |
| EventSubscriber | Subscribe + Publish | Subscribe-only (no Publish capability) |

Extended fields:
- Triple extensions: Confidence, SubjectType, ObjectType
- Relation extension: Confidence

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Interrupt Mechanism

```
interceptLoop (goroutine)
  ├── InputInterruptChan() ← Timer/message notifications
  ├── (a) cancelLLM() → Cancel Provider HTTP request
  ├── (b) interceptCh → process() pre-loop read [interrupt message]
  └── (c) InjectInput() → Trigger new processing when idle
```

Three delivery paths:

| Path | Effect | Timing |
|------|--------|--------|
| cancelLLM | Cancel current HTTP request | On context.Canceled |
| interceptCh | Insert `[interrupt message]` in process() | Before each LLM call |
| InjectInput | Trigger new processing when eventLoop is idle | No ongoing request |

Code: `internal/agent/core/agent.go` — `interceptLoop` / `drainInterrupt`

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Configuration System

`internal/config/registry.go` — ConfigRegistry

- SQLite storage, `config` table + `config_<plugin>` independent tables
- Namespaces: `core.*` / `plugin.<name>.*`
- `RegisterDefault` inserts ~80 default keys (seeds for 8 LLM sources)
- WebUI settings page `/api/v1/settings` for read/write

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Code Structure

```
cmd/homed/main.go          — Entry: assembles all subsystems
cmd/waiter/main.go         — CLI client (Unix socket)
internal/
├── agent/
│   ├── core/              — Agent core (eventLoop/process/stages/context)
│   │   └── plugin_health.go — Plugin health monitoring and auto-restart
│   ├── api/               — Provider interface + LuaAdaptedProvider
│   ├── io/                — IOManager (queue/interrupt/output)
│   └── personal.go        — Persona loading
├── plugin/
│   ├── registry.go        — Registry + lifecycle
│   ├── dynamic.go         — .so dynamic loader
│   └── manifest.go        — plugin.json metadata
├── plugins/               — Built-in plugin implementations
│   ├── all.go             — Blank imports
│   ├── webui/             — HTTP server + embedded SPA
│   ├── cli/               — Unix socket CLI
│   ├── timer/             — Timer
│   ├── cmd/               — Command execution
│   ├── mcp/               — MCP protocol
│   ├── openclaw/          — OpenClaw compatible
│   ├── agentcli/          — PTY terminal
│   ├── healthcheck/       — Health check
│   └── pluginmgr/         — Plugin manager
├── sdk/                   — PluginSDK definitions
│   ├── plugin.go          — Plugin interface + PluginSDK
│   ├── memory.go          — MemoryAPI
│   ├── knowledge.go       — KnowledgeAPI
│   ├── settings.go        — SettingsAPI
│   └── llm.go             — LLMAPI
├── memory/
│   ├── graph.go           — SQLite graph database
│   ├── indexer.go         — Graph → vector index
│   ├── vector/store.go    — TF-IDF vector engine
│   ├── document/document.go — Document memory
│   ├── text/text.go       — Text logs
│   └── pipeline/          — Distiller
├── knowledge/knowledge.go — Knowledge base
├── lua/
│   ├── vm.go              — Lua VM (json/log/http)
│   └── adapters/          — 8 LLM adapter scripts
├── config/registry.go     — SQLite config center
├── events/bus.go          — Event bus
├── tracker/               — OverlayFS change tracking
├── supervisor/            — Daemon management
├── skill/                 — Skill plugin management
│   └── manager.go         — Skill loading/matching
└── meta/                  — Meta information
    └── meta.go            — Agent metadata
```

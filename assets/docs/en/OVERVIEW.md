**中文** | [English](../en/OVERVIEW.md)

# HomeAgent — Project Overview

## What Is This

HomeAgent is a continuously-running personal intelligent Agent framework.

Core architecture: a long-running kernel process (`homed`) that connects to various IO channels (QQ, Web, CLI, etc.) through a plugin system. The kernel handles LLM orchestration, memory management, and knowledge retrieval; plugins handle all external IO — sending/receiving messages, file operations, web search, etc.

### Design Highlights

**Separation of Core Domain and Application Domain** — The kernel (core domain) performs no IO operations; all IO capabilities belong to plugins (application domain). The boundary is defined through PluginSDK:
- Plugins register tools (Tool) with the kernel for LLM invocation
- Plugins hook into the processing pipeline (Stage) to intercept/rewrite message flow at various phases
- Plugins subscribe/publish events (Event) for loosely-coupled communication
- Plugins queue or interrupt input delivery through IO API

The significance lies in clear responsibility boundaries: the kernel focuses on orchestration and memory management, while plugins handle IO implementation — the two are not coupled.

**Three-Layer Memory Architecture** — Manages information retention across long agent runtimes through a tiered storage strategy:
- **Context Layer**: In-memory pretrained word embedding scored event window (StaticEmbedder word vectors → CosineSimilarity, TF-IDF fallback), maintains recent context in real-time, low-relevance events automatically sink to the next layer
- **Document Layer**: JSON files + TF-IDF vector-indexed temporary memory, supports explicit submission and implicit archival, cold data distills to Graph
- **Graph Layer**: SQLite graph database, persists entities and relations, BFS traversal recall, distillation pipeline extracts triples from conversations

Three progressive layers — context, cold archive, long-term graph memory — form an information decay and consolidation pipeline from short-term to persistent storage.

## What It Actually Does

Code is in the project root, implemented in Go.

**Kernel** (`internal/agent/core/`):
- `eventloop.go` — Message loop (`eventLoop`), queuing input from the IO layer
- `process.go` / `stages.go` — Processing pipeline: memory recall → persona injection → LLM call → tool execution → output delivery, 7 stage hooks
- `toolcall.go` — Tool scheduling and execution
- `context.go` — Context management (pretrained word embedding scoring StaticEmbedder → CosineSimilarity, TF-IDF fallback), automatic pruning of low-relevance events
- LLM calls abstracted through Provider interface, supports 8 LLM sources with automatic fallback

**Memory System** (`internal/memory/`):
- **GraphDB** (`graph.go`) — SQLite, entities + relations tables, BFS traversal
- **Document Store** (`document/document.go`) — Temporary memory, JSON files + TF-IDF vector index, consume-on-read
- **Text Memory** (`text/text.go`) — Raw conversation logs, JSONL file rotation
- **Social Store** (`social/social.go`) — Persona traits + relationship network, wraps GraphDB
- **Memory Indexer** (`indexer.go`) — Auto-vectorizes GraphDB entities, recalls and injects into system prompt on user input

**Knowledge Base** (`internal/knowledge/knowledge.go`):
- File system directory `knowledge/<name>/content.md`
- TF-IDF vector search, independent index instance from the memory system
- LLM operates via three tools: `knowledge_search` / `knowledge_create` / `knowledge_list`

**Plugin System** (`internal/plugin/`):
- Built-in plugins: Go `init()` self-registration, compiled into kernel
- External plugins: Go `-buildmode=c-shared` compiled to `.so`, dynamically loaded via C ABI bridge; also supports Lua script plugins
- PluginSDK (`internal/sdk/`) defines four channels: RegisterTool / RegisterStage / Subscribe / RegisterOutputChannel
- 7 stage hooks: on_input → pre_action → post_action → before_toolcall → after_toolcall → before_output → after_output

**LLM Provider** (`internal/agent/api/provider.go`):
- Provider interface: Name / Chat / ChatStream
- Three implementations: OpenAIProvider (standard OpenAI API), OllamaProvider (local), LuaAdaptedProvider (Lua adapter)
- LuaAdapter located at `internal/lua/adapters/`, each LLM source has a corresponding `.lua` script
- 8 built-in adapters: deepseek / openai / anthropic / gemini / mistral / groq / github / ollama

**WebUI** (`internal/plugins/webui/`):
- Embedded SPA dashboard (`dashboard.html` packaged via `//go:embed`)
- REST API: status query, configuration management, memory operations, knowledge management, plugin management
- OpenAI API-compatible `/v1/chat/completions` endpoint
- SSE event stream `/api/v1/chat/events`

**ClawHub Adapter** (`internal/plugins/clawhubadapter/`):
- Unified loader for OC plugins (Node.js), Python sidecar, JS sidecar, and SKILL plugins
- RegistryDispatcher pattern: routes registration notifications to Tool/Provider/Channel/Stage registries
- ClawHub marketplace search and install: `clawhubadapter_search` / `clawhubadapter_npm_install`
- 9 provider types mapped to LLM-accessible tools (image generation, web search, speech, etc.)
- OC channels auto-registered as IO devices with text/file/image/audio capability flags


## Project Status

Core functionality is operational. Plugin system and SDK are ready for independent external plugin development.

- Built-in plugins: webui / cli / timer / cmd / mcp / agentcli / healthcheck / pluginmgr / clawhubadapter / files / cfgmgr
- External plugin examples ([homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repo `example/`, both Go and Lua types): qq / files / web / memo / bili / editdoc / a2a / ocr / sanitizer / luaplugintest / testlua
- Distribution: `.hmap` plugin package format, installable via WebUI

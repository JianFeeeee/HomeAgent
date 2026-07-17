**中文** | [English](../zh/OVERVIEW.md)

# HomeAgent — Project Overview

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## What Is This

HomeAgent is a continuously-running personal intelligent Agent framework.

Core architecture: a long-running kernel process (`homed`) that connects to various IO channels (QQ, Web, CLI, etc.) through a plugin system. The kernel handles LLM orchestration, memory management, and knowledge retrieval; plugins handle all external IO — sending/receiving messages, file operations, web search, etc.

### Key Innovations

**Separation of Core Domain and Application Domain** — This is the first Agent framework to explicitly make this distinction. The kernel (core domain) performs zero IO; all IO capabilities belong to plugins (application domain). The boundary is clearly defined through PluginSDK:
- Plugins register tools (Tool) with the kernel for LLM invocation
- Plugins hook into the processing pipeline (Stage) to intercept/rewrite message flow at various phases
- Plugins subscribe/publish events (Event) for loosely-coupled communication
- Plugins queue or interrupt input delivery through IO API

The significance: the kernel stays pure (zero IO, only orchestration and memory), plugins stay flexible (each does its job, hot-loadable), with no cross-contamination.

**Three-Layer Memory Architecture** — Solves the memory decay problem for long-running agents:
- **Context Layer**: In-memory local word embedding scored event window (jieba + TF-IDF + PMI → CosineSimilarity), maintains recent context in real-time, low-relevance events automatically sink to the next layer
- **Document Layer**: JSON files + TF-IDF vector-indexed temporary memory, supports explicit submission and implicit archival, cold data distills to Graph
- **Graph Layer**: SQLite graph database, persists entities and relations, BFS traversal recall, distillation pipeline extracts triples from conversations

Three progressive layers: context → cold archive → long-term graph memory, ensuring the agent doesn't degrade over time.

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## What It Actually Does

Code is in the project root, implemented in Go.

**Kernel** (`internal/agent/core/agent.go`):
- Maintains a message loop (`eventLoop`), queuing input from the IO layer
- Each input goes through the full processing pipeline: memory recall → persona injection → LLM call → tool execution → output delivery
- LLM calls abstracted through Provider interface, supports 8 LLM sources with automatic fallback
- Context management (`context.go`) based on word embedding scoring (LocalWordEmbedder → CosineSimilarity), automatic pruning of low-relevance events

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

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Project Status

Core functionality is operational. Plugin system and SDK are ready for independent external plugin development.

- Built-in plugins: webui / cli / timer / cmd / mcp / agentcli / healthcheck / pluginmgr / openclaw / files
- External plugin examples ([homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repo `example/`, both Go and Lua types): qq / files / web / memo / bili / editdoc / a2a / ocr / sanitizer / luaplugintest / testlua
- Distribution: `.hmap` plugin package format, installable via WebUI

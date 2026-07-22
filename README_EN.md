<img src="branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

# HomeAgent

> **中文**: [README.md](./README.md)

The first Agent framework to propose **separation of core domain and application domain**. The kernel performs zero IO — all external interaction is handled by plugins: WebUI, QQ, CLI, file operations, web search, memos — everything is a plugin, the kernel doesn't touch any IO.

Combined with a **three-layer memory architecture** (Context → Document → Graph), it achieves stable long-running single-conversation operation without memory decay.

```go
homed (kernel, zero IO) ← PluginSDK → plugins (all IO capabilities)
```

## Key Innovations

**Separation of Core Domain and Application Domain** — The kernel only handles LLM orchestration, memory management, and knowledge retrieval; all IO capabilities (sending/receiving messages, reading/writing files, network requests, hardware interaction) are implemented by plugins. Plugins can be hot-loaded, independently developed, and independently released. This is not a microservice split of an RPC framework, but a domain-level separation in Agent framework design.

**Three-Layer Memory Architecture** — Solves the memory decay problem for long-running agents:
- **Context Layer**: Pretrained word embedding / TF-IDF fallback relevance-scored event window, protects last 10 entries, maintains topK context entries
- **Document Layer**: Temporary memory with automatic cold data sinking, also supports user-initiated submissions
- **Graph Layer**: SQLite graph database, persists entity relationships and semantic memory, supports distillation pipelines to extract triples from conversations

## Architecture Diagrams

### 1. Message Processing Sequence

```mermaid
sequenceDiagram
    participant U as User/Plugin
    participant IO as IOManager
    participant EV as eventLoop
    participant CTX as RelevanceContext
    participant LLM as LLM+Tool Loop
    participant ST as StageHost
    participant MEM as Three-Layer Memory

    U->>IO: InjectInput(type, payload)
    IO->>EV: inputCh
    rect lavender
        Note over EV: processTextInput
        EV->>ST: StageOnInput  Plugin can rewrite/short-circuit
        EV->>CTX: Prune(input,topK)  StaticEmbedder/TF-IDF cosine pruning
        CTX->>MEM: Low-score events archived to Document (original timestamp)
        EV->>CTX: Append(input)  CleanTemplateText→three-branch vector→5s write
    end
    rect lightgreen
        Note over EV,LLM: process()
        EV->>MEM: buildMemoryContext  Indexer recalls from Graph (vector+jieba→BFS depth=2)
        EV->>MEM: buildSystemPrompt  DocQuery summary+Graph memory index+Persona+Skills
        EV->>ST: StagePreAction  Plugin can pre-intercept
        loop Tool loop
            LLM->>LLM: drainInterrupts
            LLM->>LLM: LLM Chat
            LLM->>ST: StagePostAction  Plugin can modify/short-circuit
            alt No tool call
                LLM-->>EV: Returns response
            else
                loop Each tool
                    ST->>ST: StageBeforeToolcall  Plugin can reject
                    LLM->>LLM: executeToolCall
                    ST->>ST: StageAfterToolcall
                end
            end
        end
    end
    rect lightpink
        Note over EV: emitResponse
        CTX->>CTX: Append(response)
        ST->>ST: StageBeforeOutput  Plugin can rewrite
        EV-->>U: ResponseCh CLI sync
        EV-->>EV: Event bus WebUI SSE
        ST->>ST: StageAfterOutput  Read-only
        EV->>MEM: emitMemoryCandidate
    end
```

### 2. Stage Pipeline

```mermaid
flowchart LR
    S1[① on_input] --> S2[② pre_action]
    S2 --> S3[③ post_action]
    S3 --> Q{Has tool?}
    Q -->|Yes| S4[④ before_toolcall]
    S4 --> T[executeToolCall]
    T --> S5[⑤ after_toolcall]
    S5 --> S3
    Q -->|No| S6[⑥ before_output]
    S6 --> S7[⑦ after_output]
    style S1 fill:#e1f5fe
    style S3 fill:#fff3e0
    style S6 fill:#e8f5e9
```

### 3. Three-Layer Memory

```mermaid
flowchart TB
    subgraph C[① Context Working Window]
        RC[RelevanceContext]
        A[Append] -->|CleanTemplateText→three-branch vector| RC
        P[Prune StaticEmbedder/TF-IDF Cosine] -->|Low score original timestamp| D
        P -->|Keep| TL[timeline→chronological→system prompt]
    end
    subgraph D[② Document File Memory]
        DS[DocStore JSON+TF-IDF]
        Q1[Query summary auto-inject] -->|[Related Memory Docs]| SP
        Q2[doc_query LLM active recall] -->|Consume+delete source| DS
        Q2 -->|Original timestamp write to context| RC
        CD[FindColdDocs 72h] -->|docToTriples| G
    end
    subgraph G[③ Graph Database]
        DB[(SQLite)]
        IDX[Indexer vector+jieba→BFS depth=2] -->|[Memory Index]| SP
        MEM[memory_recall/commit/merge/purge/edit]
        SOC[person_query/set_trait]
    end
    subgraph H[④ Heartbeat Distillation]
        REORG -->|Step3 Cold docs| CD
        REORG -->|Step4 Bigram Jaccard| CONS[consolidation]
        PIPE[Pipeline regex] -->|Name/Address/Likes/Age/Job| DB
    end
    SP[System Prompt] -->|Sequential assembly| LLM
    LLM[LLM] -->|doc_query| Q2
    LLM -->|memory_recall| MEM
```

See [`docs/en/ARCHITECTURE.md`](docs/en/ARCHITECTURE.md) for details.

## Web Mascot

<div align="center">
  <img src="branding/mascot-xiaozhai.webp" alt="HomeAgent Web Mascot Xiaozhai" width="200">
  <p><strong>Xiaozhai</strong> — HomeAgent Web Mascot</p>
</div>

## Quick Start

```bash
make build build-cli
./build/homed -data /tmp/ha
```

```bash
# Interactive mode
./build/waiter

# Or single message
echo "Hello, remember that I like coffee" | ./build/waiter
```

API keys are configured via WebUI `http://localhost:8080` settings page, persisted in SQLite.

## Code Structure

```
cmd/homed/          Daemon entry, assembles all subsystems
cmd/waiter/         CLI client (Unix socket)
internal/
├── agent/core/     Agent core: event loop, LLM tool loop, 7-stage pipeline
├── agent/api/      LLM Provider + 8 Lua adapters
├── memory/         Three-layer memory: Graph(SQLite) / Document(JSON+TF-IDF) / Text(JSONL) + StaticEmbedder(pretrained word embedding/TF-IDF fallback) + CleanTemplateText(de-template)
├── knowledge/      Knowledge base (filesystem + TF-IDF)
├── plugin/         Plugin registry + .so/.dll dynamic loader
├── plugins/        11 built-in plugins (webui/cli/timer/cmd/mcp/clawhubadapter/agentcli/healthcheck/pluginmgr/files/cfgmgr)
├── sdk/            PluginSDK (Tool/Stage/Event three channels)
├── config/         SQLite config center
├── events/         Event bus
└── internal/lua/adapters/   8 LLM protocol adapter scripts
External plugin development: see [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repo, use `plugindev` toolchain, refer to Go and Lua examples in `example/`
```

## Project Status

**v0.7.1** — Core is functional, plugin system and SDK are ready. 11 built-in plugins. External plugin development via [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repo using `plugindev` toolchain. Output channel system, restricted external plugin API, and EventAgentLLMChain event are live.

## Documentation

- [Project Overview](docs/en/OVERVIEW.md) | [中文](docs/zh/OVERVIEW.md)
- [Technical Architecture](docs/en/ARCHITECTURE.md) | [中文](docs/zh/ARCHITECTURE.md)
- [Plugin Development Guide](docs/en/PLUGIN_DEV.md) | [中文](docs/zh/PLUGIN_DEV.md)
- [Lua Adapter](docs/en/ADAPTER.md) | [中文](docs/zh/ADAPTER.md)
- [Knowledge Base Demo](knowledge/homeagent_architecture/content.md)

## Build

```bash
make build build-cli    # Build daemon + CLI
make test               # go test ./...
make install            # Install to system
```

Dependencies: Go 1.25+, CGo (go-sqlite3), Linux/Windows.

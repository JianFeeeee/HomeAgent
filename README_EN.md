> ⚠️ **AI-Assisted Programming Notice**: Parts of this project's code, documentation, and commit history were generated or modified with AI assistance. Key changes have been human-reviewed, but please evaluate and verify before use.

# HomeAgent

> **中文**: [README.md](./README.md)

An Agent framework designed around **separation of core domain and application domain**. The kernel enforces a zero-IO policy — all external interaction (WebUI, QQ, CLI, file operations, web search, memos, etc.) is handled by the plugin layer; the kernel performs no direct IO operations.

Combined with a **three-layer memory architecture** (Context → Document → Graph), it maintains contextual coherence across long-running single-conversation sessions through tiered storage and automated archival.

```go
homed (kernel, zero IO) ← PluginSDK → plugins (all IO capabilities)
```

**Since v1.1.1 media reaches the plugin boundary**: plugins and the model can both read and
write images/audio in memory (`InsertWithMedia`, `InjectInputMedia`). Media lives in plain-text
memory as a `[<mime> <short digest>] <description>` marker — the description is the searchable
semantic memory, the digest is the key back to the bytes.

**Since v1.0.0 external plugins are independent subprocesses**, communicating with the kernel over
stdio JSON-RPC (control plane) + a shared memory segment (data plane) + an event ring (notification
plane). A plugin crash cannot take down the kernel and it restarts automatically; swapping
`plugin.bin` gives true hot-reload.

## Design Principles

**Separation of Core Domain and Application Domain** — The kernel's responsibilities are limited to LLM orchestration, memory management, and knowledge retrieval; all IO capabilities (message send/receive, file read/write, network requests, hardware interaction, etc.) are implemented by plugins. This separation defines domain boundaries at the Agent framework level, with distinct responsibility scopes for the kernel and plugins.

**Three-Layer Memory Architecture** — Manages information retention in long-running agents through a tiered storage strategy:

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

See [`assets/docs/en/ARCHITECTURE.md`](assets/docs/en/ARCHITECTURE.md) for details.

## Web Mascot

<div align="center">
  <img src="assets/branding/mascot-xiaozhai.webp" alt="HomeAgent Web Mascot Xiaozhai" width="200">
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
├── plugin/         Plugin registry + subprocess loader (stdio RPC + shared memory segment + event ring)
├── plugins/        11 built-in plugins (webui/cli/timer/cmd/mcp/clawhubadapter/agentcli/healthcheck/pluginmgr/files/cfgmgr)
├── sdk/            PluginSDK (Tool/Stage/Event three channels)
├── config/         SQLite config center
├── events/         Event bus
└── internal/lua/adapters/   8 LLM protocol adapter scripts
External plugin development: see [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repo, use `plugindev` toolchain, refer to Go and Lua examples in `example/`
```

## Project Status

**v1.1.1** — Multimodal reaches the **plugin boundary**. v1.1.0 gave the memory system binary
multimedia nodes, but that path was open only to the kernel itself; this release opens it to
plugins and the model. The public SDK gains media fields and three media injection methods
(paired with [SDK v1.1.0](https://gitcode.com/JianFeeeee/homeagent-sdk/releases/tag/v1.1.0),
shared by the whole 1.1.x line), and the kernel implements the four matching RPCs. The bridge
layer had been **silently dropping fields**: `Confidence`/types/`SentenceText` handed in by a
plugin were discarded, `Doc` kept only three fields, and `Remove` never released references
(media stayed "referenced" forever, so GC could never reclaim it). `processTextInput` and
`processMediaInput` were unified into a single `processInput`, which finally gives the media
path the dedup, `no_memory`, channel `Cleaner`, interrupt semantics and correct `EventRawInput`
it had always lacked. Three real defects fixed: **user-sent images never appeared in the WebUI
chat log** (the media path published a map while the subscriber asserted a string),
**`memory_commit`'s `sentence_text` had never been exposed to the model** (though it is the
mandatory link in the media binding chain), and **two data races in `PluginSDK`** (11 reported
by `-race`; in production this showed up as sporadic nil-dereference crashes during plugin reload).

**v1.1.0** — Memory system supports **binary multimedia nodes**. Content-addressed media store
(CAS + SQLite metadata + on-disk blobs, `Get` always re-verifies the digest) wired through L0
(context events) / L2 (documents) / L3 (graph sentences), with reference-counted GC (referenced
items are never deleted). The description text produced by the vision model is the durable
semantic memory; the blob is only a cache that capacity GC may evict.

**v1.0.0** — External plugins moved from C ABI shared libraries to **subprocess + shared memory**. The first release that no longer loads `.so`/`.dll`, and it is incompatible with 0.9.x (existing plugins must be rebuilt into `plugin.bin` with the new `plugindev`, though **business code needs zero changes**). Eliminates 6 classes of defects that had caused production incidents: hot-reload silently failing (`DF_1_NODELETE` making `dlclose` a no-op), no crash isolation (a plugin panic took down homed), stage lost updates (35.8~36.8% loss under the copy model), uncancellable cgo timeouts (linear OS-thread leaks), `output_send` reporting false success (the model was told "sent" while the message never went out), and Windows capability degradation (only 3 stage fields visible, no write-back). Three communication planes: stdio JSON-RPC (control) + shared memory segment (data) + event ring (notification); the privilege gradient is now enforced by three explicit gates. RPC round-trip p50 24.1µs; crash-to-recovery under 1s.

**v0.9.0** — C ABI v2: external plugin Stage callbacks can now write back (`invoke_stage` gained a result out-param; plugins may mutate RawMessage/LLMText/ToolResults etc. in OnInput/AfterToolcall/PostAction and have them synced to the core). ABI version now tracks core minor releases (v0.9.x → ABIVersion=2, `version_min=1` keeps old plugins loadable). Also fixes the tool-loop zen-compat placeholder that wrongly fired on first-turn system context tail. The SDK ships an enhanced sanitizer example (bad-UTF-8 / U+FFFD / ANSI-escape scrub across the whole pipeline). **This ABI retired with v1.0.0.**

**v0.8.0** — Core is functional, plugin system enhanced. 20+ built-in plugins. External plugin development via [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repo. Added input channel `NoMemory`/`Cleaner`, `ChannelDef`, plugin disable/enable system (CLI + WebUI), `plugindev` toolchain C ABI `ChannelDef` support.

## Documentation

- [Project Overview](assets/docs/en/OVERVIEW.md) | [中文](assets/docs/zh/OVERVIEW.md)
- [Technical Architecture](assets/docs/en/ARCHITECTURE.md) | [中文](assets/docs/zh/ARCHITECTURE.md)
- [Plugin Development Guide](assets/docs/en/PLUGIN_DEV.md) | [中文](assets/docs/zh/PLUGIN_DEV.md)
- [Lua Adapter](assets/docs/en/ADAPTER.md) | [中文](assets/docs/zh/ADAPTER.md)
- [Knowledge Base Demo](assets/knowledge/homeagent_architecture/content.md)

## Downloads

[Releases](https://gitcode.com/JianFeeeee/HomeAgent/releases) ship three variants:

| Variant | Contents | For |
|---|---|---|
| **full** | homed + waiter + desktop GUI + systemd unit | Single-machine, everything |
| **server** | homed + waiter + systemd unit | Servers (no desktop environment) |
| **client** | waiter + desktop GUI | Connecting to a remote HomeAgent |

- Linux: `.deb` (amd64/arm64), `.rpm` (x86_64), `.tar.gz`
- Windows: `HomeAgent_v1.1.1_{Full,Server,Client}_win64.exe` (NSIS installer)
- Portable: `homeagent-bin-<os>_<arch>.tar.gz` (homed/waiter/initconfig)
- Verification: `SHA256SUMS`

The macOS `homed` requires a native macOS build (CGO + sqlite3), so release packages ship only `waiter`/`initconfig`.

## Build

```bash
make build build-cli    # Build daemon + CLI
make test               # go test ./...
make install            # Install to system
```

Dependencies: Go 1.25+, CGo (go-sqlite3), Linux/Windows.

## License

This project is released under the **GNU Affero General Public License, version 3
(AGPL-3.0-only)** — see [LICENSE](LICENSE).

This is the strongest copyleft in the GPL family: besides shipping the complete corresponding
source when you distribute the software, **you must also offer the source to users who interact
with it over a network** (§13, Remote Network Interaction). Anyone running a modified HomeAgent
as a network service therefore has to make the modified source available to that service's users.

Plugins are **statically linked** against this project through the public SDK (the SDK source
ends up inside the plugin binary), so plugins are derivative works and must be released under
the same license. Process isolation does not change this — what is linked is the SDK code itself.

### Third-party components shipped with the packages

| Component | License | Location |
|---|---|---|
| Chinese-CLIP ViT-B/16 (ONNX artifacts) | Apache-2.0 | `/usr/lib/homeagent/models/chinese-clip-vit-b16-onnx/` |
| ONNX Runtime (`libonnxruntime.so`) | MIT | `/usr/lib/homeagent/onnxruntime/` |
| jieba dictionary (embedded in the binary) | MIT | `internal/memory/jiebadict/` |
| Go dependencies (go-sqlite3, gojieba, bubbletea, …) | MIT / BSD-3 / Apache-2.0 | permissive, AGPL-3.0-compatible |

These components keep their own licenses and are not relicensed by this project. Full texts are
shipped in `/usr/share/doc/homeagent/licenses/`, and the package metadata declares this package
as `AGPL-3.0-only`.

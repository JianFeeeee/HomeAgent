[English](../en/OVERVIEW.md) | **中文**

# HomeAgent — 项目概览

## 这是什么

HomeAgent 是一个持续运行的个人智能 Agent 框架。

核心架构：一个长时间运行的内核进程（`homed`），通过插件系统接入各种 IO 通道（QQ、Web、命令行等）。内核负责 LLM 调用编排、记忆管理、知识检索；插件负责所有外部 IO——收发消息、执行文件操作、搜索网络等。

### 核心创新

**核心域与应用域分离** — 这是首个明确提出这一划分的 Agent 框架。内核（核心域）不做任何 IO，所有 IO 能力归属插件（应用域）。边界通过 PluginSDK 明确定义：
- 插件向内核注册工具（Tool），供 LLM 调用
- 插件挂入处理管道（Stage），在各阶段拦截/改写消息流
- 插件订阅/发布事件（Event），松耦合通信
- 插件通过 IO API 排队或打断投递输入

这一划分的意义：内核保持纯粹（零 IO，只做编排和记忆），插件保持灵活（各司其职，热加载），互不污染。

**三层记忆架构** — 解决 Agent 长期运行的记忆衰减：
- **Context 层**：内存中局部词嵌入评分的事件窗口（jieba + TF-IDF + PMI → CosineSimilarity），实时维护最近上下文，低相关性事件自动下沉到下一层
- **Document 层**：JSON 文件 + TF-IDF 向量索引的临时记忆，支持显式提交和隐式归档，冷数据蒸馏到 Graph
- **Graph 层**：SQLite 图数据库，持久化实体（entities）和关系（relations），BFS 遍历召回，蒸馏管道从对话中提取三元组

三层递进：上下文 → 冷归档 → 长期图记忆，确保 Agent 长时间运行不退化。

## 它实际做了什么

代码位于项目仓库根目录，Go 语言实现。

**内核** (`internal/agent/core/agent.go`)：
- 维护一个消息循环（`eventLoop`），从 IO 层排队接收输入
- 每次输入走完整的处理管道：记忆召回 → 人格注入 → LLM 调用 → 工具执行 → 输出发送
- LLM 调用通过 Provider 接口抽象，支持 8 个 LLM 源自动降级
- 上下文管理（`context.go`）基于词嵌入评分（LocalWordEmbedder → CosineSimilarity），自动剪枝低相关性事件

**记忆系统** (`internal/memory/`)：
- **GraphDB** (`graph.go`) — SQLite，entities + relations 表，BFS 遍历
- **Document Store** (`document/document.go`) — 临时记忆，JSON 文件 + TF-IDF 向量索引，消费即删
- **Text Memory** (`text/text.go`) — 原始对话日志，JSONL 文件轮转
- **Social Store** (`social/social.go`) — 人格特质 + 关系网，包装 GraphDB
- **Memory Indexer** (`indexer.go`) — 自动将 GraphDB 实体向量化，用户输入时召回注入 system prompt

**知识库** (`internal/knowledge/knowledge.go`)：
- 文件系统目录 `knowledge/<name>/content.md`
- TF-IDF 向量搜索，独立于记忆系统的索引实例
- LLM 通过 `knowledge_search` / `knowledge_create` / `knowledge_list` 三个工具操作

**插件系统** (`internal/plugin/`)：
- 内置插件：Go `init()` 自注册，编译进内核
- 外部插件：Go `-buildmode=c-shared` 编译为 `.so`，通过 C ABI bridge 动态加载；也支持 Lua 脚本插件
- PluginSDK (`internal/sdk/`) 定义四通道：RegisterTool / RegisterStage / Subscribe / RegisterOutputChannel
- 阶段钩子 7 个：on_input → pre_action → post_action → before_toolcall → after_toolcall → before_output → after_output

**LLM Provider** (`internal/agent/api/provider.go`)：
- Provider 接口：Name / Chat / ChatStream
- 三种实现：OpenAIProvider（标准 OpenAI API）、OllamaProvider（本地）、LuaAdaptedProvider（Lua 胶水适配）
- LuaAdapter 位于 `internal/lua/adapters/`，每个 LLM 源对应一个 `.lua` 脚本
- 内置 8 个适配器：deepseek / openai / anthropic / gemini / mistral / groq / github / ollama

**WebUI** (`internal/plugins/webui/`)：
- 嵌入式 SPA 仪表盘（`dashboard.html` 通过 `//go:embed` 打包）
- REST API：状态查询、配置管理、记忆操作、知识库管理、插件管理
- 兼容 OpenAI API 格式的 `/v1/chat/completions` 端点
- SSE 事件流 `/api/v1/chat/events`

## 项目状态

核心功能已可运行。插件系统和 SDK 已就绪，可独立开发外部插件。

- 内置插件：webui / cli / timer / cmd / mcp / agentcli / healthcheck / pluginmgr / openclaw / files
- 外部插件示例（[homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) 仓库 `example/`，含 Go 和 Lua 两种类型）：qq / files / web / memo / bili / editdoc / a2a / ocr / sanitizer / luaplugintest / testlua
- 打包分发：`.hmap` 插件包格式，通过 WebUI 安装

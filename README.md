# HomeAgent

24/7 智能管家。**核心零 IO**，一切外界交互来自插件。

## 架构概览

```
homed (内核) — 零 IO，纯管理
  ├── LLM 源管理 (Lua 适配器协议转换)
  ├── Agent 编排 (主 agent + interceptLoop + 子 agent)
  ├── 三层记忆 (Context → Document → Graph)
  ├── 知识库 (独立 TF-IDF)
  ├── IO 通道管理 (Queue / Interrupt / Output)
  ├── 阶段管道 (StageHost: 7 阶段并行)
  └── 事件总线 (EventBus)
        │
        ▼ PluginSDK (Go API: 工具/阶段/事件/记忆/知识/LLM/配置)
        │
  plugins (init() 自注册 + .so 动态加载)
  ├── WebUI (HTTP 服务)
  ├── CLI (Unix socket)
  ├── OpenClaw 兼容 (SKILL.md → SDK 工具)
  ├── Timer (timer_set 工具 + 中断反馈)
  └── 第三方 .so 插件 (plugins/<name>/plugin.so)
```

核心文档: [PLAN.md](PLAN.md) · [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

## 构建

```bash
make build
./build/homed -data /tmp/homeagent
./build/waiter -say "你好"
```

依赖: Go 1.19+, CGo (go-sqlite3), Linux (Unix socket + overlayfs)

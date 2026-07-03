# HomeAgent 架构设计 v4

完整架构文档参见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 核心原则

- **核心零 IO** — 无任何硬编码 IO 能力，所有 IO 来自插件
- **输出是工具调用** — Agent 必须显式 `output_send` 才能通信
- **三通道插件** — 工具 (RegisterTool)、阶段 (RegisterStage)、事件 (Subscribe/Publish)
- **阶段管道** — 7 个 hook 点让插件干预消息处理流：`on_input` → `pre_action` → `post_action` ↔ `before_toolcall`/`after_toolcall` → `before_output` → `after_output`
- **三层记忆** — Context (内存) → Document (JSON+向量) → Graph (SQLite)
- **知识独立** — 独立 TF-IDF 向量索引，不与记忆耦合

## 快速启动

```bash
make build        # 编译
make run          # 编译并启动（数据 /tmp/homeagent）
```

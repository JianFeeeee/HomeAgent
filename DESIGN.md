# HomeAgent 架构设计

完整架构文档参见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。
实施计划参见 [PLAN.md](PLAN.md)。

## 核心原则

- **核心零 IO** — 无任何硬编码 IO 能力
- **输出是工具调用** — Agent 必须显式调用 output_send 才能通信
- **三通道插件** — 工具 (RegisterTool)、阶段 (RegisterStage)、事件 (Subscribe/Publish)
- **阶段管道** — 7 个 hook 点让插件干预消息处理流
- **三层记忆** — Context (内存) → Document (JSON+向量) → Graph (SQLite)
- **知识独立** — 独立 TF-IDF 向量索引，不与记忆耦合

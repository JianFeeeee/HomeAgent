# HomeAgent

单二进制 24/7 智能管家。**核心零 IO**，所有输入输出通过插件，插件通过三通道与核心交互：工具、阶段钩子、事件订阅。

## 架构

```
  外部（QQ / HTTP / 硬件...）
    │ 通过插件注册
    ▼
  IOManager.InjectInput()
    │
    ▼
  ┌─────────────────────────────────────────────────┐
  │              Agent Core                          │
  │                                                  │
  │  on_input → Context → Memory Recall              │
  │  → pre_action → [LLM ↔ 工具循环] → before_output │
  │  → output_send → after_output                   │
  │                                                  │
  │  内置：三层记忆 + 知识库 + Child Agent           │
  └─────────────────────────────────────────────────┘
    │
    ├── Stage Pipeline（7 阶段，插件可拦截/改写）
    ├── Tool System（插件注册工具给 LLM）
    └── Event Bus（插件订阅系统事件）
```

## 快速开始

```bash
make build
./build/homed -data /tmp/homeagent
```

依赖：Go 1.19+、CGo（go-sqlite3）。

## 阶段管道

```
on_input → pre_action → post_action ↔ before_toolcall/after_toolcall → before_output → after_output
                                           ↑_______________|
                                               循环
```

| 阶段 | 插件可做 |
|---|---|
| `on_input` | 鉴权、拉黑、改写、短路 |
| `pre_action` | 注入 context 消息 |
| `post_action` | 审查/改写 LLM 输出、增删工具 |
| `before_toolcall` | 拒绝、改参、审计 |
| `after_toolcall` | 脱敏、改写结果 |
| `before_output` | 改写最终文本、加格式 |
| `after_output` | 记录/统计 |

## 三层记忆

| 层 | 存储 | 容量 | 裁剪 |
|---|---|---|---|
| Context | 内存 TF-IDF | 30 条 | 余弦相似度排序→文档 |
| Document | JSON + 向量索引 | ∞ | 72h 冷→图 |
| Graph | SQLite 三元组 | ∞ | 定期重整+同义合并 |

## 插件三通道

| 通道 | 方向 | 用途 |
|---|---|---|
| `RegisterTool` | 插件→LLM | Agent 调用插件功能 |
| `RegisterStage` | 核心→插件 | 干预消息处理流 |
| `Subscribe/Publish` | 双向 | 审计/日志/通知 |

## 核心命令

```bash
make build        # 编译
make run          # 编译+启动
make test         # 测试
make install      # 系统安装
```

## 完整文档

详见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

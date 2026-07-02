# HomeAgent

单二进制 24/7 智能家庭管家。基于 NextAgent 认知解耦架构 + TrulyMEM 自主图记忆。

## 架构

```
                    IO 抽象层（唯一输入路径）
  ┌─────────────────────────────────────────────────────┐
  │  Microphone  Camera  GPIO  HTTP  OneBot-QQ  Plugins │
  │  所有外部输入 → InputEvent → inputCh                  │
  └──────────────────────┬──────────────────────────────┘
                         │
  ┌──────────────────────▼──────────────────────────────┐
  │              Agent Core（编排器）                      │
  │  三层记忆注入 → Provider.Chat() → 工具执行 → 输出     │
  └──────────────────────┬──────────────────────────────┘
                         │
  ┌──────────────────────▼──────────────────────────────┐
  │             API 抽象层（唯一输出路径）                  │
  │   Provider: OpenAI / Ollama / Lua 适配               │
  │   DeepSeek v4 flash（默认）                          │
  └─────────────────────────────────────────────────────┘
```

## 快速开始

```bash
make build
./build/homed -data /tmp/homeagent
```

依赖：Go 1.19+、CGo（go-sqlite3）。

## 记忆体系（三层）

| 层 | 存储 | 容量 | 裁剪策略 |
|---|---|---|---|
| 上下文 | 内存 | 30 条 | TF-IDF 相关性排序，淘汰→文档 |
| 文档 | JSON + TF-IDF 向量 | 无上限 | 72h 冷访问→图数据库 |
| 图 | SQLite 三元组 | 无上限 | 定期重整+同义合并 |

## 插件系统（OpenClaw 兼容）

插件 = 容器，内嵌 IO 通道为组件。`plugins/` 目录热插拔，agent 通过 `plgreload` 工具控制重载。

```
plugins/
├── qq/                          # OneBot QQ 通道插件
│   ├── SKILL.md                 # 技能描述 + IO 端口声明
│   └── skill.json               # 元数据 + WS 连接配置
```

### 示例 QQ 插件 `plugins/qq/SKILL.md`

```markdown
# QQ 通知插件
io_type: io
io_input_route: qq
io_output_route: qq
io_output_caps: text,file,image

## qq_send_private_msg
发送 QQ 私聊消息
- user_id: 目标 QQ 号
- message: 消息内容（支持 CQ 码）
```

## 核心命令

```bash
make build        # 编译主二进制
make run          # 编译并启动（数据 /tmp/homeagent）
make install      # 安装到系统
make test         # 运行测试
```

## 配置

`/etc/homeagent/config.yaml`（默认 `config/config.yaml`）：

```yaml
daemon:
  listen_addr: ":8080"
  data_dir: "/var/lib/homeagent"
llm:
  model: "deepseek-v4-flash"
  base_url: "https://api.deepseek.com/v1"
```

## 许可证

MIT

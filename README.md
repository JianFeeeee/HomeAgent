# HomeAgent

24/7 智能管家。**核心零 IO**，一切外界交互来自插件。

📖 [项目概览（非技术）](docs/OVERVIEW.md) ·
🔧 [插件开发指南](docs/PLUGIN_DEV.md) ·
🏗️ [技术架构](docs/ARCHITECTURE.md) ·
📋 [实施计划](PLAN.md)

## 快速体验

```bash
# 构建
make build build-cli

# 启动内核（需要 DeepSeek API 密钥）
DEEPSEEK_API_KEY="sk-xxx" ./build/homed -data /tmp/ha

# 交互模式（自动发现 socket）
./build/waiter

# 或单条消息
echo "你好" | ./build/waiter
```

配置文件 `~/.config/homeagent/cli.yaml`：

```yaml
mode: auto          # auto / local / remote
colors: true
history_size: 1000
prompt: "waiter> "
```

## 架构一句话

```
homed（内核零 IO）← PluginSDK → 插件（所有 IO 能力）
```

依赖：Go 1.19+, CGo (go-sqlite3), Linux。

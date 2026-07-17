# C ABI 插件桥接计划

## 问题

Go `-buildmode=plugin` 要求插件的所有 Go 依赖（包括标准库）与核心编译版本完全一致，导致：
- 核心升级 Go 版本 → 所有插件必须重新编译
- 不同环境下编译的插件无法加载
- 插件开发者必须与核心使用完全相同的构建环境

## 方案

完全采用 C ABI 作为插件和核心的通信层，彻底摒弃 Go `-buildmode=plugin`。

```
┌──────────────────────┐
│  核心 (编译时)        │
│  ├── 内部插件         │ ← init() 自注册，纯 Go，伴随核心编译
│  └── 核心逻辑         │
├──────────────────────┤
│  外部插件 (运行时)     │
│  ┌──────────────────┐│
│  │  插件 Go 代码     ││ ← 插件开发者写 Go，接口不变
│  ├──────────────────┤│
│  │  SDK Go Wrapper  ││ ← 序列化/反序列化，隐藏 C ABI
│  ├──────────────────┤│
│  │  C ABI 函数表     ││ ← 唯一的共享 ABI，版本隔离层
│  ├──────────────────┤│
│  │  核心 C 接收层    ││ ← dlopen + dispatch → 内部 API
│  └──────────────────┘│
└──────────────────────┘
```

外部插件统一走 `-buildmode=c-shared`，编译为 C ABI `.so`，核心通过 `dlopen` 加载。

---

## 阶段一：C ABI 函数表定义（SDK 仓库）

**位置**：`homeagent-sdk/tools/plugindev/templates.go`（新建 C ABI 模板，独立于现有 Windows `tmplBridge`）

- [ ] 1.1 定义 C ABI 函数表结构体 `HomeAgentCABI`（C 代码）
- [ ] 1.2 核心→插件方向（核心调插件）：
  - `init_plugin(name, config_json) → error_json`
  - `start_plugin(sdk_table, version) → error_json`
  - `stop_plugin() → error_json`
  - `invoke_tool(name, args_json) → result_json`
  - `invoke_stage(stage, ctx_json) → error_json`
  - `free_string(ptr)` — 释放插件返回的 C 字符串
- [ ] 1.3 插件→核心方向（插件调核心，通过 `sdk_table` 传入）：
  - `register_tool(name, def_json, handler_id) → error_json`
  - `register_stage(stage, handler_id) → error_json`
  - `register_output_channel(name, caps, desc, handler_id) → error_json`
  - `inject_text(source, channel, text) → error_json`
  - `inject_interrupt_text(source, channel, text) → error_json`
  - `memory_recall(query, depth) → result_json`
  - `memory_commit(triples_json) → error_json`
  - `memory_introspect() → result_json`
  - `doc_query(text, topK) → result_json`
  - `knowledge_search(query, topK) → result_json`
  - `settings_get(key) → value_json`
  - `settings_set(key, value_json) → error_json`
  - `llm_list_sources() → result_json`
  - `social_get_person(name) → result_json`
  - `subscribe(event_type, handler_id) → error_json`
  - `free_string(ptr)` — 释放核心返回的 C 字符串
- [ ] 1.4 定义回调 dispatch 机制（插件侧注册，核心侧触发）：
  - `tool_handler(handler_id, args_json) → result_json`
  - `stage_handler(handler_id, ctx_json) → error_json`
  - `output_handler(handler_id, msg_type, payload_json) → error_json`
  - `event_handler(handler_id, event_json) → error_json`
- [ ] 1.5 入口导出：
  ```c
  extern HomeAgentCABI* plugin_init(void);
  ```
- [ ] 1.6 生成的 C 桥代码放入 `tmplCABIBridge`，与现有 Windows `tmplBridge` 独立

---

## 阶段二：核心侧 C ABI 加载器

**位置**：`HomeAgent/internal/plugin/cabi/`

- [ ] 2.1 创建 `internal/plugin/cabi/types.go`
  - ABI 版本号常量
  - `PluginHandle` 结构（封装 `dlopen` handle + C 函数表指针）
  - 返回/错误结构体定义
- [ ] 2.2 创建 `internal/plugin/cabi/bridge.go`
  - 核心侧暴露给插件的 C 函数表
  - 每个函数：接收 JSON → 反序列化 → 调内部 API → 序列化返回
  - 回调注册表：handler_id → Go 回调函数的映射
- [ ] 2.3 创建 `internal/plugin/cabi/loader.go`
  - `Load(path string) (*PluginHandle, error)`：
    1. `dlopen(path, RTLD_NOW|RTLD_LOCAL)`
    2. `dlsym("plugin_init")` → 获取 C 函数表指针
    3. 检查 version 兼容性
    4. 初始化：`init_plugin(name, config)` → `start_plugin(sdk_table)`
  - `Unload(handle *PluginHandle) error`：
    1. `stop_plugin()`
    2. `dlclose`
- [ ] 2.4 创建 `internal/plugin/cabi/sdk_stub.go`
  - 核心侧接收插件 SDK 调用的 dispatch 层
  - 每个 dispatch 对应一个 PluginSDK 方法
- [ ] 2.5 序列化辅助函数
  - `MarshalJSON / UnmarshalJSON / FreeCString`

---

## 阶段三：核心注册器集成

**位置**：`HomeAgent/internal/plugin/registry.go`

- [ ] 3.1 内部插件路径不变（`RegisterFactory` → `init()` 自注册，编译进内核）
- [ ] 3.2 外部插件加载改为 C ABI：
  ```
  func loadExternalPlugin(path string) (sdk.Plugin, error) {
      return cabi.Load(path)
  }
  ```
- [ ] 3.3 实现 `cabiPlugin` 适配器（实现 `sdk.Plugin` 接口）：
  - `Name()` → 从 manifest 读取
  - `Start(sdk)` → 核心创建 PluginSDK 实例传入 C ABI
  - `Stop()` → 调用 `stop_plugin`
- [ ] 3.4 移除 Go `plugin.Open` / `Lookup` 代码路径（仅影响外部插件加载）
- [ ] 3.5 动态插件结果验证：加载 / 调用工具 / 卸载全流程测试

---

## 阶段四：SDK 侧 C ABI dispatch

**位置**：`homeagent-sdk/sdk/plugin.go` + 新增 `sdk/cabi.go`

- [ ] 4.1 创建 `sdk/cabi.go`
  - 定义 C ABI dispatch 结构体和客户端
  - 序列化/反序列化辅助函数
  - 回调注册表（handler_id → handler 映射）
- [ ] 4.2 修改 `PluginSDK` 结构，移除所有 Go 接口引用，改为 C ABI dispatch：
  ```go
  type PluginSDK struct {
      abi  *CABI
  }

  func (s *PluginSDK) RegisterTool(name string, def ToolDef, handler ToolHandler) error {
      // register handler in callback table → get handler_id
      // marshal(def) → call("register_tool", name, def_json, handler_id) → unmarshal result
  }
  ```
- [ ] 4.3 入口函数导出：
  ```go
  //export plugin_init
  func plugin_init() *C.HomeAgentCABI {
      return &C.HomeAgentCABI{...}
  }
  ```
- [ ] 4.4 构建约束：
  - `//go:build cgo` — 所有调用 cgo 的代码
  - plugin 入口文件 `main.go` 去掉 `!windows` 约束

---

## 阶段五：plugindev 工具链适配

**位置**：`homeagent-sdk/tools/plugindev/`

- [ ] 5.1 `cmd_build.go` — Linux/macOS 默认 `-buildmode=c-shared`：
  ```go
  // 不再有 -buildmode=plugin 选项
  // 默认 target=linux/amd64 → -buildmode=c-shared → plugin.so + C ABI bridge
  // Windows → -buildmode=c-shared → plugin.dll + C ABI bridge
  ```
- [ ] 5.2 `cmd_init.go` — 生成的 `plg.json` 不再包含 `abi` 字段（统一 C ABI）
- [ ] 5.3 `templates.go` — 移除 `tmplBridge`（Windows 专用）与 `tmplCABIBridge` 合并为统一桥
- [ ] 5.4 移除现有的 `//go:build !windows || !cgo` / `//go:build windows && cgo` 分支

---

## 阶段六：示例插件迁移

- [ ] 6.1 逐个验证所有 example 插件在 C ABI 模式下编译通过
- [ ] 6.2 部署测试：编译 → 安装 → 加载 → 功能验证
- [ ] 6.3 压力测试：反复加载/卸载 / 并发工具调用

---

## 阶段七：文档

- [ ] 7.1 更新 `homeagent-sdk/README.md` — 构建说明
- [ ] 7.2 更新核心 `docs/zh/PLUGIN_DEV.md` 和 `docs/en/PLUGIN_DEV.md`
- [ ] 7.3 更新核心 `docs/zh/ARCHITECTURE.md` 和 `docs/en/ARCHITECTURE.md`
- [ ] 7.4 移除所有关于 Go plugin 的文档描述

---

## 向后兼容策略

因零存量，无需兼容旧格式。C ABI 版本号仅用于核心和插件间的接口协商。

```c
typedef struct {
    int version;              // ABI 版本号
    int version_min;          // 兼容的最低版本号
    // ... 函数指针表
} HomeAgentCABI;
```

加载时：
- 核心 `version >= plugin.version_min` → 加载
- 核心 `version < plugin.version_min` → 拒绝加载，报明确错误信息

---

## 风险与权衡

| 风险 | 缓解 |
|------|------|
| C ABI 序列化开销（JSON 编解码） | 非性能关键路径，可接受；后期可切 MessagePack |
| cgo 调用开销（~1-2µs/次） | 相对于 LLM 调用（秒级）可忽略 |
| 回调桥接（ToolDef.Handler 从插件侧回调核心） | handler ID 表 + goroutine 桥接 |

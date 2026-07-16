**中文** | [English](../zh/PLUGIN_DEV.md)

# HomeAgent Plugin Development Guide

## Overview

All external interaction capabilities of HomeAgent comes from plugins. Plugins interact with the kernel through `PluginSDK` (Go API).

**SDK Repository**: Plugin development tools, template code, and example plugins are hosted in the [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk) repository.

```bash
git clone https://gitcode.com/JianFeeeee/homeagent-sdk.git
cd homeagent-sdk
```

Each plugin implements a three-method interface:

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### Three Development Methods

| Method | Use Case | Complexity |
|--------|----------|------------|
| **Dynamic .so/.dll plugin (recommended)** | Independently distributed third-party plugins | Medium, generated using `plugindev` toolchain |
| **Built-in plugin** | Released with HomeAgent | Simple, requires merging into main repo |
| **Lua script plugin** | Lightweight rapid prototyping | Simple, generated using `plugindev init --lua` |

---

## 1. Quick Start: Using the plugindev Toolchain

`plugindev` is the unified plugin development toolchain provided in the SDK repository, supporting both Go and Lua plugin types.

### Installation

```bash
cd homeagent-sdk/tools/plugindev
go build -o plugindev.exe
# Add plugindev.exe to PATH or use directly
```

### Creating a Go Plugin

```bash
plugindev init myplugin
cd myplugin
# Edit plugin code
vim plugin.go
# Build and package
plugindev build
# Output: dist/myplugin_linux_amd64.hmap (or windows_amd64)
```

### Creating a Lua Plugin

```bash
plugindev init myluaplugin --lua
cd myluaplugin
# Edit plugin code
vim main.lua
# Local test
lua main.lua
# Build and package
plugindev build
# Output: dist/myluaplugin_lua.hmap
```

### Template Project Structure

**Go plugin**:

```
myplugin/
├── plg.json       — Plugin metadata (name, version, entry, target platform)
├── main.go        — Entry point (compiled for non-Windows or non-cgo)
├── plugin.go      — Plugin implementation (Plugin interface)
├── go.mod         — Go module definition
└── README.md      — Documentation
```

**Lua plugin**:

```
myluaplugin/
├── plg.json       — Plugin metadata (entry: "main.lua", targets: "lua")
├── main.lua       — Plugin implementation (Lua version of Plugin interface)
├── sdk.lua        — SDK mock layer (supports `lua main.lua` standalone testing)
└── README.md      — Documentation
```

### Build & Package

`plugindev build` automatically handles compilation and packaging:

```bash
cd myplugin
plugindev build
```

Execution process:
1. Reads `plg.json` to determine target platform
2. **Go plugin**: Runs `go build -buildmode=plugin` (Linux) or `-buildmode=c-shared` (Windows)
3. **Lua plugin**: Packages source code directly, no compilation needed
4. Generates `plugin.json` manifest file
5. Packages as `.hmap` distribution (zip format, containing `plugin.json` + `plugin.so`/`plugin.dll`/`main.lua`)

Output in `dist/` directory:
```
dist/
├── myplugin_linux_amd64.hmap      # Go plugin Linux version
├── myplugin_windows_amd64.hmap    # Go plugin Windows version
└── myplugin_lua.hmap              # Lua plugin
```

### Deployment

Install via PluginMgr HTTP API:

```bash
# Kernel PluginMgr listens on :9876
curl -X POST http://127.0.0.1:9876/plugins \
  -F "file=@dist/myplugin_linux_amd64.hmap"
```

Or upload via WebUI plugin management page.

---

## 2. Go Plugin Development in Detail

### Plugin Interface

```go
package main

import "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

type Plugin struct {
    name string
    sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    p.sdk = s
    // Register config items, tools, stage hooks, etc.
    return nil
}

func (p *Plugin) Stop() error {
    // Clean up resources
    return nil
}

// NewPluginFactory creates plugin instance (called by main.go or Windows bridge)
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

### Entry Point

`main.go` provides the `NewPlugin` export function, which is the entry point when the kernel loads the plugin:

```go
//go:build !windows || !cgo

package main

import "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return NewPluginFactory(name, config)
}
```

For Windows `-buildmode=c-shared`, `plugindev build` auto-generates C ABI bridge code, no manual handling needed.

### PluginSDK Core API

#### Tool Registration — Make your capabilities callable by LLM

```go
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "Query weather for a specified city",
    Parameters: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "city": map[string]interface{}{
                "type":        "string",
                "description": "City name, e.g. Beijing",
            },
        },
        "required": []string{"city"},
    },
}, func(args map[string]interface{}) (interface{}, error) {
    city, _ := args["city"].(string)
    return map[string]interface{}{
        "city":    city,
        "temp":    25,
        "weather": "Sunny",
    }, nil
})
```

#### Stage Hooks — Intervene in message processing flow

7 stages:

| Stage | Timing | Purpose |
|-------|--------|---------|
| `on_input` | Message just arrived at Agent | Blacklist, rate-limit, short-circuit |
| `pre_action` | About to call LLM | Inject context |
| `post_action` | LLM returned results | Modify output/tool list |
| `before_toolcall` | Before tool execution | Audit, reject, modify params |
| `after_toolcall` | After tool execution | Desensitize, rewrite results |
| `before_output` | Before output | Format adaptation |
| `after_output` | After output | Statistics/logging |

```go
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
    ctx.Lock()
    ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
        "role":    "system",
        "content": "Injected context content",
    })
    ctx.Unlock()
    return nil
})
```

#### Configuration Management

```go
// Register config definition
s.Settings().RegisterDef(sdk.ConfigDef{
    Key:         "plugin.myplugin.api_key",
    Default:     "",
    Type:        "string",
    DisplayName: "API Key",
    Description: "API key",
    Category:    "myplugin",
})

// Read/write config
val, err := s.Settings().Get("api_key")
s.Settings().Set("api_key", "new-value")

// Read core config
s.Settings().GetCore("llm.model")

// Read other plugin's config
s.Settings().GetPlugin("other_plugin", "some_key")
```

#### Input Delivery

```go
// Queued delivery (processed in order)
s.InjectInput(source, channel, eventType string, payload map[string]interface{})

// Interrupt delivery (can interrupt current LLM processing)
s.InjectInterrupt(source, channel, eventType string, payload map[string]interface{})

// Shortcuts
s.InjectText(source, channel, text string)
s.InjectInterruptText(source, channel, text string)
```

#### Event Subscription

```go
unsub := s.Subscribe("tool_call", func(evt *events.Event) {
    log.Printf("Tool was called: %v", evt.Payload)
})
defer unsub()
```

#### Capability Access

```go
// Memory
s.Memory().Recall(query string) ([]MemItem, error)
s.Memory().Commit(triples []Triple) error

// Knowledge
s.Knowledge().Search(query string) ([]string, error)

// LLM source management
s.LLM().ListSources() []SourceInfo
s.LLM().SetSource(name string) error
```

#### Event Subscription (built-in plugins)

```go
// Subscribe to system events, returns unsubscribe function
unsub := s.Subscribe("tool_call", func(evt *events.Event) {
    log.Printf("Tool was called: %v", evt.Payload)
})
defer unsub()

// Publish event
s.Publish(&events.Event{
    Type:    "custom_event",
    Payload: map[string]interface{}{"key": "value"},
})
```

#### IO Channel Management (built-in plugins)

```go
// Register a channel (bind device driver), dev must implement the agentIO.Device interface:
//   Name() string
//   Type() DeviceType
//   Description() string
//   Tools() []ToolDef
//   Execute(tool string, args map[string]interface{}) (interface{}, error)
//   Start() error
//   Stop() error
//   OutputCapabilities() OutputCapability
s.RegisterChannel("mydevice", deviceImpl)

// Unregister a channel
s.UnregisterChannel("mydevice")

// List all channels
channels := s.ListChannels()
```

> **Note**: `Subscribe`, `Publish`, `RegisterChannel`, `UnregisterChannel`, `ListChannels`, `InjectInput`, `InjectInputSync`, `InjectInterrupt`, `InjectTextSync`, `InjectTextSyncNoMemory`, `OutputChan` are only available in built-in plugins (`internal/sdk` package). External dynamic plugins should use the public APIs: `InjectText`, `InjectInterruptText`, `InjectTextNoMemory`.

---

## 3. Lua Plugin Development in Detail

Lua plugins are suitable for lightweight rapid prototyping, requiring no Go compilation environment. Changes take effect after kernel restart.

### Plugin Structure

```lua
-- main.lua
local plugin = {
  name = "myluaplugin"
}

function plugin.start(sdk)
  sdk.log("info", "myluaplugin starting...")

  sdk.register_tool("myluaplugin_hello", {
    description = "A hello world tool",
    parameters = {
      type = "object",
      properties = {}
    }
  }, function(args)
    return { content = "Hello from myluaplugin plugin!" }
  end)

  sdk.log("info", "myluaplugin started")
end

function plugin.stop()
  sdk.log("info", "myluaplugin stopped")
end

return plugin
```

### SDK Mock Layer

`sdk.lua` provides a pure Lua SDK mock implementation, supporting `lua main.lua` standalone testing:

```bash
lua main.lua
# Output:
# [lua-plugin] info: myluaplugin starting...
# [lua-plugin] register_tool: myluaplugin_hello
# [lua-plugin] info: myluaplugin started
```

When running inside the kernel, `sdk.*` global variables are injected by the Go layer, and all functions marked with `-- !impl` are replaced with real implementations.

### Lua SDK API

| Function | Description |
|----------|-------------|
| `sdk.log(level, msg)` | Log output |
| `sdk.register_tool(name, def, handler)` | Register tool |
| `sdk.register_stage(stage, handler)` | Register stage hook |
| `sdk.register_api(name)` | Register API |
| `sdk.get_setting(key)` | Read config |
| `sdk.set_setting(key, value)` | Write config |
| `sdk.inject_text(source, channel, text)` | Deliver text message |
| `sdk.inject_interrupt(source, channel, text)` | Interrupt delivery |
| `sdk.json.encode(val)` | JSON encode |
| `sdk.json.decode(str)` | JSON decode |
| `sdk.http.get(url)` | HTTP GET request (`-- !impl`) |
| `sdk.http.post(url, body, content_type)` | HTTP POST request (`-- !impl`) |

> **Note**: Lua plugin's `sdk.register_stage` callback currently only receives `raw_message`, `user_id`, `phase` fields. The functionality is limited. For complex stage handling logic, use Go plugins.

---

## 4. Built-in Plugins

Built-in plugins use `init()` self-registration, compiled into the kernel, no separate deployment needed.

### Directory Structure

```
internal/plugins/yourplugin/
    plugin.go       — Plugin main file
```

### Minimal Plugin Example

```go
package yourplugin

import (
    "gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
    sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
    plugin.RegisterFactory("yourplugin", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name), nil
    })
}

type Plugin struct {
    name string
}

func New(name string) *Plugin {
    return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    // Initialize plugin here: start goroutines, register tools, subscribe events, etc.
    return nil
}

func (p *Plugin) Stop() error {
    // Clean up resources
    return nil
}
```

### Register with Kernel

Add blank import in `internal/plugins/all.go`:

```go
package plugins

import (
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/yourplugin"
    // ... other plugins
)
```

---

## 5. Best Practices

1. `Start()` is non-blocking — start long tasks in goroutines, don't block Start
2. `Stop()` cleans up resources — close connections, stop goroutines, cancel subscriptions
3. Unique tool names — use plugin name prefix to avoid conflicts
4. When handler returns `error`, LLM will receive it and may retry
5. Use `InjectInterruptText` for interrupts, `InjectText` for normal delivery
6. Use `Settings().Get/Set` for config, don't hardcode
7. External Go plugins compile independently, not tied to kernel version; only built-in plugins need recompilation with kernel

---

## 6. Example Plugin Reference

### SDK Repository Examples (`homeagent-sdk/example/`)

| Example | Type | Features |
|---------|------|----------|
| [memo](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/memo) | Go | Memo management, PreAction injection + timed interrupt dual reminder |
| [files](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/files) | Go | File system operations, 4 write modes, sandbox isolation |
| [web](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/web) | Go | DuckDuckGo search + web scraping, SSRF protection |
| [qq](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/qq) | Go | NapCat OneBot integration, 17 tools |
| [bili](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/bili) | Go | Bilibili video download (you-get) |
| [editdoc](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/editdoc) | Go | Office document editing and format conversion |
| [a2a](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/a2a) | Go | Agent-to-Agent protocol |
| [ocr](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/ocr) | Go | Offline text recognition (Tesseract) |
| [sanitizer](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/sanitizer) | Go | Output sanitizer filter |
| [luaplugintest](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/luaplugintest) | Lua | Lua plugin Hello World |
| [testlua](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/testlua) | Lua | Lua plugin example |

### Built-in Plugins

| Plugin | Location | Features |
|--------|----------|----------|
| Timer | `internal/plugins/timer/` | Simplest complete example, registers one tool + interrupt feedback |
| CLI | `internal/plugins/cli/` | Unix socket listener + synchronous request-response |
| WebUI | `internal/plugins/webui/` | HTTP service + dependency injection |

---

*Want to understand the project goals? See [OVERVIEW.md](OVERVIEW.md).*
*Want to understand the architecture? See [ARCHITECTURE.md](ARCHITECTURE.md).*
*SDK repository and development tools? See [homeagent-sdk](https://gitcode.com/JianFeeeee/homeagent-sdk).*

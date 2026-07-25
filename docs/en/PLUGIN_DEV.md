**中文** | [English](../en/PLUGIN_DEV.md)

# HomeAgent Plugin Development Guide

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 1. Quick Start: Using the plugindev Toolchain

`plugindev` is the unified plugin development toolchain provided in the SDK repository, supporting both Go and Lua plugin types.

### Installation

```bash
cd homeagent-sdk/tools/plugindev
go build -o plugindev
# Add plugindev to PATH or use directly
```

### SDK Version Management

`plugindev sdk` manages local SDK versions:

```bash
plugindev sdk list       # list installed SDK versions
plugindev sdk current    # show current SDK version
plugindev sdk latest     # show latest available version
plugindev sdk install v0.7.1  # install a specific version
plugindev sdk use v0.7.1      # switch to a version
plugindev sdk path       # show current SDK path
```

SDK is stored at `~/.homeagent/plugindev/sdk/<version>/`; `plugindev init` reads the current SDK version for `go.mod`.

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
├── plg.json       — Plugin metadata (name, version, entry, target platforms)
├── plugin.go      — Plugin implementation (Plugin interface + NewPlugin export)
├── go.mod         — Go module definition
├── README.md      — Documentation
└── thirdpart/     — Optional external source code directory
```

C ABI bridge files (`z_bridge_gen.go` + `z_entry.c`) are auto-generated at build time.

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
1. Reads `plg.json` `targets` field to determine target platforms
2. Auto-generates C ABI bridge code (`z_bridge_gen.go` + `z_entry.c`)
3. **Go plugin**: Runs `go build -buildmode=c-shared` (produces `.so` / `.dylib` / `.dll`)
4. **Lua plugin**: Packages source code directly, no compilation needed
5. Generates `plugin.json` output manifest
6. Packages as `.hmap` distribution (zip format, containing `plugin.json` + binary)

### plg.json (project config) vs plugin.json (output manifest)

| File | Purpose | Key fields |
|------|---------|------------|
| `plg.json` | Project metadata, maintained by developer | `targets` — build targets (e.g. `"linux/amd64,windows/amd64"`) |
| `plugin.json` | Build artifact manifest, auto-generated | `entry` — entry filename; `platforms` — declared platforms |

Each target produces a separate `.hmap`; binary name by platform:

| Platform | Binary |
|----------|--------|
| Linux | `plugin.so` |
| macOS | `plugin.dylib` |
| Windows | `plugin.dll` |

### Multi-platform bundle: --bundle

```bash
plugindev build --bundle
```

Builds linux/amd64 + darwin/amd64 + windows/amd64 in one pass, producing a single `.hmap`
with all platform binaries. The output manifest includes a `platforms` field.
The kernel auto-selects the correct binary during installation.

Output in `dist/` directory:
```
dist/
├── myplugin_linux_amd64.hmap      # Single platform: Linux
├── myplugin_windows_amd64.hmap    # Single platform: Windows
├── myplugin_darwin_amd64.hmap     # Single platform: macOS
├── myplugin_bundle.hmap           # Multi-platform bundle
└── myplugin_lua.hmap              # Lua plugin
```

### Deployment

Install via PluginMgr HTTP API (three methods):

```bash
# 1. Install from URL (auto-cleanup)
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/myplugin.hmap"}'

# 2. Install from local path (keeps source file)
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'

# 3. Upload binary directly
curl -X POST http://127.0.0.1:9876/plugins \
  --data-binary @dist/myplugin.hmap
```

Reload plugins via `/api/v1/plugins/reload` or restart the kernel to activate.

Or upload via WebUI plugin management page.

---

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

`plugindev init` generates `plugin.go` with the `NewPlugin` export function directly,
which is the entry point when the kernel loads the plugin:

```go
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

At build time, `plugindev build` auto-generates C ABI bridge code (`z_bridge_gen.go` + `z_entry.c`),
shared by both Windows DLL and Linux/macOS .so builds. No manual bridge code needed.

### PluginSDK Core API

#### Tool Registration — Make your capabilities callable by LLM

```go
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "Query weather for a specified city",
    NoMemory:    false,                            // false=output participates in memory, true=skip
    // Cleaner:  func(output string) string {     // Optional: clean output before vector/jieba/distill
    //     return extractJSON(output, "content")
    // },
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

##### NoMemory and Cleaner

`NoMemory` and `Cleaner` are optional fields on `ToolDef` that control how tool output participates in the **memory computation layer** (vectorization, jieba tokenization, distillation):

- **`NoMemory`** (default `false`): When `true`, the tool's output is excluded from all memory computation (vector, tokenization, distillation), but the original text is preserved in Context and Document. LLM attention is unaffected. Use cases: `cmd_run` (unpredictable noise in command output), pure operation tools like file upload/delete.

- **`Cleaner`** (optional): A function `func(output string) string`. When set, the tool output is filtered through this function before participating in vectorization/jieba/distillation. Typical use: stripping SQL prefixes, extracting a `content` field from JSON. The original output is never modified — Cleaner only affects the computation layer input.

Decision matrix:

```
Tool output → valuable for LLM attention?
  ├── No  → NoMemory=true (output preserved, skipped in computation)
  └── Yes → Contains cleanable noise?
       ├── Yes → Cleaner filters before computation
       └── No  → Normal memory, no extra handling
```

> **Note**: `Cleaner` is a Go `func` type (`json:"-"`), cannot cross C ABI boundaries. Not available for Lua plugins or remote plugins.

#### Stage Hooks — Intervene in message processing flow

7 stages:

| Stage | Timing | Purpose |
|-------|--------|---------|
| `on_input` | Message just arrived at Agent | Blacklist, rate-limit, short-circuit |
| `pre_action` | About to call LLM | Inject context |
| `post_action` | LLM returned results | Modify output/tool list |
| `before_toolcall` | Before tool execution | Audit, reject, modify params |
| `after_toolcall` | After tool execution | Desensitize, rewrite results |
| `before_output` | Before output | Format adaptation, leak cleanup |
| `after_output` | After output | Statistics/logging |

```go
// Global: receive all stage events
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
	ctx.Lock()
	ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": "Injected context content",
	})
	ctx.Unlock()
	return nil
})

// Own tools only: only before_toolcall/after_toolcall for this plugin's tools
s.RegisterStage(sdk.StageBeforeToolcall, myHandler, sdk.StageScopeOwnTools)
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
// Normal delivery (processed in order)
s.InjectText(source, channel, text string)

// Interrupt delivery (can interrupt current LLM processing)
s.InjectInterruptText(source, channel, text string)

// No memory recording
s.InjectTextNoMemory(source, channel, text string)
```

#### Event Subscription

```go
import "gitcode.com/JianFeeeee/homeagent-sdk/sdk"

unsub := s.Events().Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
    log.Printf("Tool was called: %v", evt.Payload)
})
defer unsub()
```

#### Capability Access

```go
// Graph Memory (entity-relation store)
entities, relations, err := s.Memory().Recall([]string{"keyword"}, 2)

// Document Memory (vector store)
docs := s.DocMemory().Query("query text", 3)

// Knowledge
results, err := s.Knowledge().Search("query", 5)

// LLM source management
s.LLM().ListSources() // returns []string
s.LLM().SetSource("deepseek")
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

#### Input Delivery (built-in plugins)

```go
// Queued delivery (processed in order)
s.InjectInput(source, channel, eventType string, payload map[string]interface{})

// Synchronous delivery (waits for response)
resp := s.InjectInputSync(source, channel, eventType string, payload map[string]interface{})

// Interrupt delivery (can preempt current LLM processing)
s.InjectInterrupt(source, channel, eventType string, payload map[string]interface{})

// Synchronous text shortcuts
resp := s.InjectTextSync(source, channel, text string)
resp := s.InjectTextSyncNoMemory(source, channel, text string)

// Get output channel
outputCh := s.OutputChan()
```

> **Note**: `Subscribe`, `Publish`, `RegisterChannel`, `UnregisterChannel`, `ListChannels`, `InjectInput`, `InjectInputSync`, `InjectInterrupt`, `InjectTextSync`, `InjectTextSyncNoMemory`, `OutputChan` are only available in built-in plugins (`internal/sdk` package). External dynamic plugins should use the public APIs: `InjectText`, `InjectInterruptText`, `InjectTextNoMemory`.

---

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

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

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 5. Best Practices

1. `Start()` is non-blocking — start long tasks in goroutines, don't block Start
2. `Stop()` cleans up resources — close connections, stop goroutines, cancel subscriptions
3. Unique tool names — use plugin name prefix to avoid conflicts
4. When handler returns `error`, LLM will receive it and may retry
5. Use `InjectInterruptText` for interrupts, `InjectText` for normal delivery
6. Use `Settings().Get/Set` for config, don't hardcode
7. External Go plugins compile independently, not tied to kernel version; only built-in plugins need recompilation with kernel

---

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 6. Example Plugin Reference

### SDK Repository Examples (`homeagent-sdk/example/`)

| Example | Type | Features |
|---------|------|----------|
| [memo](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/memo) | Go | Memo management, PreAction injection + timed interrupt dual reminder |
| [files](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/files) | Go | File system operations, 4 write modes, sandbox isolation |
| [browser](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/browser) | Go | Web search + HTTP fetch (SSRF) + Chromium render (merged from web/webfetch) |
| [bili](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/bili) | Go | Bilibili video download (yt-dlp) |
| [qq](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/qq) | Go | NapCat OneBot integration, 17 tools |
| [editdoc](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/editdoc) | Go | Office document editing and format conversion |
| [a2a](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/a2a) | Go | Agent-to-Agent protocol |
| [ocr](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/ocr) | Go | Offline text recognition (Tesseract) |
| [sanitizer](https://gitcode.com/JianFeeeee/homeagent-sdk/tree/main/example/sanitizer) | Go | Output sanitizer filter |

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

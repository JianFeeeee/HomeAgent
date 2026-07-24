# HomeAgent SDK

Plugin development SDK for building intelligent plugins that interact with the HomeAgent platform.

## SDK API Surface

### Plugin Interface

Plugins implement the `Plugin` interface:

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### PluginSDK Methods

The SDK instance injected via `Start(sdk *PluginSDK)` provides:

| Category | Method | Description |
|----------|--------|-------------|
| Stage Hooks | `RegisterStage(stage, handler, scope...)` | Register stage callback; scope: `StageScopeGlobal` (all, default) or `StageScopeOwnTools` (own tools only) |
| Output Channel | `RegisterOutputChannel(name, caps, desc, handler)` | Register output channel with capability bitmask |
| Tool Registration | `RegisterTool(name, def, handler)` | Register a tool for LLM invocation |
| Plugin API | `RegisterPluginAPI(name)` | Register plugin API for inter-plugin access |
| Graph Memory | `Memory()` | Access graph memory API (entity-relation store) |
| Text Memory | `TextMemory()` | Access text memory API (chronological events) |
| Doc Memory | `DocMemory()` | Access document memory API (vector store) |
| Social Graph | `Social()` | Access social graph API (read-only for external plugins) |
| Knowledge | `Knowledge()` | Access knowledge base API |
| LLM | `LLM()` | Access LLM provider manager API |
| Settings | `Settings()` | Access settings API |
| Events | `Events()` | Access event subscriber (subscribe-only for external plugins) |
| Inject | `InjectText(source, channel, text)` / `InjectInterruptText(source, channel, text)` / `InjectTextNoMemory(source, channel, text)` | Inject text into the agent pipeline |
| Auto-Restart | `SetAutoRestart(enabled)` / `AutoRestart()` | Control automatic restart on crash |

### Stage Hooks

```go
// Listen to all stage events globally
sdk.RegisterStage(StagePreAction, func(ctx *StageContext) error { return nil })

// Listen only to this plugin's own tool calls (before_toolcall / after_toolcall only)
sdk.RegisterStage(StageBeforeToolcall, myHandler, StageScopeOwnTools)
```

### Output Channels

```go
sdk.RegisterOutputChannel("my-channel", CapText|CapFile, "channel description", handler)
```

The handler receives three arguments:
- `payload` (string) — message content. For `type=text` it's plain text, for `type=file/image` it's a URL
- `meta` (string) — optional JSON routing metadata (e.g. `{"group_id":123,"user_id":456}`)
- `type` (string) — content type enum (see below)

Capability flags:

| Flag | Value | Description |
|------|-------|-------------|
| `CapText` | 1 | Plain text output |
| `CapFile` | 2 | File output |
| `CapImage` | 4 | Image output |
| `CapAudio` | 8 | Audio output |
| `CapStructured` | 16 | Structured data output |

Type enum values:

| Value | Description |
|-------|-------------|
| `text` | plain text |
| `voice` / `audio` | audio/voice |
| `image` | image |
| `file` | file |

### IOInjector Channel Routing

| Method | Description |
|--------|-------------|
| `InjectText(source, channel, text)` | Inject text, record to memory, route to specified channel |
| `InjectInterruptText(source, channel, text)` | Inject interrupt text, interrupt current processing, route to specified channel |
| `InjectTextNoMemory(source, channel, text)` | Inject text without memory recording, route to specified channel |

`source` identifies the origin, `channel` specifies the target output channel.

### Triple Extended Fields

The Triple data structure includes additional fields:

- `Confidence` — confidence score (0.0–1.0)
- `SubjectType` — subject type
- `ObjectType` — object type

### New Constructor

`New()` is called by the kernel when loading a plugin. Plugin developers do not need to construct PluginSDK manually:

```go
func New(name string, sett SettingsAPI, regTool ToolRegistrar, regStage StageRegistrar, regAPI APIRegistrar, regOutput OutputChannelRegistrar) *PluginSDK
```

Plugin developers only need to implement the `Plugin` interface and export a `NewPlugin()` entry function.

## plugindev Toolchain

`plugindev` provides full development workflow support:

| Command | Description |
|---------|-------------|
| `plugindev init` | Initialize plugin project (generates plg.json, entry template) |
| `plugindev build` | Build plugin, output .hmap package |
| `plugindev clean` | Clean build artifacts |
| `plugindev debug` | Run plugin in local debug mode |

Supports both **Go** and **Lua** plugin languages.

### plg.json Manifest Format

```json
{
  "name": "my-plugin",
  "version": "1.0.0",
  "lang": "go",
  "entry": "main.go",
  "description": "Plugin description",
  "channels": ["my-channel"],
  "dependencies": {}
}
```

### .hmap Package Format

`.hmap` is a ZIP archive containing:

- `plugin.json` — plugin metadata
- `plugin.so` — Go compiled artifact (Linux)
- `plugin.dll` — Go compiled artifact (Windows)
- `main.lua` — Lua plugin entry (for Lua plugins)

## Plugin Lifecycle

### Start & Stop

- `Start(sdk *PluginSDK) error` — Plugin startup, receives SDK instance
- `Stop() error` — Plugin shutdown, release resources

### Auto-Restart

```go
sdk.SetAutoRestart(true)
// Query state
enabled := sdk.AutoRestart()
```

The platform automatically restarts the plugin on crash, ensuring service availability.

## Restricted SDK vs Full SDK

External plugins (third-party distribution) use a **restricted SDK** that only exposes a safe subset:

| Restricted API | Allowed Operations |
|----------------|-------------------|
| `SocialAPI` | Read-only: `GetPerson`, `GetTrait`, `GetRelations`, `GetNetwork`, `ListPersons` |
| `EventSubscriber` | Subscribe-only: `Subscribe` (no `Publish`) |

Internal plugins (platform built-in) have full SDK access including SocialAPI write operations and EventPublisher.

## Example Plugins

| Plugin | Description |
|--------|-------------|
| a2a | Agent-to-Agent protocol communication |
| bili | Bilibili data fetching |
| editdoc | Document editing |
| files | File management |
| memo | Memo/notes |
| ocr | Optical character recognition |
| qq | QQ messaging integration |
| sanitizer | Content sanitization/safety filtering |
| web | Web browsing and interaction |
| webfetch | Web content fetching |

## Building & Installing

### Build

```bash
plugindev build
```

Outputs a `.hmap` package to the project directory.

### Install

Via pluginmgr HTTP API:

```bash
curl -X POST http://<host>:<port>/api/plugins/install \
  -F "package=@my-plugin.hmap"
```

Or manually place the `.hmap` in the plugin directory and restart the platform.

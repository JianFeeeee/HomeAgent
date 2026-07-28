**中文** | [English](../en/ADAPTER.md)

# Lua Adapter — LLM Source Adaptation Guide

> **Kernel Internal Format Notice**: The CompletionRequest / CompletionResponse JSON formats described below are the kernel LLM adapter's **private internal wire protocol**.
> These types are defined as Go structs in `internal/agent/api/provider.go` and are **not exported as an external API**.
> This document publicly describes this format solely as the contract standard for Lua adapter scripts — users follow this documentation to write Lua scripts that integrate any LLM API source.

Each LLM API source corresponds to a Lua script, responsible for request transformation (kernel private format → API format) and response transformation (API format → kernel private format).

## Adapter Contract

The Lua script must return a table containing the following fields and functions:

```lua
local adapter = {}

-- Metadata
adapter.name = "my_provider"   -- Unique identifier, matches adapter field in config
adapter.version = "2.0.0"
adapter.endpoint = "/v1/chat/completions"   -- API path, appended to base_url
adapter.headers = {}                        -- Additional HTTP request headers

-- Request transformation: kernel private format → API format
function adapter.transform_request(raw_json)
    -- raw_json: Kernel's CompletionRequest JSON string (full fields below)
    -- Returns: JSON string to send to API
    return transformed_json
end

-- Response transformation: API format → kernel private format
function adapter.transform_response(raw_json)
    -- raw_json: API's raw response JSON string
    -- Returns: Unified CompletionResponse JSON string (full format below)
    return unified_json
end

-- Stream chunk transformation (optional)
function adapter.transform_stream_chunk(raw_line)
    -- raw_line: Raw JSON string after data: in SSE
    -- Returns: Unified StreamChunk JSON string (format below), return "" to skip this chunk
    return chunk_json
end

return adapter
```

## Kernel Private CompletionRequest Format (Go → Lua)

```json
{
  "model": "deepseek-v4-flash",
  "messages": [
    { "role": "system", "content": "You are an AI assistant" },
    { "role": "user", "content": "Hello" },
    { "role": "assistant", "content": "Hi!", "reasoning_content": "thinking...", "tool_calls": [ { "id": "call_xxx", "type": "function", "function": { "name": "get_weather", "arguments": "{\"city\": \"Beijing\"}" } } ] },
    { "role": "tool", "tool_call_id": "call_xxx", "content": "Weather: sunny" }
  ],
  "temperature": 0.7,
  "max_tokens": 4096,
  "stream": false,
  "tools": [ { "type": "function", "function": { "name": "get_weather", "description": "...", "parameters": { ... } } } ],
  "tool_choice": "auto",
  "disable_thinking": true
}
```

### Field Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | no | Model name, auto-filled by kernel from BaseConfig.Model |
| `messages` | array | yes | Message list (see Message below) |
| `temperature` | float | no | Sampling temperature, default 0.7 |
| `max_tokens` | int | no | Max generated tokens, default 4096 |
| `stream` | bool | no | Whether to stream output |
| `tools` | array | no | Tool definitions (OpenAI tools format) |
| `tool_choice` | string/object | no | Tool selection strategy: "auto" / "none" / { type: "function", function: { name: "..." } } |
| `disable_thinking` | bool | no | Disable CoT reasoning (for reasoning models like DeepSeek-R1) |

**Extra fields**: The kernel may merge additional keys into the top-level JSON object (via an internal ExtraBody mechanism not listed in this table). Lua scripts should pass through or handle these fields as needed.

### Message Object

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `role` | string | yes | Role: `system` / `user` / `assistant` / `tool` |
| `content` | string/array | no | Text content; for multimodal, can be an array of ContentBlock (see below) |
| `reasoning_content` | string | no | Chain-of-thought reasoning content (assistant only, if available) |
| `tool_call_id` | string | no | Tool call ID (tool role only, corresponding to assistant's tool_calls) |
| `tool_calls` | array | no | Tool call list (assistant role only) |

### ContentBlock Object (Multimodal Messages)

When `content` is an array, each element format:

```json
{ "type": "text", "text": "Describe the image" }
{ "type": "image_url", "image_url": { "url": "https://...", "detail": "auto" } }
{ "type": "audio_url", "audio_url": { "url": "https://..." } }
```

### ToolCall Object (in CompletionRequest messages)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string | yes | Unique tool call ID |
| `type` | string | yes | Always `"function"` |
| `function` | object | yes | Contains `name` (string) and `arguments` (JSON **string**, not an object!) |

**Note**: In CompletionRequest messages, tool_calls use the OpenAI wire format:
`{id, type, function: {name: string, arguments: string}}` — `arguments` is a **stringified JSON**, not an object.
The kernel's `Message.MarshalJSON` performs this conversion.

In the CompletionResponse (returned by `transform_response`), tool_calls use a **flat format** as documented in the next section.

## Kernel Private CompletionResponse Format (Lua → Go)

```json
{
  "content": "Response content",
  "reasoning_content": "Chain of thought",
  "finish_reason": "stop",
  "token_usage": { "prompt": 10, "completion": 20, "total": 30 },
  "tool_calls": [
    { "id": "call_xxx", "type": "function", "name": "tool_name", "arguments": { "key": "val" } }
  ]
}
```

### Field Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `content` | string | yes | Response text |
| `reasoning_content` | string | no | Chain-of-thought content (if returned by model) |
| `finish_reason` | string | no | Finish reason: `"stop"` / `"tool_calls"` / `"length"` etc. |
| `token_usage` | object | no | Token usage with `prompt` / `completion` / `total` (int) fields |
| `tool_calls` | array | no | Tool call list in **flat format**: `{id, type, name, arguments: {object}}`. This differs from the `function:{name, arguments:string}` wire format used inside CompletionRequest messages — do not confuse them. |

## Stream Chunk Format (StreamChunk)

`transform_stream_chunk` should return JSON in this format:

```json
{ "content": "delta text", "done": false }
{ "content": "", "done": true, "tool_call": { "id": "call_xxx", "type": "function", "name": "get_weather", "arguments": { "city": "Bei" } } }
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `content` | string | yes | Delta content for this chunk |
| `done` | bool | yes | Whether stream is finished |
| `tool_call` | object | no | Tool call delta (arguments may be partial/incomplete JSON) |

Return empty string `""` to skip the chunk.

## Lua VM Built-in Functions

`json.encode(table)` — Encode Lua table to JSON string

`json.decode(string)` — Decode JSON string to Lua table

`log(level, message)` — Output log (level: info/warn/error)

`http_get(url)` — Perform HTTP GET request, returns response body as string

`http_post(url, body)` — Perform HTTP POST request, returns response body as string

## Adapting Typical APIs

| API | endpoint | auth method | Format differences |
|-----|----------|-------------|-------------------|
| **OpenAI** | `/chat/completions` | `Authorization: Bearer <key>` | Standard OpenAI format |
| **DeepSeek** | `/chat/completions` | `Authorization: Bearer <key>` | OpenAI compatible, forces temperature=1 |
| **Anthropic** | `/v1/messages` | `x-api-key: <key>` | Messages API, system message separated, content as block array |
| **Gemini** | `/v1/models/{model}:generateContent` | `?key=<key>` or Bearer | contents/parts format, role uses model instead of assistant |
| **Mistral** | `/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI compatible |
| **Groq** | `/openai/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI compatible |
| **GitHub Models** | `/chat/completions` | `Authorization: Bearer <pat>` | OpenAI compatible |
| **Ollama** | `/api/chat` | None | Different options format |

## Steps to Add a New LLM Source

1. Write a Lua adapter script defining `transform_request` and `transform_response`
2. (Optional) Define `transform_stream_chunk` for streaming support
3. Place the script `<name>.lua` in the `adapters/` subdirectory under the data directory (i.e. `daemon.data_dir/adapters/`)
4. Reference the adapter in configuration: `"adapter": "<name>"` (must match `adapter.name` in the script)
5. No recompilation needed — the VM automatically scans and loads all `.lua` files from that directory at startup

> **Note**: Built-in adapters are located in `internal/lua/adapters/` and compiled into the binary.
> User-defined custom adapters **do not** need to go into the source directory — place them in `daemon.data_dir/adapters/`.
> If the same adapter name exists in both locations, the custom file takes precedence.

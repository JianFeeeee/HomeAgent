**中文** | [English](../zh/ADAPTER.md)

# Lua Adapter — LLM Source Adaptation Guide

Each LLM API source corresponds to a Lua script, responsible for request transformation (Go unified format → API format) and response transformation (API format → Go unified format).

## Adapter Contract

The Lua script must return a table containing the following fields and functions:

```lua
local adapter = {}

-- Metadata
adapter.name = "my_provider"   -- Unique identifier, matches adapter field in config
adapter.version = "2.0.0"
adapter.endpoint = "/v1/chat/completions"   -- API path, appended to base_url
adapter.headers = {}                        -- Additional HTTP request headers

-- Request transformation: Go → API
function adapter.transform_request(raw_json)
    -- raw_json: Go's CompletionRequest JSON string
    -- Returns: JSON string to send to API
    return transformed_json
end

-- Response transformation: API → Go
function adapter.transform_response(raw_json)
    -- raw_json: API's raw response JSON string
    -- Returns: Unified CompletionResponse JSON string
    -- Unified format:
    --   { content: "", finish_reason: "", token_usage: { prompt: N, completion: N, total: N }, tool_calls?: [...] }
    return unified_json
end

-- Stream chunk transformation (optional)
function adapter.transform_stream_chunk(raw_line)
    -- raw_line: JSON string after data: in SSE
    -- Returns: JSON of { content: "", done: bool }, return "" to skip this chunk
    return chunk_json
end

return adapter
```

## Unified CompletionRequest Format (Go → Adapter)

```json
{
  "model": "deepseek-v4-flash",
  "messages": [
    { "role": "system", "content": "..." },
    { "role": "user", "content": "..." },
    { "role": "assistant", "content": "...", "tool_calls": [...] }
  ],
  "temperature": 0.7,
  "max_tokens": 4096,
  "stream": false,
  "tools": [...],
  "tool_choice": "auto"
}
```

## Unified CompletionResponse Format (Adapter → Go)

```json
{
  "content": "Response content",
  "finish_reason": "stop",
  "token_usage": { "prompt": 10, "completion": 20, "total": 30 },
  "tool_calls": [
    { "id": "call_xxx", "type": "function", "name": "tool_name", "arguments": { "key": "val" } }
  ]
}
```

## Lua VM Built-in Functions

`json.encode(table)` — Encode Lua table to JSON string

`json.decode(string)` — Decode JSON string to Lua table

`log(level, message)` — Output log (level: info/warn/error)

## Adapting Typical APIs

| API | endpoint | auth method | Format differences |
|-----|----------|-------------|-------------------|
| **OpenAI** | `/chat/completions` | `Authorization: Bearer <key>` | Standard OpenAI format |
| **DeepSeek** | `/chat/completions` | `Authorization: Bearer <key>` | OpenAI compatible, forces temperature=0 |
| **Anthropic** | `/v1/messages` | `x-api-key: <key>` | Messages API, system message separated, content as block array |
| **Gemini** | `/v1/models/{model}:generateContent` | `?key=<key>` or Bearer | contents/parts format, role uses model instead of assistant |
| **Mistral** | `/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI compatible |
| **Groq** | `/openai/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI compatible |
| **GitHub Models** | `/chat/completions` | `Authorization: Bearer <pat>` | OpenAI compatible |
| **Ollama** | `/api/chat` | None | Different options format |

## Steps to Add a New Source

1. Create `<name>.lua` under `internal/lua/adapters/`
2. Script defines `transform_request` and `transform_response`
3. (Optional) Define `transform_stream_chunk` for streaming support
4. Build verification: `go build ./cmd/homed/`
5. Test verification: `go test ./...`

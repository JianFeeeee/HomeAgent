[English](../en/ADAPTER.md) | **中文**

<img src="../../branding/mascot-xiaozhai.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

# Lua Adapter — LLM 源适配指南

每个 LLM API 源对应一个 Lua 脚本，负责请求转换（Go 统一格式 → API 格式）和响应转换（API 格式 → Go 统一格式）。

## 适配器契约

Lua 脚本必须返回一个包含以下字段和函数的 table：

```lua
local adapter = {}

-- 元信息
adapter.name = "my_provider"   -- 唯一标识，与 config 中 adapter 字段一致
adapter.version = "2.0.0"
adapter.endpoint = "/v1/chat/completions"   -- API 路径，拼接到 base_url 后
adapter.headers = {}                        -- 额外 HTTP 请求头

-- 请求转换：Go → API
function adapter.transform_request(raw_json)
    -- raw_json: Go 的 CompletionRequest JSON 字符串
    -- 返回: 应发送给 API 的 JSON 字符串
    return transformed_json
end

-- 响应转换：API → Go
function adapter.transform_response(raw_json)
    -- raw_json: API 返回的原始 JSON 字符串
    -- 返回: 统一 CompletionResponse JSON 字符串
    -- 统一格式:
    --   { content: "", finish_reason: "", token_usage: { prompt: N, completion: N, total: N }, tool_calls?: [...] }
    return unified_json
end

-- 流式块转换（可选）
function adapter.transform_stream_chunk(raw_line)
    -- raw_line: SSE 中 data: 后的 JSON 字符串
    -- 返回: { content: "", done: bool } 的 JSON，返回 "" 表示跳过该 chunk
    return chunk_json
end

return adapter
```

## 统一 CompletionRequest 格式（Go → Adapter）

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

## 统一 CompletionResponse 格式（Adapter → Go）

```json
{
  "content": "回复内容",
  "finish_reason": "stop",
  "token_usage": { "prompt": 10, "completion": 20, "total": 30 },
  "tool_calls": [
    { "id": "call_xxx", "type": "function", "name": "tool_name", "arguments": { "key": "val" } }
  ]
}
```

## Lua VM 内置函数

`json.encode(table)` — 将 Lua table 编码为 JSON 字符串

`json.decode(string)` — 将 JSON 字符串解码为 Lua table

`log(level, message)` — 输出日志（level: info/warn/error）

`http_get(url)` — 发起 HTTP GET 请求，返回响应体字符串

`http_post(url, body)` — 发起 HTTP POST 请求，返回响应体字符串

## 适配典型 API

| API | endpoint | auth 方式 | 格式差异 |
|---|---|---|---|
| **OpenAI** | `/chat/completions` | `Authorization: Bearer <key>` | 标准 OpenAI 格式 |
| **DeepSeek** | `/chat/completions` | `Authorization: Bearer <key>` | OpenAI 兼容，强制 temperature=0 |
| **Anthropic** | `/v1/messages` | `x-api-key: <key>` | Messages API，system 消息分离，content 为 block 数组 |
| **Gemini** | `/v1/models/{model}:generateContent` | `?key=<key>` 或 Bearer | contents/parts 格式，role 用 model 而非 assistant |
| **Mistral** | `/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI 兼容 |
| **Groq** | `/openai/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI 兼容 |
| **GitHub Models** | `/chat/completions` | `Authorization: Bearer <pat>` | OpenAI 兼容 |
| **Ollama** | `/api/chat` | 无 | 不同的 options 格式 |

## 添加新源步骤

1. 在 `internal/lua/adapters/` 下创建 `<name>.lua`
2. 脚本定义 `transform_request` 和 `transform_response`
3. （可选）定义 `transform_stream_chunk` 支持流式
4. 编译验证：`go build ./cmd/homed/`
5. 测试验证：`go test ./...`

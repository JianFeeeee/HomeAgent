[English](../en/ADAPTER.md) | **中文**

# Lua Adapter — LLM 源适配指南

> **内核内部格式说明**：下文描述的 CompletionRequest / CompletionResponse JSON 格式是内核 LLM 适配器的**私有内部线缆协议**。
> 该格式以 Go 结构体定义在 `internal/agent/api/provider.go` 中，**不导出为外部 API**。
> 本文档公开此格式的唯一目的是作为 Lua 适配器脚本的契约标准——用户按照此文档编写 Lua 脚本，即可接入任意 LLM API 源。

每个 LLM API 源对应一个 Lua 脚本，负责请求转换（内核私有格式 → API 格式）和响应转换（API 格式 → 内核私有格式）。

## 适配器契约

Lua 脚本必须返回一个包含以下字段和函数的 table：

```lua
local adapter = {}

-- 元信息
adapter.name = "my_provider"   -- 唯一标识，与 config 中 adapter 字段一致
adapter.version = "2.0.0"
adapter.endpoint = "/v1/chat/completions"   -- API 路径，拼接到 base_url 后
adapter.headers = {}                        -- 额外 HTTP 请求头

-- 请求转换：内核私有格式 → API 格式
function adapter.transform_request(raw_json)
    -- raw_json: 内核 CompletionRequest 的 JSON 字符串（完整字段见下文）
    -- 返回: 应发送给 API 的 JSON 字符串
    return transformed_json
end

-- 响应转换：API 格式 → 内核私有格式
function adapter.transform_response(raw_json)
    -- raw_json: API 返回的原始 JSON 字符串
    -- 返回: 统一 CompletionResponse JSON 字符串（完整格式见下文）
    return unified_json
end

-- 流式块转换（可选）
function adapter.transform_stream_chunk(raw_line)
    -- raw_line: SSE 中 data: 后的原始 JSON 字符串
    -- 返回: 统一 StreamChunk JSON 字符串（格式见下文），返回 "" 表示跳过该 chunk
    return chunk_json
end

return adapter
```

## 内核私有 CompletionRequest 格式（Go → Lua）

```json
{
  "model": "deepseek-v4-flash",
  "messages": [
    { "role": "system", "content": "你是 AI 助手" },
    { "role": "user", "content": "你好" },
    { "role": "assistant", "content": "你好！", "reasoning_content": "思考过程...", "tool_calls": [ { "id": "call_xxx", "type": "function", "function": { "name": "get_weather", "arguments": "{\"city\": \"北京\"}" } } ] },
    { "role": "tool", "tool_call_id": "call_xxx", "content": "天气：晴" }
  ],
  "temperature": 0.7,
  "max_tokens": 4096,
  "stream": false,
  "tools": [ { "type": "function", "function": { "name": "get_weather", "description": "...", "parameters": { ... } } } ],
  "tool_choice": "auto",
  "disable_thinking": true
}
```

### 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model` | string | 否 | 模型名，内核用 adapter 所在源的 BaseConfig.Model 自动填充 |
| `messages` | array | 是 | 对话消息列表（详见下方 Message） |
| `temperature` | float | 否 | 采样温度，默认 0.7 |
| `max_tokens` | float | 否 | 最大生成 token 数，默认 4096 |
| `stream` | bool | 否 | 是否流式输出 |
| `tools` | array | 否 | 工具定义列表（OpenAI tools 格式） |
| `tool_choice` | string/object | 否 | 工具选择策略，"auto" / "none" / { type: "function", function: { name: "..." } } |
| `disable_thinking` | bool | 否 | 是否禁用 CoT 思考（适用于 DeepSeek-R1 等推理模型） |

**扩展字段**：内核可能会将不在此表中的额外键值对合并到顶层 JSON（通过内部 ExtraBody 机制），Lua 脚本应当透传或按需处理这些字段。

### Message 对象

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `role` | string | 是 | 角色：`system` / `user` / `assistant` / `tool` |
| `content` | string/array | 否 | 文本内容；多模态时可为 ContentBlock 数组（见下方） |
| `reasoning_content` | string | 否 | 思维链/推理内容（仅 assistant 角色，如有） |
| `tool_call_id` | string | 否 | 工具调用 ID（仅 tool 角色，与 assistant 的 tool_calls 对应） |
| `tool_calls` | array | 否 | 工具调用列表（仅 assistant 角色） |

### ContentBlock 对象（多模态消息）

当 `content` 为数组时，每个元素格式：

```json
{ "type": "text", "text": "描述图片" }
{ "type": "image_url", "image_url": { "url": "https://...", "detail": "auto" } }
{ "type": "audio_url", "audio_url": { "url": "https://..." } }
```

### ToolCall 对象（CompletionRequest messages 中）

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `id` | string | 是 | 工具调用唯一 ID |
| `type` | string | 是 | 固定为 `"function"` |
| `function` | object | 是 | 内含 `name` (string) 和 `arguments` (JSON 字符串，非对象！) |

**注意**：在 CompletionRequest 的 messages 中，tool_calls 使用的是 OpenAI 线缆格式：
`{id, type, function: {name: string, arguments: string}}`，其中 `arguments` 是 **JSON 字符串**（非对象），因为内核序列化时对 ToolCall 做了此转换。

Lua 适配器在 `transform_response` 中返回给内核的 CompletionResponse 则使用**扁平格式**（见下节）。

## 内核私有 CompletionResponse 格式（Lua → Go）

```json
{
  "content": "回复内容",
  "reasoning_content": "思维链内容",
  "finish_reason": "stop",
  "token_usage": { "prompt": 10, "completion": 20, "total": 30 },
  "tool_calls": [
    { "id": "call_xxx", "type": "function", "name": "tool_name", "arguments": { "key": "val" } }
  ]
}
```

### 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `content` | string | 是 | 回复文本内容 |
| `reasoning_content` | string | 否 | 思维链内容（如模型返回） |
| `finish_reason` | string | 否 | 结束原因：`"stop"` / `"tool_calls"` / `"length"` 等 |
| `token_usage` | object | 否 | Token 用量，含 `prompt` / `completion` / `total` 三个 int 字段 |
| `tool_calls` | array | 否 | 工具调用列表（扁平格式：`{id, type, name, arguments: {object}}`，与 CompletionRequest 中 messages 的 `function: {name, arguments: string}` 格式不同，请勿混淆） |

## 流式 StreamChunk 格式

Lua 的 `transform_stream_chunk` 应返回以下 JSON：

```json
{ "content": "增量文本", "done": false }
{ "content": "", "done": true, "tool_call": { "id": "call_xxx", "type": "function", "name": "get_weather", "arguments": { "city": "北" } } }
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `content` | string | 是 | 本轮增量的文本内容 |
| `done` | bool | 是 | 是否结束 |
| `tool_call` | object | 否 | 工具调用增量（部分调用时 `arguments` 可能为不完整 JSON） |

返回空字符串 `""` 表示跳过该 chunk。

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
| **DeepSeek** | `/chat/completions` | `Authorization: Bearer <key>` | OpenAI 兼容，强制 temperature=1 |
| **Anthropic** | `/v1/messages` | `x-api-key: <key>` | Messages API，system 消息分离，content 为 block 数组 |
| **Gemini** | `/v1/models/{model}:generateContent` | `?key=<key>` 或 Bearer | contents/parts 格式，role 用 model 而非 assistant |
| **Mistral** | `/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI 兼容 |
| **Groq** | `/openai/v1/chat/completions` | `Authorization: Bearer <key>` | OpenAI 兼容 |
| **GitHub Models** | `/chat/completions` | `Authorization: Bearer <pat>` | OpenAI 兼容 |
| **Ollama** | `/api/chat` | 无 | 不同的 options 格式 |

## 添加新 LLM 源步骤

1. 编写 Lua 适配器脚本，定义 `transform_request` 和 `transform_response` 函数
2. （可选）定义 `transform_stream_chunk` 支持流式
3. 将脚本文件 `<name>.lua` 放入数据目录下的 `adapters/` 文件夹中（即 `daemon.data_dir/adapters/`）
4. 在配置中引用该适配器：`"adapter": "<name>"`（与脚本中 `adapter.name` 一致）
5. 无需重新编译——VM 启动时自动扫描该目录并加载所有 `.lua` 文件

> **注意**：内置适配器存放在 `internal/lua/adapters/` 目录下，编译时嵌入二进制。
> 用户自定义适配器**不需要**放入源码目录，只需放入 `daemon.data_dir/adapters/` 即可。
> 同名适配器：自定义文件优先级高于内置文件。

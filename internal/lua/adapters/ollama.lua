local adapter = {}

adapter.name = "ollama"
adapter.version = "2.0.0"
adapter.endpoint = "/api/chat"
adapter.headers = {}

-- Ollama API 格式：{ model, messages, stream, options:{temperature,num_predict} }
function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    local ollama_req = {
        model = req.model or "llama3",
        stream = req.stream or false,
        options = {
            temperature = req.temperature or 0.7,
            num_predict = req.max_tokens or 2048
        }
    }

    -- 转换 messages 格式（Ollama 兼容 OpenAI 的 messages 格式）
    if req.messages then
        local msgs = {}
        for _, m in ipairs(req.messages) do
            table.insert(msgs, { role = m.role, content = m.content })
        end
        ollama_req.messages = msgs
    end

    return json.encode(ollama_req)
end

function adapter.transform_response(raw_body)
    local ok, resp = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    local unified = {
        content = "",
        finish_reason = resp.done_reason or "",
        tool_calls = {},
        usage = { prompt = 0, completion = 0, total = 0 }
    }

    if resp.message then
        unified.content = resp.message.content or ""
    end

    return json.encode(unified)
end

function adapter.transform_stream_chunk(raw_chunk)
    local ok, chunk = pcall(json.decode, raw_chunk)
    if not ok then return "" end
    if not chunk.message then return "" end

    return json.encode({
        content = chunk.message.content or "",
        done = chunk.done or false
    })
end

return adapter

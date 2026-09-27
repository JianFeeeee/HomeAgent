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

    -- 转换 messages 格式（Ollama messages 支持 images base64 数组）
    if req.messages then
        local msgs = {}
        for _, m in ipairs(req.messages) do
            local text, images
            if type(m.content) == "string" then
                text, images = m.content, nil
            else
                text = ""
                images = {}
                for _, p in ipairs(m.content or {}) do
                    if p.type == "text" then
                        text = text .. (p.text or "")
                    elseif p.type == "image_url" and type(p.image_url) == "table" and p.image_url.url then
                        local b64 = string.match(p.image_url.url, "^data:[^,]+;base64,(.+)$")
                        if b64 then table.insert(images, b64) end
                    end
                end
                if #images == 0 then images = nil end
            end
            local msg = { role = m.role, content = text }
            if images then msg.images = images end
            table.insert(msgs, msg)
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

    local unified = {
        content = chunk.message.content or "",
        done = chunk.done or false
    }
    if chunk.message.reasoning_content then
        unified.reasoning_content = chunk.message.reasoning_content
    end
    if chunk.message.tool_calls then
        local tools = {}
        for i, tc in ipairs(chunk.message.tool_calls) do
            -- ★ 必须是**扁平**结构（name / raw_arguments 在顶层）且键名是
            --   stream_index —— homed 的 agentAPI.ToolCall 按 json tag 反序列化：
            --   · 嵌套 ["function"]={...} ⇒ Go 侧取不到 name/raw_arguments（零值）
            --   · 键名写 index ⇒ StreamIndex 取零值 ⇒ 多个分片并到同一个桶，
            --     argsRaw 混拼 ⇒ 每个工具报"参数不是合法 JSON"而一个都没真跑
            --   两种都是**静默**失效，所以这里逐项对齐。
            local fn = tc["function"]
            local name = (type(fn) == "table" and fn.name) or tc.name or ""
            local args = "{}"
            if type(fn) == "table" and fn.arguments ~= nil then
                args = fn.arguments
            elseif type(tc.arguments) == "string" then
                args = tc.arguments
            end
            -- ollama 的 tool_calls **整条一次发完**（不分片），所以序号即下标
            table.insert(tools, {
                id = tc.id or ("call_" .. i),
                type = "function",
                name = name,
                raw_arguments = args,
                stream_index = (tc.index or (i - 1))
            })
        end
        unified.tool_calls = tools
    end
    return json.encode(unified)
end

return adapter

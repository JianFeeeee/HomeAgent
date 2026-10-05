local adapter = {}

adapter.name = "ollama"
adapter.version = "2.0.0"
adapter.endpoint = "/api/chat"
adapter.headers = {}

-- ── 用量归一化（transform_response 与 transform_stream_chunk 共用）──
--
-- 输出键名对齐 homed 的 agentAPI.TokenUsage（json tag）：
--   prompt / completion / total / cache_read / cache_reported 等，
-- 所以这张表会被 json.Unmarshal 直接吃进 StreamChunk.Usage。
--
-- ★ 为何必须透传：适配器是**归一化层**，上游给的用量只有它看得见。
--   不透传则内核只剩估算，永远答不出真实成本与缓存命中。
-- Ollama 的计量在**顶层**（不是嵌在 usage 对象里）：
--   prompt_eval_count → prompt，eval_count → completion
-- 且只在最后一帧给出，所以帧上没有这两个键时返回 nil（表示「本帧无用量」）。
local function usage_to_unified(u)
    if type(u) ~= "table" then return nil end
    local p = u.prompt_eval_count or 0
    local c = u.eval_count or 0
    if p == 0 and c == 0 then return nil end
    return { prompt = p, completion = c, total = p + c }
end

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
        -- ★ 键名必须是 token_usage：Go 侧 CompletionResponse 的 json tag 就是
        --   它。原先这里写的是 usage，于是这份用量**被静默忽略**（缺字段不报错，
        --   只是永远取零值）——又一个「算了却不返回」。
        token_usage = { prompt = 0, completion = 0, total = 0 }
    }
    local rusg = usage_to_unified(resp)
    if rusg then unified.token_usage = rusg end

    if resp.message then
        unified.content = resp.message.content or ""
    end

    return json.encode(unified)
end

function adapter.transform_stream_chunk(raw_chunk)
    local ok, chunk = pcall(json.decode, raw_chunk)
    if not ok then return "" end
    local usg = usage_to_unified(chunk)
    if not chunk.message then
        if usg then return json.encode({ done = chunk.done or false, usage = usg }) end
        return ""
    end

    local unified = {
        content = chunk.message.content or "",
        done = chunk.done or false
    }
    if usg then unified.usage = usg end
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

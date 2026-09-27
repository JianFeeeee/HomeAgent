local adapter = {}

adapter.name = "deepseek"
adapter.version = "2.1.0"
adapter.endpoint = "/chat/completions"
adapter.headers = {}

function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    req.model = req.model or "deepseek-chat"
    req.stream = req.stream or false
    if req.disable_thinking then
        req.extra_body = req.extra_body or {}
        req.extra_body.thinking = { type = "disabled" }
    end
    req.disable_thinking = nil
    return json.encode(req)
end

function adapter.transform_response(raw_body)
    local ok, resp = pcall(json.decode, raw_body)
    if not ok or resp == nil then return raw_body end

    local unified = {
        content = "",
        finish_reason = "",
        token_usage = { prompt = 0, completion = 0, total = 0 }
    }

    if type(resp.usage) == "table" then
        unified.token_usage.prompt = resp.usage.prompt_tokens or 0
        unified.token_usage.completion = resp.usage.completion_tokens or 0
        unified.token_usage.total = resp.usage.total_tokens or 0
    end

    if type(resp.choices) == "table" and #resp.choices > 0 then
        local ch = resp.choices[1]
        if type(ch.message) == "table" then
            unified.content = ch.message.content or ""
            if ch.message.reasoning_content then
                unified.reasoning_content = ch.message.reasoning_content
            end
            if type(ch.message.tool_calls) == "table" then
                local tcs = {}
                for _, tc in ipairs(ch.message.tool_calls) do
                    local fn = tc["function"]
                    local name = tc.name
                    local raw_args = tc.arguments
                    if type(fn) == "table" then
                        name = fn.name or name
                        raw_args = fn.arguments or raw_args
                    end
                    local args = {}
                    if type(raw_args) == "table" then
                        args = raw_args
                    elseif type(raw_args) == "string" and raw_args ~= "" then
                        local args_ok, decoded = pcall(json.decode, raw_args)
                        if args_ok and type(decoded) == "table" then
                            args = decoded
                        elseif args_ok then
                            args = { value = decoded }
                        else
                            args = { raw = raw_args }
                        end
                    end
                    if name ~= nil and name ~= "" then
                        table.insert(tcs, {
                            id = tc.id,
                            type = tc.type or "function",
                            name = name,
                            arguments = args
                        })
                    end
                end
                unified.tool_calls = tcs
            end
        end
        unified.finish_reason = ch.finish_reason or ""
    end

    return json.encode(unified)
end

function adapter.transform_stream_chunk(raw_chunk)
    local ok, chunk = pcall(json.decode, raw_chunk)
    if not ok then return "" end
    if not chunk.choices or #chunk.choices == 0 then return "" end
    local delta = chunk.choices[1].delta or {}
    local fr = chunk.choices[1].finish_reason
    local unified = {
        content = delta.content or "",
        done = (fr ~= nil)
    }
    if delta.reasoning_content then
        unified.reasoning_content = delta.reasoning_content
    end
    -- ★ 必须处理流式 tool_calls —— 此前只透 content/done，导致 deepseek 源
    --   在**流式**模式下工具调用全部丢失，模型调不动任何工具且无任何报错。
    --
    --   为什么难发现：非流式路径（transform_response）是好的，所以端到端
    --   手工测试也过；而内核的 tool call 循环默认走流式。
    --   功能判据（core 包的批内测试）直接构造 Go 结构体，绕过适配器。
    --
    -- 形态与 openai.lua 一致：OpenAI 兼容流式格式
    -- {function:{name,arguments}, id, type, index} → homed 扁平结构
    -- {id, type, name, raw_arguments, stream_index}。
    if delta.tool_calls then
        local tcs = {}
        for _, tc in ipairs(delta.tool_calls) do
            local fn = tc["function"]
            local name = (type(fn) == "table" and fn.name) or tc.name or ""
            local raw_args = ""
            if type(fn) == "table" and type(fn.arguments) == "string" then
                raw_args = fn.arguments
            elseif type(tc.arguments) == "string" then
                raw_args = tc.arguments
            end
            -- 不能按 name 过滤：流式续传片 name 为空但携带 arguments，
            -- 内核 accumulateStream 按 stream_index 分桶并累积
            table.insert(tcs, {
                id = tc.id or "",
                type = tc.type or "function",
                name = name,
                raw_arguments = raw_args,
                -- 透传上游分片 index：并行多工具调用时内核按它区分归属桶
                stream_index = tc.index or 0
            })
        end
        unified.tool_calls = tcs
    end
    return json.encode(unified)
end

return adapter

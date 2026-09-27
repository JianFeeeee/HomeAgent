local adapter = {}

adapter.name = "server"
adapter.version = "1.0.0"
adapter.endpoint = "/chat/completions"
adapter.headers = {}

-- 专用于 zen 兼容网关（thinking 模式要求回传 reasoning_content）。
-- 关键：不删除 disable_thinking（homeagent 置 true 时网关关闭 thinking，
-- 从而不再强制要求 reasoning_content 回传）；同时保留已有 reasoning_content 双保险。
function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end
    req.extra_body = nil
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
    if delta.tool_calls then
        local tcs = {}
        for _, tc in ipairs(delta.tool_calls) do
            -- OpenAI 流式格式: {function:{name,arguments}, id, type, index}
            -- homed StreamChunk.ToolCalls 期望扁平格式: {id, type, name, raw_arguments}
            local fn = tc["function"]
            local name = (type(fn) == "table" and fn.name) or tc.name or ""
            local raw_args = ""
            if type(fn) == "table" and type(fn.arguments) == "string" then
                raw_args = fn.arguments
            elseif type(tc.arguments) == "string" then
                raw_args = tc.arguments
            end
            -- 不能按 name 过滤：OpenAI 流式分片中后续块 name 为空但携带 arguments
            -- accumulateStream 按 index 累积并在 flushToolCall 时校验 name
            table.insert(tcs, {
                id = tc.id or "",
                type = tc.type or "function",
                name = name,
                raw_arguments = raw_args,
                -- ★ 必须透传上游 index（键名是 stream_index，不是 index）。
                --
                -- 内核按 stream_index 分桶累积同一轮多个 tool_call 的分片
                -- （process.go:347 `idx := tc.StreamIndex`）。缺了这一项，
                -- 所有分片的 StreamIndex 都是缺省 0 ⇒ 全部并进同一个桶 ⇒
                -- argsRaw 混拼 ⇒ 每个工具都报"参数不是合法 JSON"，
                -- 而工具一次都没真跑过。
                --
                -- 单工具调用时上游 index 恒为 0，缺省也是 0，所以这个问题
                -- 在生产上长期不显形 —— 直到模型一轮发多个工具才炸。
                --
                -- 续传分片（只有 arguments、没有 name）尤其依赖它：
                -- 那种分片除了 index 没有任何可归位的依据。
                stream_index = tc.index or 0
            })
        end
        unified.tool_calls = tcs
    end
    return json.encode(unified)
end

return adapter

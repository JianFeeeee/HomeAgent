local adapter = {}

adapter.name = "anthropic"
adapter.version = "2.0.0"
adapter.endpoint = "/v1/messages"
adapter.headers = {
    ["anthropic-version"] = "2023-06-01"
}

-- ── 用量归一化（transform_response 与 transform_stream_chunk 共用）──
--
-- ★ Anthropic 的计量口径与其他家**不一样**，这里必须算对：
--
--   input_tokens                = **不含**缓存读写的输入
--   cache_read_input_tokens     = 命中缓存、跳过计算的输入
--   cache_creation_input_tokens = 写入缓存的输入（是**写**，不算命中）
--   ⤷ 真实总输入 = input_tokens + cache_read + cache_creation
--
-- 直接拿 input_tokens 当 prompt 会**少算缓存那部分**（偏偏那是通常最大的那块），
-- 于是“输入很短”的错觉会让缓存收益看起来不存在。
--
-- 输出键名对齐 homed 的 agentAPI.TokenUsage（json tag），
-- 可直接被 json.Unmarshal 吃进 StreamChunk.Usage。
local function usage_to_unified(u)
    if type(u) ~= "table" then return nil end
    local inp = u.input_tokens or 0
    local cread = u.cache_read_input_tokens or 0
    local cwrite = u.cache_creation_input_tokens or 0
    local outp = u.output_tokens or 0
    local out = {
        -- 总量含缓存两部分：这样它与 OpenAI 系的 prompt_tokens 才是同一口径
        -- （OpenAI 的 prompt_tokens 本就含命中部分）。
        prompt = inp + cread + cwrite,
        completion = outp,
        total = inp + cread + cwrite + outp,
        cache_read = cread
    }
    -- ★只要上游给了 cache_read_input_tokens 字段（哪怕为 0）就算「报了缓存」。
    --   它必须与「根本没给」区分：前者是「这次没命中」，后者是「不知道」。
    if u.cache_read_input_tokens ~= nil then
        out.cache_reported = true
    end
    if cwrite > 0 then
        -- 缓存写入是未命中侧的开销（要花钱但不算 hit）。
        -- 命中数上不能把它算成 hit，否则命中率会被写缓存抬高。
        out.cache_miss = cwrite
    end
    return out
end

function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    -- 将 OpenAI 风格 content（字符串或 [{type:*}] 数组）拆成文本/图片块
    local function collect_blocks(content)
        if type(content) == "string" then
            return { { type = "text", text = content } }
        end
        local blocks = {}
        for _, p in ipairs(content or {}) do
            if p.type == "text" then
                table.insert(blocks, { type = "text", text = p.text })
            elseif p.type == "image_url" and type(p.image_url) == "table" and p.image_url.url then
                local mt, b64 = string.match(p.image_url.url, "^data:([^,]+);base64,(.+)$")
                if b64 then
                    table.insert(blocks, { type = "image", source = { type = "base64", media_type = mt or "image/png", data = b64 } })
                else
                    table.insert(blocks, { type = "image", source = { type = "url", url = p.image_url.url } })
                end
            end
        end
        return blocks
    end
    local function text_of(content)
        if type(content) == "string" then return content end
        local t = ""
        for _, p in ipairs(content or {}) do
            if p.type == "text" and p.text then t = t .. p.text end
        end
        return t
    end

    local msgs = {}
    local system = ""
    for _, m in ipairs(req.messages or {}) do
        if m.role == "system" then
            system = system .. text_of(m.content) .. "\n"
        else
            table.insert(msgs, { role = m.role, content = collect_blocks(m.content) })
        end
    end

    local anthropic_req = {
        model = req.model or "claude-sonnet-4-20250514",
        max_tokens = req.max_tokens or 4096,
        messages = msgs,
        stream = req.stream or false,
    }

    if not req.disable_thinking then
        anthropic_req.thinking = { type = "enabled", budget_tokens = 4096 }
    end

    if system ~= "" then
        anthropic_req.system = system
    end

    return json.encode(anthropic_req)
end

function adapter.transform_response(raw_body)
    local ok, resp = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    local unified = {
        content = "",
        finish_reason = "",
        token_usage = { prompt = 0, completion = 0, total = 0 }
    }

    if resp.usage then
        unified.token_usage = usage_to_unified(resp.usage)
    end

    if resp.content and #resp.content > 0 then
        for _, block in ipairs(resp.content) do
            if block.type == "text" then
                unified.content = unified.content .. (block.text or "")
            end
        end
    end
    unified.finish_reason = resp.stop_reason or ""

    return json.encode(unified)
end

function adapter.transform_stream_chunk(raw_chunk)
    local ok, chunk = pcall(json.decode, raw_chunk)
    if not ok then return "" end
    if chunk.type == "message_start" then
        -- ★ 不能整帧丢弃。这一帧携带着**最贵的两个数**：input_tokens 与
        --   cache_read_input_tokens —— prompt caching 的收益全在这里，
        --   而它们在流里**只会出现这一次**，丢了就再也拿不回来。
        --
        --   发空内容块是安全的：done=false 且无 finish_reason，
        --   既不会被 errorOnlyChunk 判为退化流（它要求 Done 且 finish_reason
        --   非空、且不是已知正常值），也不会在累积器里拼出任何内容。
        local u = usage_to_unified(chunk.message and chunk.message.usage)
        if u then
            return json.encode({ content = "", done = false, usage = u })
        end
        return ""
    end
    if chunk.type == "message_delta" then
        -- output_tokens 只在这一帧。返回非空 unified ⇒ Go 侧会采用适配器结果、
        -- 不再走回退解析 ⇒ 不透传就彻底没有。
        --
        -- 此处只带 completion；prompt/缓存由早先的 message_start 帧带出，
        -- 内核累积器按「非零字段赢」合并两帧（process.go 的 accumulateStream）。
        local unified = {
            content = "",
            done = (chunk.delta and chunk.delta.stop_reason ~= nil)
        }
        local u = usage_to_unified(chunk.usage)
        if u then unified.usage = u end
        return json.encode(unified)
    end
    if chunk.type == "content_block_start" and chunk.content_block
        and chunk.content_block.type == "tool_use" then
        -- first fragment of a tool call: emit index + id + name, empty args
        return json.encode({
            content = "", done = false,
            -- ★ 必须是**扁平**结构（name / raw_arguments 在顶层）且键名是
            --   stream_index —— homed 的 agentAPI.ToolCall 按 json tag 反序列化：
            --   · 嵌套 ["function"]={...} ⇒ Go 侧取不到 name/raw_arguments（取零值）
            --   · 键名写 index ⇒ StreamIndex 取零值 ⇒ 多个分片并到同一个桶，
            --     argsRaw 混拼 ⇒ 每个工具报"参数不是合法 JSON"而一个都没真跑
            --   两种形态都是**静默**失效，所以这里逐项对齐。
            tool_calls = { {
                id = chunk.content_block.id or "",
                type = "function",
                name = chunk.content_block.name or "",
                raw_arguments = "",
                stream_index = chunk.index or 0
            } }
        })
    end
    if chunk.type == "content_block_delta" and chunk.delta then
        if chunk.delta.type == "input_json_delta" then
            -- incremental JSON fragment; clients accumulate across chunks
            -- 同上：扁平 + stream_index。续传片 name 为空是正常的 ——
            -- 内核按 stream_index 累积，补齐 name 后才 flush。
            local unified = { content = "", done = false, tool_calls = { {
                id = "",
                type = "function",
                name = "",
                raw_arguments = chunk.delta.partial_json or "",
                stream_index = chunk.index or 0
            } } }
            return json.encode(unified)
        end
        if chunk.delta.type == "thinking_delta" and chunk.delta.thinking then
            return json.encode({ content = "", done = false, reasoning_content = chunk.delta.thinking })
        end
        return json.encode({ content = chunk.delta.text or "", done = false })
    end
    if chunk.type == "message_stop" then
        return json.encode({ content = "", done = true })
    end
    if chunk.type == "content_block_stop" then
        return json.encode({ content = "", done = false })
    end
    return ""
end

return adapter

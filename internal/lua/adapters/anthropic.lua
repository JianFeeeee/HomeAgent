local adapter = {}

adapter.name = "anthropic"
adapter.version = "2.0.0"
adapter.endpoint = "/v1/messages"
adapter.headers = {
    ["anthropic-version"] = "2023-06-01"
}

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
        unified.token_usage.prompt = resp.usage.input_tokens or 0
        unified.token_usage.completion = resp.usage.output_tokens or 0
        unified.token_usage.total = (resp.usage.input_tokens or 0) + (resp.usage.output_tokens or 0)
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
    if chunk.type == "message_start" then return "" end
    if chunk.type == "message_delta" then
        return json.encode({ content = "", done = (chunk.delta and chunk.delta.stop_reason ~= nil) })
    end
    if chunk.type == "content_block_start" and chunk.content_block
        and chunk.content_block.type == "tool_use" then
        -- first fragment of a tool call: emit index + id + name, empty args
        return json.encode({
            content = "", done = false,
            tool_calls = { {
                index = chunk.index or 0,
                id = chunk.content_block.id or "",
                type = "function",
                ["function"] = { name = chunk.content_block.name or "", arguments = "" }
            } }
        })
    end
    if chunk.type == "content_block_delta" and chunk.delta then
        if chunk.delta.type == "input_json_delta" then
            -- incremental JSON fragment; clients accumulate across chunks
            local unified = { content = "", done = false, tool_calls = { {
                index = chunk.index or 0,
                id = "",
                type = "function",
                ["function"] = { name = "", arguments = chunk.delta.partial_json or "" }
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

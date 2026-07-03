local adapter = {}

adapter.name = "anthropic"
adapter.version = "2.0.0"
adapter.endpoint = "/v1/messages"
adapter.headers = {
    ["anthropic-version"] = "2023-06-01"
}

-- Anthropic Messages API: { model, messages[], max_tokens, system, stream }
function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    local msgs = {}
    local system = ""
    for _, m in ipairs(req.messages or {}) do
        if m.role == "system" then
            system = system .. m.content .. "\n"
        else
            table.insert(msgs, { role = m.role, content = m.content })
        end
    end

    local anthropic_req = {
        model = req.model or "claude-sonnet-4-20250514",
        max_tokens = req.max_tokens or 4096,
        messages = msgs,
        stream = req.stream or false,
    }
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
    if chunk.type == "content_block_delta" and chunk.delta then
        return json.encode({ content = chunk.delta.text or "", done = false })
    end
    if chunk.type == "message_stop" then
        return json.encode({ content = "", done = true })
    end
    return ""
end

return adapter

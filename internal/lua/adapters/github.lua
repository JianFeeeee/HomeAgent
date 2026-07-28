local adapter = {}

adapter.name = "github"
adapter.version = "2.0.0"
adapter.endpoint = "/chat/completions"
adapter.headers = {}

-- GitHub Models: Azure-like endpoint, auth via Bearer token (PAT)
-- BaseURL example: https://models.inference.ai.azure.com
function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end
    req.model = req.model or "gpt-4o"
    req.temperature = req.temperature or 0.7
    req.max_tokens = req.max_tokens or 4096
    req.stream = req.stream or false
    req.disable_thinking = nil
    req.extra_body = nil
    return json.encode(req)
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
        unified.token_usage.prompt = resp.usage.prompt_tokens or 0
        unified.token_usage.completion = resp.usage.completion_tokens or 0
        unified.token_usage.total = resp.usage.total_tokens or 0
    end

    if resp.choices and #resp.choices > 0 then
        local ch = resp.choices[1]
        if ch.message then
            unified.content = ch.message.content or ""
            if ch.message.tool_calls then
                local tcs = {}
                for _, tc in ipairs(ch.message.tool_calls) do
                    local args_ok, args = pcall(json.decode, tc["function"].arguments)
                    if not args_ok then args = {} end
                    table.insert(tcs, {
                        id = tc.id,
                        type = tc.type or "function",
                        name = tc["function"].name,
                        arguments = args
                    })
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
    return json.encode({
        content = delta.content or "",
        done = (fr ~= nil)
    })
end

return adapter

local adapter = {}

adapter.name = "gemini"
adapter.version = "2.0.0"
adapter.endpoint = "/v1/models"
adapter.headers = {}

-- Gemini API: POST /v1/models/{model}:generateContent
-- Auth: API key in query param ?key=XXX or Authorization: Bearer XXX
function adapter.transform_request(raw_body)
    local ok, req = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    local contents = {}
    for _, m in ipairs(req.messages or {}) do
        table.insert(contents, {
            role = (m.role == "assistant") and "model" or m.role,
            parts = { { text = m.content } }
        })
    end

    local gemini_req = {
        contents = contents,
        generationConfig = {
            temperature = req.temperature or 0.7,
            maxOutputTokens = req.max_tokens or 4096,
        }
    }

    if req.stream then
        gemini_req.stream = true
    end

    return json.encode(gemini_req)
end

-- Gemini 的 endpoint 动态拼接：/v1/models/{model}:generateContent
function adapter.transform_response(raw_body)
    local ok, resp = pcall(json.decode, raw_body)
    if not ok then return raw_body end

    local unified = {
        content = "",
        finish_reason = "",
        token_usage = { prompt = 0, completion = 0, total = 0 }
    }

    if resp.usageMetadata then
        unified.token_usage.prompt = resp.usageMetadata.promptTokenCount or 0
        unified.token_usage.completion = resp.usageMetadata.candidatesTokenCount or 0
        unified.token_usage.total = resp.usageMetadata.totalTokenCount or 0
    end

    if resp.candidates and #resp.candidates > 0 then
        local cand = resp.candidates[1]
        if cand.content and cand.content.parts then
            for _, part in ipairs(cand.content.parts) do
                if part.text then
                    unified.content = unified.content .. part.text
                end
            end
        end
        if cand.finishReason then
            unified.finish_reason = cand.finishReason
        end
    end

    return json.encode(unified)
end

function adapter.transform_stream_chunk(raw_chunk)
    local ok, chunk = pcall(json.decode, raw_chunk)
    if not ok then return "" end

    if not chunk.candidates or #chunk.candidates == 0 then return "" end
    local cand = chunk.candidates[1]
    local content = ""
    if cand.content and cand.content.parts then
        for _, part in ipairs(cand.content.parts) do
            content = content .. (part.text or "")
        end
    end
    return json.encode({
        content = content,
        done = (cand.finishReason ~= nil)
    })
end

return adapter

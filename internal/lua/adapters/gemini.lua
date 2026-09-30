local adapter = {}

adapter.name = "gemini"
adapter.version = "2.0.0"
adapter.endpoint = "/v1/models"
adapter.headers = {}

-- ── 用量归一化（transform_response 与 transform_stream_chunk 共用）──
--
-- 输出键名对齐 homed 的 agentAPI.TokenUsage（json tag）：
--   prompt / completion / total / cache_read / cache_reported 等，
-- 所以这张表会被 json.Unmarshal 直接吃进 StreamChunk.Usage。
--
-- ★ 为何必须透传：适配器是**归一化层**，上游给的用量只有它看得见。
--   不透传则内核只剩估算，永远答不出真实成本与缓存命中。
local function usage_to_unified(u)
    if type(u) ~= "table" then return nil end
    local out = {
        prompt = u.promptTokenCount or 0,
        completion = u.candidatesTokenCount or 0,
        total = u.totalTokenCount or 0
    }
    -- Gemini 的 cachedContentTokenCount 就是命中缓存的输入 token 数，
    -- 对应 OpenAI 的 prompt_tokens_details.cached_tokens。
    if u.cachedContentTokenCount ~= nil then
        out.cache_read = u.cachedContentTokenCount
        -- 字段存在即「上游报了缓存」——即使为 0 也要标记，
        -- 否则「报了但没命中」会被当成「不知道」，界面显示成无数据。
        out.cache_reported = true
    end
    if out.total == 0 then out.total = out.prompt + out.completion end
    return out
end

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
        unified.token_usage = usage_to_unified(resp.usageMetadata)
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

    -- 用量可能出现在末帧（且那帧常常没有 candidates），同一趟里先取出来。
    local usg = usage_to_unified(chunk.usageMetadata)
    if not chunk.candidates or #chunk.candidates == 0 then
        if usg then return json.encode({ usage = usg }) end
        return ""
    end
    local cand = chunk.candidates[1]
    local content = ""
    if cand.content and cand.content.parts then
        for _, part in ipairs(cand.content.parts) do
            content = content .. (part.text or "")
        end
    end
    local out = {
        content = content,
        done = (cand.finishReason ~= nil)
    }
    if usg then out.usage = usg end
    return json.encode(out)
end

return adapter

local adapter = {}

adapter.name = "deepseek"
adapter.version = "1.0.0"

function adapter.transform_request(input)
    local messages = input.messages or {}
    local result = {
        model = input.model or "deepseek-chat",
        messages = messages,
        temperature = input.temperature or 0.0,
        max_tokens = input.max_tokens or 4096,
        stream = input.stream or false
    }
    return result
end

function adapter.transform_response(raw)
    return raw
end

return adapter

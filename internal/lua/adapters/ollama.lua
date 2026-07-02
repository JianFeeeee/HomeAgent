local adapter = {}

adapter.name = "ollama"
adapter.version = "1.0.0"

function adapter.transform_request(input)
    local messages = input.messages or {}
    local result = {
        model = input.model or "llama3",
        messages = messages,
        stream = input.stream or false,
        options = {
            temperature = input.temperature or 0.7,
            num_predict = input.max_tokens or 2048
        }
    }
    return result
end

function adapter.transform_response(raw)
    return raw
end

return adapter

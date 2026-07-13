-- HomeAgent Lua Plugin SDK
-- Interface contract between Lua plugins and HomeAgent kernel.
-- !impl functions are replaced by Go implementations at runtime.
-- Standalone/debug: pure Lua mock implementations are used.
-- Usage: local sdk = require("sdk")

sdk = {}

-- !impl
-- level: "debug" | "info" | "warn" | "error"
function sdk.log(level, msg)
    print("[lua-plugin] " .. tostring(level) .. ": " .. tostring(msg))
end

-- !impl
-- def: { description="...", parameters={...} }
-- handler: function(args) -> result
function sdk.register_tool(name, def, handler)
    print("[lua-plugin] register_tool: " .. tostring(name))
end

-- !impl
-- stage: "on_input" | "pre_action" | "post_action" | ...
function sdk.register_stage(stage, handler)
    print("[lua-plugin] register_stage: " .. tostring(stage))
end

-- !impl
function sdk.register_api(name)
    print("[lua-plugin] register_api: " .. tostring(name))
end

-- !impl
function sdk.get_setting(key)
    return nil
end

-- !impl
function sdk.set_setting(key, value)
    print("[lua-plugin] set_setting: " .. tostring(key))
end

-- !impl
function sdk.inject_text(source, channel, text)
    print("[lua-plugin] inject_text: " .. tostring(source) .. "/" .. tostring(channel))
end

-- !impl
function sdk.inject_interrupt(source, channel, text)
    print("[lua-plugin] inject_interrupt: " .. tostring(source))
end

-- !impl
function sdk.inject_text_no_memory(source, channel, text)
    print("[lua-plugin] inject_text_no_memory: " .. tostring(source))
end

-- json utils (pure Lua)
sdk.json = {}

function sdk.json.encode(val)
    local ok, result = pcall(function()
        local function _encode(v)
            local t = type(v)
            if t == "string" then
                local s = v:gsub('\\', '\\\\'):gsub('"', '\\"'):gsub('\n', '\\n'):gsub('\r', '\\r'):gsub('\t', '\\t')
                return '"' .. s .. '"'
            elseif t == "number" then
                return tostring(v)
            elseif t == "boolean" then
                return tostring(v)
            elseif t == "table" then
                local keys = {}
                local is_array = true
                local maxn = 0
                for k in pairs(v) do
                    keys[#keys + 1] = k
                    if type(k) ~= "number" or k < 1 or k ~= math.floor(k) then
                        is_array = false
                    end
                    if type(k) == "number" and k > maxn then maxn = k end
                end
                if is_array and #keys >= maxn then
                    local parts = {}
                    for i = 1, maxn do
                        parts[#parts + 1] = _encode(v[i])
                    end
                    return "[" .. table.concat(parts, ",") .. "]"
                else
                    local parts = {}
                    for _, k in ipairs(keys) do
                        parts[#parts + 1] = _encode(tostring(k)) .. ":" .. _encode(v[k])
                    end
                    return "{" .. table.concat(parts, ",") .. "}"
                end
            else
                return "null"
            end
        end
        return _encode(val)
    end)
    if ok then return result end
    return "null"
end

function sdk.json.decode(str)
    local ok, result = pcall(function()
        local pos, _end = 1, #str
        local function skip()
            while pos <= _end and str:sub(pos, pos):match("%s") do pos = pos + 1 end
        end
        local function parse()
            skip()
            if pos > _end then return nil end
            local c = str:sub(pos, pos)
            if c == '"' then
                local s = {}
                pos = pos + 1
                while pos <= _end do
                    local ch = str:sub(pos, pos)
                    if ch == '"' then
                        pos = pos + 1
                        return table.concat(s)
                    elseif ch == '\\' then
                        pos = pos + 1
                        local n = str:sub(pos, pos)
                        if n == '"' then s[#s+1] = '"'
                        elseif n == '\\' then s[#s+1] = '\\'
                        elseif n == '/' then s[#s+1] = '/'
                        elseif n == 'b' then s[#s+1] = '\b'
                        elseif n == 'f' then s[#s+1] = '\f'
                        elseif n == 'n' then s[#s+1] = '\n'
                        elseif n == 'r' then s[#s+1] = '\r'
                        elseif n == 't' then s[#s+1] = '\t'
                        elseif n == 'u' then
                            local hex = str:sub(pos+1, pos+4)
                            pos = pos + 4
                            s[#s+1] = utf8 and utf8.char(tonumber(hex, 16)) or '?'
                        end
                        pos = pos + 1
                    else
                        s[#s+1] = ch
                        pos = pos + 1
                    end
                end
                return table.concat(s)
            elseif c == 't' then pos = pos + 4; return true
            elseif c == 'f' then pos = pos + 5; return false
            elseif c == 'n' then pos = pos + 4; return nil
            elseif c == '{' then
                pos = pos + 1; skip()
                local t = {}
                if str:sub(pos, pos) == '}' then pos = pos + 1; return t end
                while true do
                    skip(); local k = parse(); skip()
                    if str:sub(pos, pos) == ':' then pos = pos + 1 end
                    skip(); t[k] = parse(); skip()
                    local sep = str:sub(pos, pos)
                    if sep == '}' then pos = pos + 1; return t end
                    if sep == ',' then pos = pos + 1 end
                end
            elseif c == '[' then
                pos = pos + 1; skip()
                local t = {}
                if str:sub(pos, pos) == ']' then pos = pos + 1; return t end
                local idx = 1
                while true do
                    skip(); t[idx] = parse(); idx = idx + 1; skip()
                    local sep = str:sub(pos, pos)
                    if sep == ']' then pos = pos + 1; return t end
                    if sep == ',' then pos = pos + 1 end
                end
            else
                local s, e = str:find('^[-%d%.eE]+', pos)
                if s then
                    local num = tonumber(str:sub(s, e))
                    pos = e + 1
                    return num
                end
                return nil
            end
        end
        return parse()
    end)
    if ok then return result end
    return nil
end

-- http utils
sdk.http = {}

-- !impl
function sdk.http.get(url)
    print("[lua-plugin] http.get: " .. tostring(url))
    return {status=200, body='{"mock":true}', headers={}}
end

-- !impl
function sdk.http.post(url, body, content_type)
    print("[lua-plugin] http.post: " .. tostring(url))
    return {status=200, body='{"mock":true}', headers={}}
end

return sdk

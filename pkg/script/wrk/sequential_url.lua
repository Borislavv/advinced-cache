-- sequential_url.lua

-- Диапазон ключей
local i_min, i_max = 1, 10000000
local i = i_min

-- Фиксированный язык
local language = "en"

-- Фиксированные значения
local domain = "1x001.com"
local project_id = "285"

-- Флаг для вывода первого запроса
local printed_first = false

request = function()
    local q =
        "?project[id]=" .. project_id ..
        "&domain=" .. domain ..
        "&language=" .. language ..
        "&choice[name]=betting" ..
        "&choice[choice][name]=betting_live" ..
        "&choice[choice][choice][name]=betting_live_null" ..
        "&choice[choice][choice][choice][name]=betting_live_null_" .. i ..
        "&choice[choice][choice][choice][choice][name]=betting_live_null_" .. i .. "_" .. i ..
        "&choice[choice][choice][choice][choice][choice][name]=betting_live_null_" .. i .. "_" .. i .. "_" .. i ..
        "&choice[choice][choice][choice][choice][choice][choice]=null"

    local path = "/api/v2/pagedata" .. q

--     -- Выводим первый запрос
--     if not printed_first then
--         print("[wrk] First URL: " .. path)
--         printed_first = true
--     end

--     -- Каждые 10_000 запросов логируем URL
--     if i % 10000 == 0 then
--         print("[wrk] URL at i=" .. i .. ": " .. path)
--     end

    -- Инкремент и цикл
    i = i + 1
    if i > i_max then
        i = i_min
    end

    return wrk.format("GET", path, {
        ["Host"] = "0.0.0.0:8020",
        ["Accept-Encoding"] = "gzip, deflate, br",
        ["Accept-Language"] = "en-US,en;q=0.9",
        ["Content-Type"] = "application/json"
    })
end

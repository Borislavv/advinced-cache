-- sequential_url.lua

-- Диапазоны
local sport_min, sport_max = 1, 1
local championship_min, championship_max = 1, 100
local match_min, match_max = 1, 100

-- Счётчики и параметры статистики
local sport, championship, match
local request_count = 0
local print_every = 10000

init = function()
    sport = sport_min
    championship = championship_min
    match = match_min
end

request = function()
    request_count = request_count + 1

    local q = "choice[name]=betting"
        .. "&choice[choice][name]=betting_live"
        .. "&choice[choice][choice][name]=betting_live_null"
        .. "&choice[choice][choice][choice][name]=betting_live_null_" .. sport
        .. "&choice[choice][choice][choice][choice][name]=betting_live_null_" .. sport .. "_" .. championship
        .. "&choice[choice][choice][choice][choice][choice][name]=betting_live_null_" .. sport .. "_" .. championship .. "_" .. match

    local path = "/api/v2/pagedata?language=en&domain=melbet-djibouti.com&timezone=3&project[id]=62&stream=homepage&" .. q

    if request_count % print_every == 0 then
        print("Request #" .. request_count .. ": " .. path)
    end

    match = match + 1
    if match > match_max then
        match = match_min
        championship = championship + 1
        if championship > championship_max then
            championship = championship_min
            sport = sport + 1
            if sport > sport_max then
                sport = sport_min
            end
        end
    end

    return wrk.format("GET", path)
end

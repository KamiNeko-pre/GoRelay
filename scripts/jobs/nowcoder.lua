-- Nowcoder (牛客网) job listing rule.
-- Reads the server-rendered window.__INITIAL_STATE__ JSON from the job center
-- listing, paginates the full result set, and emits golang-related positions
-- with salary, JD, city and HR-activity signals.

local MAX_PAGES = 10
local REQUEST_INTERVAL_MS = 1000
local MAX_ATTEMPTS = 2

local function retryable(error_text)
    local text = string.lower(tostring(error_text or ""))
    return text:find("状态异常: 502", 1, true)
        or text:find("状态异常: 503", 1, true)
        or text:find("状态异常: 504", 1, true)
        or text:find("timeout", 1, true)
        or text:find("connection reset", 1, true)
        or text:find("eof", 1, true)
end

local function controlled_get(url)
    local last_error = nil
    for attempt = 1, MAX_ATTEMPTS do
        local body, request_error = http_get_limited(url, REQUEST_INTERVAL_MS)
        if body then return body end
        last_error = request_error
        if attempt == MAX_ATTEMPTS or not retryable(request_error) then break end
        local _, sleep_error = sleep_ms(1000 * attempt)
        if sleep_error then return nil, sleep_error end
    end
    return nil, last_error
end

local GOLANG_WORDS = {
    "golang", "go语言", "go开发", "go 开发", "gopher", "go服务端", "go后端",
}

local function contains_any(hay, words)
    for _, w in ipairs(words) do
        if hay:find(w, 1, true) then return true end
    end
    return false
end

local function is_golang(title, keys, jd)
    if contains_any(title, GOLANG_WORDS) then return true end
    if contains_any(keys, GOLANG_WORDS) then return true end
    if keys:find("go", 1, true) then return true end
    if contains_any(jd, GOLANG_WORDS) then return true end
    if jd:find("java/go", 1, true) or jd:find("go/python", 1, true)
        or jd:find("/go", 1, true) or jd:find("go/", 1, true)
        or jd:find("go、", 1, true) or jd:find("，go", 1, true) then
        return true
    end
    return false
end

local function extract_state(html)
    local marker = "window.__INITIAL_STATE__="
    local pos = html:find(marker, 1, true)
    if not pos then return nil, "no __INITIAL_STATE__ marker" end
    local brace = html:find("{", pos + #marker, true)
    if not brace then return nil, "no JSON object start" end
    local term = html:find(";(function(", brace, true)
    if not term then return nil, "no state terminator" end
    local state, err = json_decode(html:sub(brace, term - 1))
    if err then return nil, "decode state: " .. err end
    return state
end

local function pick_store(state)
    local st = state.store
    if type(st) ~= "table" then return nil end
    if type(st.interCenter) == "table" and type(st.interCenter.jobList) == "table" then
        return st.interCenter
    end
    if type(st.fulltimeCenter) == "table" and type(st.fulltimeCenter.jobList) == "table" then
        return st.fulltimeCenter
    end
    return nil
end

local function fmt_salary(d)
    local stype = d.salaryType or 0
    local smin = d.salaryMin or 0
    local smax = d.salaryMax or 0
    local smonth = d.salaryMonth or 0
    if (smin == 0 and smax == 0) or smax >= 999999 then
        return "薪资面议", 0, 0, "面议"
    end
    local lo = math.floor(smin)
    local hi = math.floor(smax)
    if stype == 1 then
        return tostring(lo) .. "-" .. tostring(hi) .. "元/天", lo, hi, "日薪"
    end
    local suffix = ""
    if smonth and smonth > 0 then
        suffix = "·" .. tostring(math.floor(smonth)) .. "薪"
    end
    return tostring(lo) .. "-" .. tostring(hi) .. "K" .. suffix, lo, hi, "月薪"
end

local function hr_activity(d)
    local boss = d.apiSimpleBossUser
    local day = nil
    if type(boss) == "table" then day = boss.avgProcessDay end
    if day == nil then day = d.avgProcessDay end
    if day == nil then return "未知" end
    if day >= 999999 then return "不活跃" end
    if day <= 1 then return "非常活跃(1天内处理)" end
    if day <= 3 then return "活跃(3天内处理)" end
    if day <= 7 then return "较活跃(7天内处理)" end
    return "一般(" .. tostring(math.floor(day)) .. "天内处理)"
end

local function company_of(d)
    if d.companyNameText and d.companyNameText ~= "" then return d.companyNameText end
    local rc = d.recommendInternCompany
    if type(rc) == "table" and rc.companyName then return rc.companyName end
    return ""
end

local function company_meta(d)
    local rc = d.recommendInternCompany
    local scale, industry = "", ""
    if type(rc) == "table" then
        scale = rc.personScales or ""
        if type(rc.industryTagNameList) == "table" and #rc.industryTagNameList > 0 then
            industry = rc.industryTagNameList[1] or ""
        end
    end
    if industry == "" then industry = d.companyIndustryText or "" end
    return scale, industry
end

local function jd_text(d)
    local ext = d.ext
    if type(ext) ~= "string" or ext == "" then return "" end
    local jd, err = json_decode(ext)
    if err then return "" end
    local parts = {}
    if jd.infos and jd.infos ~= "" then parts[#parts + 1] = "岗位职责:\n" .. jd.infos end
    if jd.requirements and jd.requirements ~= "" then parts[#parts + 1] = "岗位要求:\n" .. jd.requirements end
    if jd.jobStrength and jd.jobStrength ~= "" then parts[#parts + 1] = "岗位亮点:\n" .. jd.jobStrength end
    return table.concat(parts, "\n\n")
end

local body, err = controlled_get(TARGET_URL)
if err then return false, "fetch: " .. err end

local state, serr = extract_state(body)
if serr then return false, serr end

local store = pick_store(state)
if not store then return false, "nowcoder: no populated job list in state" end

local total_page = math.floor(tonumber(store.totalPage) or 1)
if total_page < 1 then total_page = 1 end
if total_page > MAX_PAGES then total_page = MAX_PAGES end

local store_name = "unknown"
if TARGET_URL:find("/intern/", 1, true) then store_name = "intern" end
if TARGET_URL:find("/fulltime/", 1, true) then store_name = "fulltime" end

local function page_url(page)
    local sep = "?"
    if TARGET_URL:find("?", 1, true) then sep = "&" end
    return TARGET_URL .. sep .. "page=" .. page
end

local items = {}
local seen_total = 0
local golang_count = 0

for page = 1, total_page do
    local html = body
    if page > 1 then
        local h2, e2 = controlled_get(page_url(page))
        if e2 then break end
        html = h2
    end
    local st2, e3 = extract_state(html)
    if e3 then break end
    local store2 = pick_store(st2)
    if not store2 then break end
    local joblist = store2.jobList
    if type(joblist) ~= "table" then break end

    for _, entry in ipairs(joblist) do
        local d = entry.data
        if type(d) == "table" and (d.recruitType == 2 or d.recruitType == 3) then
            seen_total = seen_total + 1
            local title = d.jobName or ""
            local keys = d.jobKeys or ""
            local jdtxt = jd_text(d)
            if is_golang(string.lower(title), string.lower(keys), string.lower(jdtxt)) then
                golang_count = golang_count + 1
                local salary_text, salary_min, salary_max, salary_type = fmt_salary(d)
                local boss = d.apiSimpleBossUser
                local boss_day, boss_rate, boss_active = nil, nil, nil
                if type(boss) == "table" then
                    boss_day = boss.avgProcessDay
                    boss_rate = boss.avgProcessRate
                    boss_active = boss.activeTime
                end
                local scale, industry = company_meta(d)
                local jid = tostring(math.floor(d.id or 0))
                items[#items + 1] = {
                    platform = "nowcoder",
                    recruit_type = (d.recruitType == 2) and "实习" or "社招",
                    source_job_id = jid,
                    title = title,
                    job_url = "https://www.nowcoder.com/jobs/detail/" .. jid,
                    company = company_of(d),
                    city = d.jobCity or "",
                    salary_text = salary_text,
                    salary_min = salary_min,
                    salary_max = salary_max,
                    salary_type = salary_type,
                    description = jdtxt,
                    hr_activity = hr_activity(d),
                    hr_avg_process_day = boss_day,
                    hr_avg_process_rate = boss_rate,
                    hr_active_at_ms = boss_active,
                    refresh_at_ms = d.refreshTime,
                    tags = keys,
                    company_scale = scale,
                    company_industry = industry,
                }
            end
        end
    end
end

return true, {
    items = items,
    meta = {
        source = "nowcoder",
        platform = "nowcoder",
        store = store_name,
        scope = "golang 实习/社招",
        total_seen = seen_total,
        golang_count = golang_count,
        pages_crawled = total_page,
        rule_version = "1",
        url = TARGET_URL,
    },
}

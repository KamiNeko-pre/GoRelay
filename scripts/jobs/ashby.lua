local REQUEST_INTERVAL_MS = 750
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

local body, request_err = controlled_get(TARGET_URL)
if request_err then return false, request_err end

local payload, decode_err = json_decode(body)
if decode_err then return false, decode_err end
if type(payload.jobs) ~= "table" then return false, "Ashby response has no jobs array" end

local board_key = TARGET_URL:match("/job%-board/([^?]+)") or "unknown"
local items = {}
for _, job in ipairs(payload.jobs) do
    local compensation = job.compensation or {}
    local salary = {}
    for _, component in ipairs(compensation.summaryComponents or {}) do
        if component.compensationType == "Salary" then
            salary = component
            break
        end
    end

    items[#items + 1] = {
        ats_source = "ashby",
        board_key = board_key,
        source_job_id = tostring(job.id or job.jobUrl or ""),
        company_name = board_key,
        title = job.title or "",
        description = job.descriptionPlain or job.descriptionHtml or "",
        location_raw = job.location or "",
        department = job.department or "",
        team = job.team or "",
        employment_type = job.employmentType or "",
        workplace_type = job.workplaceType or "",
        is_remote = job.isRemote,
        salary_raw = compensation.scrapeableCompensationSalarySummary or compensation.compensationTierSummary,
        salary_min = salary.minValue,
        salary_max = salary.maxValue,
        salary_currency = salary.currencyCode,
        salary_interval = salary.interval,
        published_at = job.publishedAt,
        job_url = job.jobUrl or "",
        apply_url = job.applyUrl or "",
    }
end

return true, {
    items = items,
    meta = {ats_source = "ashby", board_key = board_key, count = #items, rule_version = "1"},
}

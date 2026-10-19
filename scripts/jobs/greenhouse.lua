-- The official Greenhouse board endpoint returns all open jobs as JSON,
-- including content. Parsing it directly preserves one complete-board task
-- and avoids treating an HTML selector miss as a successful empty board.
local REQUEST_INTERVAL_MS = 500
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

local board, board_err = controlled_get(TARGET_URL)
if board_err then return false, board_err end

local payload, decode_err = json_decode(board)
if decode_err then return false, decode_err end
if type(payload.jobs) ~= "table" then return false, "Greenhouse response has no jobs array" end

local board_key = TARGET_URL:match("/boards/([^/]+)/") or "unknown"
local items = {}
for _, job in ipairs(payload.jobs) do
    local location = job.location or {}
    items[#items + 1] = {
        ats_source = "greenhouse",
        board_key = board_key,
        source_job_id = tostring(job.id or job.absolute_url or ""),
        company_name = board_key,
        title = job.title or "",
        description = job.content or "",
        location_raw = location.name or "",
        department = job.departments and job.departments[1] and job.departments[1].name or "",
        published_at = job.updated_at,
        job_url = job.absolute_url or "",
    }
end

return true, {
    items = items,
    meta = {
        ats_source = "greenhouse",
        board_key = board_key,
        listed_count = #payload.jobs,
        collected_count = #items,
        detail_failures = 0,
        rule_version = "4",
    },
}

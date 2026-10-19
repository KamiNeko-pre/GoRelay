local body, err = http_get(TARGET_URL)
if err then return false, err end
local ids = html_attr_all(body, "tr.athing", "id")
local items = {}
for index, id in ipairs(ids or {}) do
    local selector = "tr[id='" .. id .. "']"
    local title = html_find(body, selector .. " .titleline > a")
    local links = html_attr_all(body, selector .. " .titleline > a", "href")
    local score = html_find(body, "#score_" .. id) or "0"
    local author = html_find(body, selector .. " + tr .hnuser") or ""
    local domain = html_find(body, selector .. " .sitestr") or "news.ycombinator.com"
    local comment_text = html_find(body, selector .. " + tr .subline > a:last-child") or ""
    if title and links and links[1] then
        items[#items + 1] = {
            id = id, title = title, url = url_resolve(TARGET_URL, links[1]),
            discussion_url = "https://news.ycombinator.com/item?id=" .. id,
            rank = index, points = tonumber((score:match("%d+"))) or 0,
            comments = tonumber((comment_text:match("%d+"))) or 0,
            author = author, domain = domain
        }
    end
end
if #items == 0 then return false, "Hacker News story rows were not found" end
return true, {items = items, meta = {source = "Hacker News", scope = "front page", url = TARGET_URL, rule_version = "4"}}

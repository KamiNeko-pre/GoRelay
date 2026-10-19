const API_ROOT = "/api/v1";

function createAPIError(response, body) {
  const message =
    body?.message || body?.error || `请求失败（${response.status}）`;
  const error = new Error(message);
  error.status = response.status;
  return error;
}

async function request(path, options = {}) {
  const response = await fetch(`${API_ROOT}${path}`, {
    ...options,
    signal: AbortSignal.timeout(20000),
    headers: { Accept: "application/json", "Content-Type": "application/json" },
  });
  let body = null;
  try {
    body = await response.json();
  } catch {
    body = null;
  }
  if (!response.ok) {
    throw createAPIError(response, body);
  }
  return body?.data ?? body;
}

export function createTask({ target, url, requestKey }) {
  return request("/tasks", {
    method: "POST",
    body: JSON.stringify({ target, url, request_key: requestKey }),
  });
}

const JD_RULE_IDS = [
  "nowcoder_jobs",
  "lever_jobs",
  "greenhouse_jobs",
  "ashby_jobs",
];

export function createJDBatch(rules, requestPrefix) {
  const rulesByID = new Map(rules.map((rule) => [rule.id, rule]));
  const selected = JD_RULE_IDS.map((id) => rulesByID.get(id));
  const unavailable = selected.filter((rule) => !rule?.script || !rule?.url);
  if (unavailable.length) {
    throw new Error("四个 JD 规则尚未全部就绪");
  }
  return Promise.allSettled(
    selected.map((rule) =>
      createTask({
        target: rule.id,
        url: rule.url,
        requestKey: `${requestPrefix}-${rule.id}`,
      }),
    ),
  );
}

export function fetchTask(id) {
  return request(`/tasks/${encodeURIComponent(id)}`);
}

function queryString(values) {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (
      value !== undefined &&
      value !== null &&
      value !== "" &&
      value !== "all"
    ) {
      params.set(key, value);
    }
  }
  return params.toString();
}

export async function fetchSnapshots({ ruleId, from, to, limit = 200 } = {}) {
  const query = queryString({ rule_id: ruleId, from, to, limit });
  const data = await request(`/analysis/snapshots${query ? `?${query}` : ""}`);
  if (Array.isArray(data)) {
    return data;
  }
  if (Array.isArray(data?.items)) {
    return data.items;
  }
  throw new Error("快照接口返回格式不正确");
}

/**
 * 递归分窗拉取全部已结束任务快照。
 * 后端单次最多返回 200 条；这里用 finished_at 时间窗向前翻页，
 * 直到取满 maxBatches 或没有更多数据。taskId 去重，避免同一时间戳边界重复。
 */
export async function fetchAllSnapshots({ limit = 200, maxBatches = 12 } = {}) {
  const seen = new Set();
  const collected = [];
  let to;
  for (let batch = 0; batch < maxBatches; batch += 1) {
    const records = await fetchSnapshots({ to, limit });
    if (!records.length) break;
    let added = 0;
    let oldest = null;
    for (const record of records) {
      // 注意：此处是 API 原始记录，字段为 snake_case（normalize 在 dataset 内做）
      const id = record?.task_id ?? record?.taskId;
      const finishedAt = record?.finished_at ?? record?.finishedAt;
      if (id === undefined || id === null || seen.has(id)) continue;
      seen.add(id);
      collected.push(record);
      added += 1;
      const at = Date.parse(finishedAt ?? "");
      if (Number.isFinite(at) && (oldest === null || at < oldest)) oldest = at;
    }
    if (!added) break; // 本批全部已见，已到时间边界
    if (oldest === null) break;
    if (records.length < limit) break; // 数据到底
    to = new Date(oldest).toISOString(); // 含边界时间，靠 taskId 去重兜底
  }
  return collected;
}

export async function fetchRules() {
  const data = await request("/analysis/rules");
  if (Array.isArray(data)) {
    return data;
  }
  if (Array.isArray(data?.items)) {
    return data.items;
  }
  throw new Error("规则接口返回格式不正确");
}

export async function fetchTaskEvents(taskId) {
  if (!taskId) {
    return [];
  }
  const data = await request(`/tasks/${encodeURIComponent(taskId)}/events`);
  if (Array.isArray(data)) {
    return data;
  }
  if (Array.isArray(data?.events)) {
    return data.events;
  }
  throw new Error("任务事件接口返回格式不正确");
}

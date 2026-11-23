import {
  normalizeSnapshot,
  readPath,
  compareSnapshots,
  evaluateRuleHealth,
  stableJSON,
} from "./analysis.js?v=20260911b";

export function numeric(value) {
  if (typeof value === "number") return Number.isFinite(value) ? value : null;
  if (typeof value !== "string" || !value.trim()) return null;
  const text = value.trim().replaceAll(",", "");
  return /^-?\d+(\.\d+)?$/.test(text) ? Number(text) : null;
}

export function field(item, rule, name) {
  return readPath(item, rule?.display?.[name] || name);
}

export function titleOf(item, rule) {
  return (
    field(item, rule, "title") ??
    item.title ??
    item.name ??
    item.full_name ??
    item.description ??
    item.url ??
    "未命名记录"
  );
}

export function flatten(value, prefix = "", result = {}) {
  for (const [key, child] of Object.entries(value ?? {})) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (child && typeof child === "object" && !Array.isArray(child))
      flatten(child, path, result);
    else result[path] = child;
  }
  return result;
}

export function differences(before, after) {
  const a = flatten(before),
    b = flatten(after);
  return [...new Set([...Object.keys(a), ...Object.keys(b)])]
    .filter((key) => stableJSON(a[key]) !== stableJSON(b[key]))
    .map((key) => ({ field: key, before: a[key], after: b[key] }));
}

export function dataset(records, rules, now = new Date()) {
  const snapshots = records
    .map((row) => (row.taskId === undefined ? normalizeSnapshot(row) : row))
    .sort(
      (a, b) =>
        Date.parse(b.finishedAt) - Date.parse(a.finishedAt) ||
        b.taskId - a.taskId,
    );
  // Unknown targets remain inspectable; only configured rules can be submitted.
  const catalog = [...rules];
  for (const s of snapshots)
    if (!catalog.some((rule) => rule.id === s.ruleId))
      catalog.push({
        id: s.ruleId,
        name: s.ruleId,
        source: s.meta?.source || s.ruleId,
        item_key: "url",
        display: {},
        schedule: "24h",
        min_items: 1,
      });
  const sources = catalog.map((rule) => {
    const runs = snapshots.filter((s) => s.ruleId === rule.id);
    const history = runs.filter(
      (s) => s.status === "succeeded" && !s.parseError,
    );
    const latest = history[0] ?? null;
    const previous =
      history[1] && history[1].meta?.rule_version === latest?.meta?.rule_version
        ? history[1]
        : null;
    const items = latest?.items ?? [];
    const comparison = previous
      ? compareSnapshots(previous, latest, rule)
      : null;
    const indexed = new Map(
      (previous?.items ?? []).map((item) => [
        String(readPath(item, rule.item_key)),
        item,
      ]),
    );
    const keys = items
      .map((item) => readPath(item, rule.item_key))
      .filter((key) => key !== null && key !== undefined && key !== "");
    const missingKeys = items.length - keys.length;
    const duplicates = keys.length - new Set(keys.map(String)).size;
    const rows = items.map((item, index) => {
      const key = readPath(item, rule.item_key);
      const before =
        key === undefined || key === null || key === ""
          ? null
          : indexed.get(String(key));
      const diff = before ? differences(before, item) : [];
      const score = numeric(field(item, rule, "score")),
        oldScore = numeric(field(before, rule, "score"));
      const rank = numeric(field(item, rule, "rank")),
        oldRank = numeric(field(before, rule, "rank"));
      return {
        item,
        before,
        diff,
        index,
        key,
        rule,
        taskId: latest.taskId,
        at: latest.finishedAt,
        score,
        rank,
        scoreDelta:
          score !== null && oldScore !== null ? score - oldScore : null,
        rankDelta: rank !== null && oldRank !== null ? oldRank - rank : null,
        change:
          !previous || missingKeys || duplicates
            ? "baseline"
            : !before
              ? "added"
              : diff.length
                ? "changed"
                : "stable",
      };
    });
    const health = evaluateRuleHealth(snapshots, rule, now);
    if (health.status === "healthy" && (missingKeys || duplicates))
      Object.assign(health, {
        status: "degraded",
        reason: `${missingKeys} 条缺少唯一键，${duplicates} 条键重复，差异分析不可靠`,
      });
    // 累计全量：跨该来源所有成功快照，按 item_key 去重，保留最新一次观测（history 已按时间倒序）
    const seenKeys = new Set();
    const allRows = [];
    for (const h of history) {
      h.items.forEach((item, index) => {
        const rawKey = readPath(item, rule.item_key);
        const key =
          rawKey === undefined || rawKey === null || rawKey === ""
            ? null
            : String(rawKey);
        if (key !== null) {
          if (seenKeys.has(key)) return;
          seenKeys.add(key);
        }
        allRows.push({
          item,
          rule,
          key,
          index,
          taskId: h.taskId,
          at: h.finishedAt,
          score: numeric(field(item, rule, "score")),
          rank: numeric(field(item, rule, "rank")) ?? index + 1,
          scoreDelta: null,
          rankDelta: null,
          before: null,
          diff: [],
          change: "baseline",
        });
      });
    }
    return {
      rule,
      runs,
      history,
      latest,
      previous,
      items,
      rows,
      allRows,
      comparison,
      health,
      missingKeys,
      duplicates,
    };
  });
  return {
    snapshots,
    sources,
    rows: sources.flatMap((s) => s.rows),
    allRows: sources.flatMap((s) => s.allRows),
    observations: sources.reduce(
      (n, s) => n + s.history.reduce((m, h) => m + h.items.length, 0),
      0,
    ),
  };
}

export function fieldProfile(items, path) {
  const values = items.map((item) => readPath(item, path));
  const present = values.filter(
    (v) => v !== null && v !== undefined && v !== "",
  );
  const numbers = present
    .map(numeric)
    .filter((v) => v !== null)
    .sort((a, b) => a - b);
  const counts = new Map();
  for (const v of present) {
    const text = typeof v === "object" ? stableJSON(v) : String(v);
    counts.set(text, (counts.get(text) || 0) + 1);
  }
  const numericField = numbers.length > 0 && numbers.length === present.length;
  return {
    path,
    total: items.length,
    present: present.length,
    missing: items.length - present.length,
    distinct: counts.size,
    numeric: numericField,
    min: numbers[0] ?? null,
    max: numbers.at(-1) ?? null,
    median: numbers.length
      ? (numbers[Math.floor((numbers.length - 1) / 2)] +
          numbers[Math.floor(numbers.length / 2)]) /
        2
      : null,
    average: numbers.length
      ? numbers.reduce((a, b) => a + b, 0) / numbers.length
      : null,
    distribution: [...counts].sort((a, b) => b[1] - a[1]).slice(0, 8),
  };
}

export function filterRows(
  rows,
  { query = "", change = "all", sort = "rank", page = 1, pageSize = 15 } = {},
) {
  const words = query.toLocaleLowerCase().trim().split(/\s+/).filter(Boolean);
  const selected = rows.filter(
    (row) =>
      (change === "all" || row.change === change) &&
      words.every((word) =>
        stableJSON(row.item).toLocaleLowerCase().includes(word),
      ),
  );
  selected.sort((a, b) => {
    if (sort === "title")
      return String(titleOf(a.item, a.rule)).localeCompare(
        String(titleOf(b.item, b.rule)),
      );
    const av = a[sort],
      bv = b[sort];
    if (av === null || av === undefined)
      return bv === null || bv === undefined ? 0 : 1;
    if (bv === null || bv === undefined) return -1;
    return sort === "rank" ? av - bv : bv - av;
  });
  const pages = Math.max(1, Math.ceil(selected.length / pageSize));
  const currentPage = Math.min(Math.max(page, 1), pages);
  return {
    all: selected,
    rows: selected.slice((currentPage - 1) * pageSize, currentPage * pageSize),
    total: selected.length,
    pages,
    page: currentPage,
  };
}

export function csv(rows) {
  const objects = rows.map((row) => ({
    source: row.rule.source,
    task_id: row.taskId,
    collected_at: row.at,
    ...flatten(row.item),
  }));
  const keys = [...new Set(objects.flatMap(Object.keys))];
  const cell = (value) => {
    let text =
      value === null || value === undefined
        ? ""
        : typeof value === "object"
          ? JSON.stringify(value)
          : String(value);
    if (/^[=+@\-\t\r]/.test(text)) text = "'" + text;
    return '"' + text.replaceAll('"', '""') + '"';
  };
  return (
    "\uFEFF" +
    [
      keys.map(cell).join(","),
      ...objects.map((row) => keys.map((key) => cell(row[key])).join(",")),
    ].join("\r\n")
  );
}

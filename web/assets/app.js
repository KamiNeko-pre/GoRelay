import { readPath, compareSnapshots } from "./analysis.js?v=20260911b";
import {
  dataset,
  field,
  titleOf,
  flatten,
  numeric,
  differences,
  fieldProfile,
  filterRows,
  csv,
} from "./workbench.js?v=20260911b";
import {
  fetchRules,
  fetchSnapshots,
  fetchAllSnapshots,
  fetchTaskEvents,
  createTask,
  createJDBatch,
  fetchTask,
} from "./api.js?v=20260911b";

const $ = (selector) => document.querySelector(selector);
const escape = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const number = (value) =>
  value === null || value === undefined
    ? "—"
    : new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(
        value,
      );
const date = (value) =>
  value
    ? new Date(value).toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        hour12: false,
      })
    : "—";
const icon = (name) => `<i data-lucide="${name}"></i>`;
const empty = (text, detail = "") =>
  `<div class="empty">${icon("inbox")}${escape(text)}${detail ? `<small>${escape(detail)}</small>` : ""}</div>`;
const colors = ["#2673df", "#17937d", "#c18a2d", "#af6195", "#7e87bf"];
const labels = {
  healthy: "健康",
  degraded: "需关注",
  failing: "异常",
  unknown: "未运行",
  succeeded: "成功",
  failed: "失败",
  added: "新出现",
  changed: "有变化",
  stable: "未变化",
  baseline: "基准快照",
  removed: "未再出现",
};
const views = {
  analysis: ["数据分析", "公开数据的持续观察与变化追踪"],
  compare: ["快照对比", "定位具体字段变化，回溯每一条数据"],
  health: ["规则健康", "从执行结果、数据质量和新鲜度检查采集规则"],
  runs: ["运行日志", "按任务回溯执行过程与失败原因"],
};
const state = {
  view: "analysis",
  source: "all",
  rules: [],
  data: dataset([], []),
  query: "",
  change: "all",
  tableScope: "all", // all=跨全部快照去重累计；latest=仅每来源最新快照
  sort: "rank",
  page: 1,
  pageSize: 15,
  rows: [],
  charts: new Map(),
  request: 0,
  detailRequest: 0,
  pending: new Map(),
  comparisonRows: [],
};

function icons() {
  window.lucide?.createIcons();
}
function selectedSources() {
  return state.source === "all"
    ? state.data.sources
    : state.data.sources.filter((s) => s.rule.id === state.source);
}
function currentSource() {
  return selectedSources().length === 1 ? selectedSources()[0] : null;
}
function statusTag(value) {
  return `<span class="tag ${escape(value)}">${escape(labels[value] || value)}</span>`;
}
function delta(value) {
  return value === null || value === undefined
    ? '<span class="neutral">—</span>'
    : `<span class="${value > 0 ? "positive" : value < 0 ? "negative" : "neutral"}">${value > 0 ? "+" : ""}${number(value)}</span>`;
}
function metric(label, value, detail, unit = "") {
  return `<div class="metric"><div class="metric-label">${escape(label)}</div><div class="metric-value">${escape(value)}${unit ? `<small>${escape(unit)}</small>` : ""}</div><div class="metric-detail">${escape(detail)}</div></div>`;
}
function sourceColor(id) {
  return colors[
    Math.max(
      0,
      state.data.sources.findIndex((s) => s.rule.id === id),
    ) % colors.length
  ];
}
function sourceChip(rule) {
  return `<span class="source-chip"><span class="source-dot" style="background:${sourceColor(rule.id)}"></span>${escape(rule.source)}</span>`;
}
function safeURL(value) {
  try {
    const url = new URL(value);
    return ["http:", "https:"].includes(url.protocol) ? url.href : null;
  } catch {
    return null;
  }
}
function setSelect(selector, options, value) {
  const el = $(selector);
  el.innerHTML = options
    .map(([v, text]) => `<option value="${escape(v)}">${escape(text)}</option>`)
    .join("");
  el.value = options.some(([v]) => String(v) === String(value))
    ? value
    : (options[0]?.[0] ?? "");
}
function banner(message, error = false) {
  const el = $(error ? "#error-banner" : "#notice");
  el.textContent = message;
  el.hidden = !message;
}

function renderNavigation() {
  $("#source-nav").innerHTML =
    `<button data-source="all" class="${state.source === "all" ? "active" : ""}">${icon("layers-2")}<span class="source-name">全部来源</span><span class="source-count">${number(state.data.rows.length)}</span></button>` +
    state.data.sources
      .map(
        (s) =>
          `<button data-source="${escape(s.rule.id)}" class="${state.source === s.rule.id ? "active" : ""}"><span class="source-dot" style="background:${sourceColor(s.rule.id)}"></span><span class="source-name">${escape(s.rule.source)}</span><span class="source-count">${number(s.items.length)}</span></button>`,
      )
      .join("");
  $("#sidebar-source-count").textContent = state.data.sources.length;
  $("#health-nav-count").textContent =
    state.data.sources.filter((s) =>
      ["failing", "degraded"].includes(s.health.status),
    ).length || "";
  setSelect(
    "#source-select",
    [
      ["all", "全部来源"],
      ...state.data.sources.map((s) => [s.rule.id, s.rule.name]),
    ],
    state.source,
  );
  $("#collect").disabled = state.rules.filter((rule) => rule.script && rule.url).length < 4;
}

function applyView() {
  const view = location.hash.slice(1).split("?")[0];
  state.view = views[view] ? view : "analysis";
  for (const key of Object.keys(views))
    $(`#${key}-view`).hidden = key !== state.view;
  document.querySelectorAll("[data-view]").forEach((el) => {
    el.classList.toggle("active", el.dataset.view === state.view);
    if (el.dataset.view === state.view) el.setAttribute("aria-current", "page");
    else el.removeAttribute("aria-current");
  });
  $("#page-title").textContent = views[state.view][0];
  $("#breadcrumb-view").textContent = views[state.view][0];
  $("#page-subtitle").textContent = views[state.view][1];
  render();
}

function chooseSource(id) {
  state.source = id;
  state.page = 1;
  state.change = "all";
  $("#trend-metric").value = "";
  $("#chart-mode").value = id === "all" ? "trend" : "ranking";
  $("#distribution-field").value = "";
  $("#compare-before").value = "";
  $("#compare-after").value = "";
  renderNavigation();
  render();
}

async function load() {
  const version = ++state.request;
  $("#refresh").disabled = true;
  document.body.classList.add("loading");
  try {
    const [rules, records] = await Promise.all([
      fetchRules(),
      fetchAllSnapshots({ limit: 200, maxBatches: 20 }),
    ]);
    if (version !== state.request) return;
    state.rules = rules;
    state.data = dataset(records, rules);
    if (
      state.source !== "all" &&
      !state.data.sources.some((s) => s.rule.id === state.source)
    )
      state.source = "all";
    banner("", true);
    $("#connection-dot").className = "status-dot online";
    $("#connection-state").textContent = "服务已连接";
    $("#last-sync").textContent =
      `同步于 ${new Date().toLocaleTimeString("zh-CN", { hour12: false })}`;
    $("#data-boundary").textContent =
      records.length >= 4000
        ? `已加载最近 ${records.length} 次运行，更早历史未展开`
        : `分析范围：全部 ${records.length} 次已结束运行（跨时间窗累计）`;
    renderNavigation();
    render();
  } catch (error) {
    if (version !== state.request) return;
    banner(
      `数据读取失败：${error.message}。${state.data.snapshots.length ? "当前保留上次成功读取的数据。" : "请确认服务和数据库正在运行。"}`,
      true,
    );
    $("#connection-dot").className = "status-dot error";
    $("#connection-state").textContent = "连接异常";
  } finally {
    if (version === state.request) {
      $("#refresh").disabled = false;
      document.body.classList.remove("loading");
      icons();
    }
  }
}

function render() {
  const sources = selectedSources();
  const runs = sources.flatMap((s) => s.runs);
  $("#scope-caption").textContent =
    `${sources.length} 个来源 · ${runs.length} 次运行 · 累计 ${number(state.data.observations ?? 0)} 次观测 · ${sources.reduce((n, s) => n + s.items.length, 0)} 条最新记录`;
  if (state.view === "analysis") {
    renderAnalysis();
    renderTable();
  }
  if (state.view === "compare") renderCompareOptions();
  if (state.view === "health") renderHealth();
  if (state.view === "runs") renderRuns();
  icons();
}

function chart(id, option, description) {
  const el = $(id);
  el.setAttribute("aria-label", description);
  if (!window.echarts) {
    el.innerHTML = empty("图表组件加载失败", "数据明细仍可查看");
    return;
  }
  let instance = state.charts.get(id);
  if (!instance) {
    el.innerHTML = "";
    instance = window.echarts.init(el, null, { renderer: "canvas" });
    state.charts.set(id, instance);
  }
  instance.off("click");
  instance.setOption(
    {
      animation: !matchMedia("(prefers-reduced-motion: reduce)").matches,
      animationDuration: 250,
      color: colors,
      textStyle: {
        fontFamily: "Segoe UI,Microsoft YaHei,sans-serif",
        fontSize: 10,
      },
      tooltip: { trigger: "axis", confine: true, renderMode: "richText" },
      ...option,
    },
    true,
  );
  instance.resize();
}
function emptyChart(id, text) {
  state.charts.get(id)?.dispose();
  state.charts.delete(id);
  $(id).innerHTML = `<div class="chart-empty">${escape(text)}</div>`;
  $(id).setAttribute("aria-label", text);
}
const axes = {
  axisLine: { show: false },
  axisTick: { show: false },
  axisLabel: { color: "#74808e", fontSize: 10 },
  splitLine: { lineStyle: { color: "#edf0f3", type: "dashed" } },
};

function renderAnalysis() {
  const sources = selectedSources(),
    source = currentSource(),
    rows = sources.flatMap((s) => s.rows),
    runs = sources.flatMap((s) => s.runs);
  const success = runs.filter((r) => r.status === "succeeded").length;
  const changes = rows.filter((r) => r.change === "changed").length,
    added = rows.filter((r) => r.change === "added").length;
  const valid =
    rows.length - sources.reduce((n, s) => n + s.missingKeys + s.duplicates, 0);
  const comparisons = sources.filter((s) => s.previous).length;
  $("#metrics").innerHTML =
    metric(
      "累计观测",
      number(state.data.observations ?? 0),
      `${sources.filter((s) => s.latest).length} 个来源 · 最新快照 ${number(rows.length)} 条记录 · ${success} 次成功运行`,
      "条",
    ) +
    metric(
      "相邻快照变化",
      comparisons ? number(changes + added) : "—",
      comparisons
        ? `${added} 条新出现 · ${changes} 条字段变化`
        : "等待第二次成功采集",
      "条",
    ) +
    metric(
      "任务成功率",
      runs.length ? number((success / runs.length) * 100) : "—",
      `${success} 次成功 / ${runs.length} 次运行`,
      "%",
    ) +
    metric(
      "可识别数据",
      rows.length ? number((valid / rows.length) * 100) : "—",
      `${rows.length - valid} 条缺键或重复 · 用于快照匹配`,
      "%",
    );
  const growth = source?.rows
    .filter((row) => row.scoreDelta > 0)
    .sort((a, b) => b.scoreDelta - a.scoreDelta)[0];
  let insight = rows.length
    ? `跨 ${sources.filter((s) => s.latest).length} 个来源累计观测 ${number(state.data.observations ?? 0)} 条；当前最新快照 ${rows.length} 条记录，最近相邻快照中 ${changes} 条字段变化、${added} 条新出现；${sources.filter((s) => s.health.status === "healthy").length} 个来源的数据健康。`
    : "还没有成功采集数据，暂无观测结论。";
  if (source) {
    insight = growth
      ? `${titleOf(growth.item, source.rule)} 的 ${source.rule.display?.score_label || "指标"} 增加 ${number(growth.scoreDelta)}，为本来源相邻快照最大增量。`
      : source.previous
        ? `${source.rule.name}：${added} 条新出现、${changes} 条字段变化、${source.comparison.removed.length} 条未再出现。`
        : `${source.rule.name} 已建立 ${rows.length} 条记录的基准快照，下一次成功采集后可计算变化。`;
  }
  $("#insight").innerHTML =
    icon("scan-line") + `<span>${escape(insight)}</span>`;
  const numericPaths = source
    ? [
        ...new Set(source.items.flatMap((item) => Object.keys(flatten(item)))),
      ].filter((path) => fieldProfile(source.items, path).numeric)
    : [];
  for (const option of $("#chart-mode").options)
    option.disabled = option.value !== "trend" && !source;
  if (!source) $("#chart-mode").value = "trend";
  const mode = $("#chart-mode").value;
  const metricOptions = [
    ...(mode === "trend" ? [["count", "记录数"]] : []),
    ...numericPaths.map((path) => [
      path,
      path === source?.rule.display?.score
        ? source.rule.display.score_label || path
        : path,
    ]),
  ];
  setSelect(
    "#trend-metric",
    metricOptions,
    $("#trend-metric").value || source?.rule.display?.score || "count",
  );
  $("#second-metric").hidden = mode !== "scatter";
  if (source && mode !== "trend") {
    renderSourceChart(source, numericPaths, mode);
    renderDistribution(source, sources);
    return;
  }
  $("#main-chart-title").textContent = "采集观测趋势";
  const metricName = $("#trend-metric").value;
  const series = sources
    .filter((s) => s.history.length)
    .map((s) => ({
      name: s.rule.source,
      type: "line",
      symbol: "circle",
      symbolSize: 7,
      showSymbol: true,
      connectNulls: false,
      lineStyle: { width: 2 },
      itemStyle: { color: sourceColor(s.rule.id) },
      data: [...s.history]
        .reverse()
        .map((h) => [
          Date.parse(h.finishedAt),
          metricName === "count"
            ? h.items.length
            : h.meta?.rule_version === s.latest?.meta?.rule_version
              ? fieldProfile(h.items, metricName).median
              : null,
        ]),
    }));
  $("#trend-caption").textContent =
    metricName === "count"
      ? "每个点是一份完整快照，重复采集不累加"
      : "相同来源、相同指标的快照中位数";
  $("#trend-footer").textContent =
    `${sources.reduce((n, s) => n + s.history.length, 0)} 份成功快照 · 时间为本地时区`;
  if (series.length)
    chart(
      "#trend-chart",
      {
        legend: {
          bottom: 0,
          icon: "circle",
          itemWidth: 7,
          itemHeight: 7,
          textStyle: { fontSize: 10, color: "#627181" },
        },
        grid: { top: 15, right: 22, bottom: 54, left: 45 },
        xAxis: {
          ...axes,
          type: "time",
          splitLine: { show: false },
          axisLabel: {
            ...axes.axisLabel,
            hideOverlap: true,
            formatter: (value) =>
              new Date(value).toLocaleTimeString("zh-CN", {
                hour: "2-digit",
                minute: "2-digit",
                hour12: false,
              }),
          },
        },
        yAxis: {
          ...axes,
          type: "value",
          minInterval: metricName === "count" ? 1 : undefined,
        },
        series,
      },
      `${sources.map((s) => s.rule.source).join("、")}的${metricName === "count" ? "记录数" : "指标中位数"}随快照变化`,
    );
  else emptyChart("#trend-chart", "尚无成功快照");
  renderDistribution(source, sources);
}

function renderSourceChart(source, paths, mode) {
  const x = $("#trend-metric").value;
  $("#main-chart-title").textContent =
    mode === "ranking" ? "指标排行" : "指标关联";
  if (!paths.length) {
    emptyChart("#trend-chart", "没有可分析的数值字段");
    $("#trend-footer").textContent = "";
    return;
  }
  const rows = source.rows.filter(
    (row) => numeric(readPath(row.item, x)) !== null,
  );
  if (mode === "ranking") {
    const top = [...rows]
      .sort(
        (a, b) => numeric(readPath(b.item, x)) - numeric(readPath(a.item, x)),
      )
      .slice(0, 8);
    $("#trend-caption").textContent = `${x} · 最新快照 Top ${top.length}`;
    chart(
      "#trend-chart",
      {
        grid: { left: 8, right: 55, top: 8, bottom: 25, containLabel: true },
        xAxis: { ...axes, type: "value" },
        yAxis: {
          ...axes,
          type: "category",
          inverse: true,
          data: top.map((row) => titleOf(row.item, source.rule)),
          axisLabel: { ...axes.axisLabel, width: 175, overflow: "truncate" },
          splitLine: { show: false },
        },
        series: [
          {
            type: "bar",
            barMaxWidth: 17,
            itemStyle: {
              color: sourceColor(source.rule.id),
              borderRadius: [0, 3, 3, 0],
            },
            data: top.map((row) => numeric(readPath(row.item, x))),
            label: {
              show: true,
              position: "right",
              fontSize: 10,
              color: "#526170",
              formatter: (p) => number(p.value),
            },
          },
        ],
      },
      `${source.rule.source} 的 ${x} 排行`,
    );
    state.charts.get("#trend-chart")?.off("click");
    state.charts.get("#trend-chart")?.on("click", (p) => {
      if (top[p.dataIndex]) openRow(top[p.dataIndex]);
    });
    $("#trend-footer").textContent =
      `${rows.length} 条记录参与排序 · 中位数 ${number(fieldProfile(source.items, x).median)}`;
  } else {
    setSelect(
      "#second-metric",
      paths.map((path) => [path, path]),
      $("#second-metric").value ||
        paths.find((path) => path !== x && path !== "rank" && path !== "id") ||
        paths[0],
    );
    const y = $("#second-metric").value,
      points = rows.filter((row) => numeric(readPath(row.item, y)) !== null);
    $("#trend-caption").textContent = `横轴 ${x} · 纵轴 ${y}`;
    chart(
      "#trend-chart",
      {
        grid: { left: 55, right: 25, top: 15, bottom: 35 },
        tooltip: {
          trigger: "item",
          confine: true,
          renderMode: "richText",
          formatter: (p) =>
            `${p.data.name}\n${x}: ${number(p.value[0])}\n${y}: ${number(p.value[1])}`,
        },
        xAxis: { ...axes, type: "value", scale: true },
        yAxis: { ...axes, type: "value", scale: true },
        series: [
          {
            type: "scatter",
            symbolSize: 10,
            itemStyle: { color: sourceColor(source.rule.id), opacity: 0.75 },
            data: points.map((row) => ({
              name: String(titleOf(row.item, source.rule)),
              value: [
                numeric(readPath(row.item, x)),
                numeric(readPath(row.item, y)),
              ],
            })),
          },
        ],
      },
      `${source.rule.source} ${x} 与 ${y} 的散点分布`,
    );
    state.charts.get("#trend-chart")?.off("click");
    state.charts.get("#trend-chart")?.on("click", (p) => {
      if (points[p.dataIndex]) openRow(points[p.dataIndex]);
    });
    $("#trend-footer").textContent =
      `${points.length} 条完整记录 · 仅展示指标分布，不推断因果关系`;
  }
}

function renderDistribution(source, sources) {
  const selector = $("#distribution-field");
  selector.hidden = !source;
  if (!source) {
    $("#distribution-title").textContent = "来源构成";
    $("#distribution-caption").textContent = "每个来源最新快照的记录数";
    const data = sources
      .filter((s) => s.items.length)
      .map((s) => ({
        name: s.rule.source,
        value: s.items.length,
        itemStyle: { color: sourceColor(s.rule.id) },
      }));
    if (data.length)
      chart(
        "#distribution-chart",
        {
          tooltip: { trigger: "item", confine: true, renderMode: "richText" },
          legend: {
            orient: "vertical",
            right: 0,
            top: "center",
            icon: "circle",
            itemWidth: 8,
            itemHeight: 8,
            textStyle: { fontSize: 10, color: "#526170" },
          },
          series: [
            {
              type: "pie",
              radius: ["47%", "72%"],
              center: ["34%", "46%"],
              avoidLabelOverlap: true,
              label: { show: false },
              emphasis: {
                label: {
                  show: true,
                  fontSize: 15,
                  fontWeight: 600,
                  formatter: "{c} 条",
                },
              },
              data,
            },
          ],
        },
        data.map((d) => `${d.name} ${d.value} 条`).join("；"),
      );
    else emptyChart("#distribution-chart", "暂无来源数据");
    $("#distribution-footer").textContent =
      "不同来源的 Stars / Points 分开分析";
    return;
  }
  const paths = [
    ...new Set(source.items.flatMap((item) => Object.keys(flatten(item)))),
  ];
  const initial =
    source.rule.display?.score || source.rule.display?.group || paths[0];
  setSelect(
    "#distribution-field",
    paths.map((path) => [path, path]),
    paths.includes(selector.value) ? selector.value : initial,
  );
  $("#distribution-title").textContent = "字段分布";
  const profile = fieldProfile(source.items, selector.value);
  $("#distribution-caption").textContent =
    `${selector.value || "未选字段"} · ${profile.present}/${profile.total} 条有值`;
  if (!profile.present) {
    emptyChart("#distribution-chart", "该字段暂无值");
    $("#distribution-footer").textContent = "";
    return;
  }
  let names, values;
  if (profile.numeric) {
    const bins = profile.min === profile.max ? 1 : 5,
      width = (profile.max - profile.min) / bins;
    const counts = Array(bins).fill(0);
    for (const item of source.items) {
      const value = numeric(readPath(item, selector.value));
      if (value !== null)
        counts[
          Math.min(bins - 1, Math.floor((value - profile.min) / (width || 1)))
        ]++;
    }
    names = counts.map((_, i) =>
      bins === 1
        ? number(profile.min)
        : `${number(Math.round(profile.min + i * width))}–${number(Math.round(profile.min + (i + 1) * width))}`,
    );
    values = counts;
    $("#distribution-footer").textContent =
      `中位数 ${number(profile.median)} · 最小 ${number(profile.min)} · 最大 ${number(profile.max)} · 缺失 ${profile.missing}`;
  } else {
    names = profile.distribution.map((d) => d[0]);
    values = profile.distribution.map((d) => d[1]);
    $("#distribution-footer").textContent =
      `${profile.distinct} 个不同值 · 显示前 ${names.length} 项 · 缺失 ${profile.missing}`;
  }
  chart(
    "#distribution-chart",
    {
      grid: { left: 14, right: 35, top: 10, bottom: 25, containLabel: true },
      xAxis: { ...axes, type: "value", minInterval: 1 },
      yAxis: {
        ...axes,
        type: "category",
        inverse: true,
        data: names,
        axisLabel: { ...axes.axisLabel, width: 130, overflow: "truncate" },
        splitLine: { show: false },
      },
      series: [
        {
          type: "bar",
          barMaxWidth: 18,
          data: values,
          itemStyle: {
            color: sourceColor(source.rule.id),
            borderRadius: [0, 3, 3, 0],
          },
          label: {
            show: true,
            position: "right",
            fontSize: 10,
            color: "#617080",
          },
        },
      ],
    },
    `${selector.value} 分布：` +
      names.map((name, i) => `${name} ${values[i]}条`).join("；"),
  );
}

function renderTable() {
  const sources = selectedSources(),
    source = currentSource();
  const crossSource = !source;
  for (const option of $("#sort").options)
    option.disabled =
      crossSource &&
      ["score", "scoreDelta", "rankDelta"].includes(option.value);
  if (
    crossSource &&
    ["score", "scoreDelta", "rankDelta"].includes(state.sort)
  ) {
    state.sort = "rank";
    $("#sort").value = "rank";
  }
  const cumulative = state.tableScope === "all";
  const baseRows = cumulative
    ? sources.flatMap((s) => s.allRows)
    : sources.flatMap((s) => s.rows);
  // 累计模式不做相邻变化筛选，强制 all，避免被上次的变化过滤条件滤空
  const result = filterRows(
    baseRows,
    cumulative ? { ...state, change: "all" } : state,
  );
  state.rows = result.rows;
  state.page = result.page;
  state.filtered = result.all;
  $("#data-total").textContent = result.total;
  const snapshotCount = sources.reduce(
    (n, s) => n + s.history.length,
    0,
  );
  $("#table-caption").textContent = cumulative
    ? source
      ? `${source.rule.name} · 跨 ${source.history.length} 份成功快照去重累计 · 指标保留原始单位`
      : `跨 ${snapshotCount} 份成功快照按链接去重累计 · 全部真实采集记录`
    : source
      ? `${source.rule.name} · ${date(source.latest?.finishedAt)} · ${source.previous ? "对比上一份成功快照" : "首次观测"}`
      : "每个来源的最新成功快照 · 指标保留原始单位";
  $("#export").disabled = !result.total;
  // 变化筛选只在"仅最新快照"相邻对比时有意义
  $("#change-tabs").hidden = cumulative;
  document.querySelectorAll("[data-scope]").forEach((el) => {
    const on = el.dataset.scope === state.tableScope;
    el.classList.toggle("active", on);
    el.setAttribute("aria-pressed", String(on));
  });
  document.querySelectorAll("[data-change]").forEach((el) => {
    el.classList.toggle("active", el.dataset.change === state.change);
    el.setAttribute("aria-pressed", String(el.dataset.change === state.change));
  });
  $("#data-table").innerHTML = result.total
    ? `<table><thead><tr><th class="numeric">排名</th><th>标题 / 名称</th>${crossSource ? "<th>来源</th>" : ""}<th class="numeric">${escape(source?.rule.display?.score_label || "来源指标")}</th><th class="numeric">指标增量</th><th class="numeric">排名变化</th><th>状态</th><th></th></tr></thead><tbody>${result.rows.map((row, index) => `<tr><td class="numeric neutral">${number(row.rank)}</td><td class="record-title"><button class="record-button" data-row="${index}">${escape(titleOf(row.item, row.rule))}</button><div class="record-description">${escape(field(row.item, row.rule, "description") ?? field(row.item, row.rule, "url") ?? "")}</div></td>${crossSource ? `<td>${sourceChip(row.rule)}</td>` : ""}<td class="numeric">${number(row.score)}${crossSource ? ` <span class="muted">${escape(row.rule.display?.score_label || "")}</span>` : ""}</td><td class="numeric">${delta(row.scoreDelta)}</td><td class="numeric">${row.rankDelta === null ? "—" : row.rankDelta > 0 ? "↑ " + number(row.rankDelta) : row.rankDelta < 0 ? "↓ " + number(-row.rankDelta) : "—"}</td><td>${statusTag(row.change)}</td><td><button class="icon-button" data-row="${index}" title="查看数据详情" aria-label="查看数据详情">${icon("arrow-up-right")}</button></td></tr>`).join("")}</tbody></table>`
    : empty(
        "没有符合条件的记录",
        state.query || state.change !== "all"
          ? "调整搜索词或变化筛选"
          : "点击开始采集以建立真实快照",
      );
  $("#page-caption").textContent =
    `${result.total ? (result.page - 1) * state.pageSize + 1 : 0}–${Math.min(result.page * state.pageSize, result.total)} / ${result.total} 条`;
  $("#page-number").textContent = `${result.page} / ${result.pages}`;
  $("#previous-page").disabled = result.page <= 1;
  $("#next-page").disabled = result.page >= result.pages;
  icons();
}

function renderCompareOptions() {
  const source = currentSource();
  const options = (source?.history ?? []).map((h) => [
    String(h.taskId),
    `#${h.taskId} · ${date(h.finishedAt)} · ${h.items.length} 条`,
  ]);
  setSelect(
    "#compare-before",
    options,
    $("#compare-before").value || String(source?.previous?.taskId ?? ""),
  );
  setSelect(
    "#compare-after",
    options,
    $("#compare-after").value || String(source?.latest?.taskId ?? ""),
  );
  renderComparison();
}
function renderComparison() {
  const source = currentSource();
  const before = source?.history.find(
      (h) => String(h.taskId) === $("#compare-before").value,
    ),
    after = source?.history.find(
      (h) => String(h.taskId) === $("#compare-after").value,
    );
  const note = $("#compare-note");
  if (
    !source ||
    !before ||
    !after ||
    before.taskId === after.taskId ||
    Date.parse(before.finishedAt) >= Date.parse(after.finishedAt)
  ) {
    $("#compare-summary").innerHTML = "";
    note.textContent = !source
      ? "先选择一个来源，再比较它的两份快照。"
      : "请选择时间更早的基准快照与时间更晚的对比快照。";
    $("#comparison-table").innerHTML = empty(
      source?.history.length < 2 ? "尚需第二份成功快照" : "等待选择快照",
    );
    return;
  }
  const keyValid = (s) => {
    const keys = s.items.map((item) => readPath(item, source.rule.item_key));
    return (
      keys.every((k) => k !== undefined && k !== null && k !== "") &&
      new Set(keys.map(String)).size === keys.length
    );
  };
  if (before.meta?.rule_version !== after.meta?.rule_version) {
    $("#compare-summary").innerHTML = "";
    note.textContent = "两份快照使用的规则版本不同，字段解析口径可能发生变化。";
    $("#comparison-table").innerHTML = empty(
      "请选择相同规则版本的快照",
      "规则修正不应被当作源站数据增长",
    );
    return;
  }
  if (!keyValid(before) || !keyValid(after)) {
    $("#compare-summary").innerHTML = "";
    note.textContent = "快照包含缺失或重复唯一键，无法可靠比较。";
    $("#comparison-table").innerHTML = empty("先检查规则健康中的唯一键质量");
    return;
  }
  const comparison = compareSnapshots(before, after, source.rule);
  $("#compare-summary").innerHTML =
    metric("新出现", number(comparison.added.length), "本次采集范围内新增") +
    metric("字段变化", number(comparison.changed.length), "按稳定业务键匹配") +
    metric("未再出现", number(comparison.removed.length), "不代表源站已删除") +
    metric(
      "记录净变化",
      number(comparison.countDelta),
      `${before.items.length} → ${after.items.length} 条`,
    );
  note.textContent = `${date(before.finishedAt)} → ${date(after.finishedAt)} · 仅比较两次采集范围，榜单退出不代表内容被删除。`;
  const entries = [
    ...comparison.changed.map((r) => ({
      ...r,
      type: "changed",
      changes: differences(r.before, r.item),
    })),
    ...comparison.added.map((r) => ({ ...r, type: "added", changes: [] })),
    ...comparison.removed.map((r) => ({ ...r, type: "removed", changes: [] })),
  ];
  state.comparisonRows = entries;
  $("#comparison-table").innerHTML = entries.length
    ? `<table><thead><tr><th>记录</th><th>变化类型</th><th>字段</th><th>基准值 → 对比值</th></tr></thead><tbody>${entries
        .flatMap((entry) => {
          const changes = entry.changes.length
            ? entry.changes
            : [
                {
                  field: "—",
                  before: entry.type === "added" ? "未收录" : "已收录",
                  after: entry.type === "removed" ? "未收录" : "已收录",
                },
              ];
          return changes.map(
            (change, i) =>
              `<tr><td class="record-title">${i ? "" : escape(titleOf(entry.item, source.rule))}</td><td>${i ? "" : statusTag(entry.type)}</td><td>${escape(change.field)}</td><td><div class="difference"><span class="before">${escape(typeof change.before === "object" ? JSON.stringify(change.before) : (change.before ?? "—"))}</span><span>→</span><span class="after">${escape(typeof change.after === "object" ? JSON.stringify(change.after) : (change.after ?? "—"))}</span></div></td></tr>`,
          );
        })
        .join("")}</tbody></table>`
    : empty("两份快照内容一致", "采集成功，未检测到字段或记录变化");
  icons();
}

function renderHealth() {
  const sources = selectedSources();
  $("#health-summary").innerHTML =
    metric(
      "健康规则",
      sources.filter((s) => s.health.status === "healthy").length,
      `${sources.length} 条已加载规则`,
    ) +
    metric(
      "需要关注",
      sources.filter((s) => ["failing", "degraded"].includes(s.health.status))
        .length,
      "执行、时效或数据质量异常",
    ) +
    metric(
      "唯一键问题",
      sources.reduce((n, s) => n + s.duplicates + s.missingKeys, 0),
      "最新成功快照的缺键或重复",
    ) +
    metric(
      "未运行",
      sources.filter((s) => !s.runs.length).length,
      "尚未建立观测基线",
    );
  $("#health-table").innerHTML =
    `<table><thead><tr><th>采集规则</th><th>健康状态</th><th>判断依据</th><th class="numeric">最新条数 / 阈值</th><th>最近成功</th><th>新鲜度基准</th><th></th></tr></thead><tbody>${sources.map((s) => `<tr><td>${escape(s.rule.name)}<div class="muted">${escape(s.rule.source)}</div></td><td>${statusTag(s.health.status)}</td><td class="health-reason">${escape(s.health.reason)}</td><td class="numeric">${s.items.length} / ${s.rule.min_items}</td><td>${date(s.health.latestSuccessAt)}</td><td>${escape(s.rule.schedule)}<div class="muted">期望周期，非自动调度</div></td><td>${s.health.latestTaskId ? `<button class="table-action" data-task="${s.health.latestTaskId}">运行详情</button>` : ""}</td></tr>`).join("")}</tbody></table>`;
}
function renderRuns() {
  const status = $("#run-status").value;
  const runs = selectedSources()
    .flatMap((s) => s.runs)
    .filter((r) => status === "all" || r.status === status)
    .sort((a, b) => Date.parse(b.finishedAt) - Date.parse(a.finishedAt));
  $("#run-count").textContent = runs.length;
  $("#runs-table").innerHTML = runs.length
    ? `<table><thead><tr><th>任务</th><th>采集规则</th><th>状态</th><th class="numeric">结果条数</th><th>完成时间</th><th>错误摘要</th><th></th></tr></thead><tbody>${runs.map((r) => `<tr><td><button class="table-action" data-task="${r.taskId}">#${r.taskId}</button></td><td>${escape(state.data.sources.find((s) => s.rule.id === r.ruleId)?.rule.name || r.ruleId)}</td><td>${statusTag(r.status)}</td><td class="numeric">${r.status === "succeeded" ? r.items.length : "—"}</td><td>${date(r.finishedAt)}</td><td title="${escape(r.lastError)}">${escape(r.failureCode || r.parseError?.message || "—")}</td><td><button class="table-action" data-task="${r.taskId}">查看日志</button></td></tr>`).join("")}</tbody></table>`
    : empty("当前范围没有运行记录");
}

function showDetail(title, source, html) {
  state.detailRequest++;
  state.charts.get("#detail-chart")?.dispose();
  state.charts.delete("#detail-chart");
  $("#detail-title").textContent = title;
  $("#detail-source").textContent = source;
  $("#detail-body").innerHTML = html;
  if (!$("#detail-dialog").open) $("#detail-dialog").showModal();
  icons();
}
function openRow(row) {
  const source = state.data.sources.find((s) => s.rule.id === row.rule.id),
    url = safeURL(field(row.item, row.rule, "url"));
  const history = source.history
    .filter((s) => s.meta?.rule_version === source.latest?.meta?.rule_version)
    .map((s) => ({
      snapshot: s,
      item: s.items.find(
        (item) => String(readPath(item, row.rule.item_key)) === String(row.key),
      ),
    }))
    .reverse();
  showDetail(
    titleOf(row.item, row.rule),
    row.rule.name,
    `<div class="detail-summary">${statusTag(row.change)}<span class="muted">采集于 ${date(row.at)}</span><button class="table-action" data-task="${row.taskId}">任务 #${row.taskId}</button>${url ? `<a href="${escape(url)}" target="_blank" rel="noopener noreferrer">查看原始页面 ${icon("external-link")}</a>` : ""}</div><section class="detail-section"><h3>${escape(row.rule.display?.score_label || "指标")} 观测历史</h3><div id="detail-chart" class="detail-chart"></div><p class="muted">${history.filter((h) => h.item).length} 次观测 · 不在采集范围的快照保留为空缺</p></section>${row.diff.length ? `<section class="detail-section"><h3>与上次相比</h3>${row.diff.map((d) => `<p><span class="muted">${escape(d.field)}</span> ${escape(typeof d.before === "object" ? JSON.stringify(d.before) : (d.before ?? "—"))} → ${escape(typeof d.after === "object" ? JSON.stringify(d.after) : (d.after ?? "—"))}</p>`).join("")}</section>` : ""}<section class="detail-section"><h3>记录字段</h3><dl class="field-grid">${Object.entries(
      flatten(row.item),
    )
      .map(
        ([key, value]) =>
          `<div><dt>${escape(key)}</dt><dd>${escape(typeof value === "object" ? JSON.stringify(value) : value)}</dd></div>`,
      )
      .join(
        "",
      )}</dl></section><details class="detail-section"><summary>原始 JSON</summary><pre>${escape(JSON.stringify(row.item, null, 2))}</pre></details>`,
  );
  const points = history.map((h) => [
    Date.parse(h.snapshot.finishedAt),
    numeric(field(h.item, row.rule, "score")),
  ]);
  if (points.some((p) => p[1] !== null))
    chart(
      "#detail-chart",
      {
        grid: { left: 55, right: 20, top: 15, bottom: 35 },
        xAxis: { ...axes, type: "time" },
        yAxis: { ...axes, type: "value", scale: true },
        series: [
          {
            type: "line",
            symbolSize: 8,
            connectNulls: false,
            data: points,
            itemStyle: { color: sourceColor(row.rule.id) },
          },
        ],
      },
      "该记录的来源指标观测历史",
    );
  else emptyChart("#detail-chart", "未配置数值指标");
}

const stages = {
  accepted: "任务受理",
  outbox_created: "创建待投递记录",
  published: "投递 Redis",
  claimed: "Worker 领取任务",
  lua_finished: "Lua 执行完成",
  mysql_committed: "结果事务提交",
  completed: "任务完成",
  acked: "消息确认",
  failed: "执行失败",
  reclaimed: "恢复未确认任务",
};
async function openTask(id) {
  showDetail(
    `任务 #${id}`,
    "运行详情",
    '<div class="empty">读取执行日志…</div>',
  );
  const version = state.detailRequest;
  try {
    const [task, events] = await Promise.all([
      fetchTask(id),
      fetchTaskEvents(id),
    ]);
    if (version !== state.detailRequest) return;
    $("#detail-body").innerHTML =
      `<div class="detail-summary">${statusTag(task.status)}<span class="muted">${escape(task.target)}</span></div><dl class="field-grid"><div><dt>目标地址</dt><dd>${escape(task.url)}</dd></div><div><dt>执行次数</dt><dd>${Number(task.retry_count || 0) + 1}</dd></div><div><dt>排队时间</dt><dd>${date(task.queued_at)}</dd></div><div><dt>完成时间</dt><dd>${date(task.finished_at)}</dd></div></dl>${task.last_error ? `<section class="detail-section"><h3>错误详情</h3><pre>${escape(task.last_error)}</pre></section>` : ""}<section class="detail-section"><h3>执行日志 <span class="count">${events.length}</span></h3><div class="trace">${events.length ? events.map((event) => `<div class="trace-event ${event.level === "error" ? "error" : ""}"><span class="trace-time">${date(event.created_at)}</span><span class="trace-line"></span><div><strong>${escape(stages[event.stage] || event.stage)}</strong><p>${escape(event.message || event.code || "")}</p></div></div>`).join("") : empty("该任务没有持久化事件")}</div></section>`;
  } catch (error) {
    if (version === state.detailRequest)
      $("#detail-body").innerHTML = empty(`读取失败：${error.message}`);
  }
  icons();
}

function openCollect() {
  const rules = state.rules.filter((rule) => rule.script && rule.url);
  setSelect(
    "#collect-rule",
    rules.map((rule) => [rule.id, rule.name]),
    state.source,
  );
  updateCollectRule();
  $("#collect-error").textContent = "";
  state.submitKey = null;
  $("#collect-dialog").showModal();
}
function updateCollectRule() {
  const rule = state.rules.find((r) => r.id === $("#collect-rule").value);
  $("#collect-url").value = rule?.url || "";
  $("#collect-script").textContent = rule ? `数据来源：${rule.source}` : "";
  state.submitKey = null;
}
function pendingNotice() {
  if (state.pending.size)
    banner(
      [...state.pending]
        .map(([id, task]) => `${task.name || `任务 #${id}`}：${task.status}`)
        .join("；"),
    );
}
function trackTask(task) {
  const rule = state.rules.find((candidate) => candidate.id === task.target);
  state.pending.set(task.id, {
    name: rule?.source || rule?.name || task.target || `任务 #${task.id}`,
    status: "正在排队",
  });
  watchTask(task.id);
}
async function watchTask(id, attempt = 0) {
  if (!state.pending.has(id)) return;
  try {
    const task = await fetchTask(id);
    if (["succeeded", "failed"].includes(task.status)) {
      state.pending.delete(id);
      banner(
        `任务 #${id} ${task.status === "succeeded" ? "采集完成，结果已更新" : "采集失败，可在运行日志查看原因"}。`,
      );
      await load();
      return;
    }
    state.pending.set(id, {
      ...state.pending.get(id),
      status: task.status === "running" ? "正在采集" : "正在排队",
    });
    pendingNotice();
  } catch (error) {
    state.pending.set(id, {
      ...state.pending.get(id),
      status: `查询中断：${error.message}`,
    });
    pendingNotice();
  }
  if (attempt >= 60) {
    state.pending.delete(id);
    banner(`任务 #${id} 仍未确认完成，请在运行日志中检查。`);
    return;
  }
  setTimeout(() => watchTask(id, attempt + 1), 2000);
}

$("#refresh").addEventListener("click", load);
$("#range").addEventListener("change", () => {
  state.page = 1;
  load();
});
$("#source-select").addEventListener("change", (event) =>
  chooseSource(event.target.value),
);
$("#source-nav").addEventListener("click", (event) => {
  const target = event.target.closest("[data-source]");
  if (target) chooseSource(target.dataset.source);
});
$("#search").addEventListener("input", (event) => {
  state.query = event.target.value;
  state.page = 1;
  renderTable();
});
$("#change-tabs").addEventListener("click", (event) => {
  const target = event.target.closest("[data-change]");
  if (target) {
    state.change = target.dataset.change;
    state.page = 1;
    renderTable();
  }
});
$("#table-scope-tabs").addEventListener("click", (event) => {
  const target = event.target.closest("[data-scope]");
  if (target && target.dataset.scope !== state.tableScope) {
    state.tableScope = target.dataset.scope;
    state.page = 1;
    renderTable();
  }
});
$("#sort").addEventListener("change", (event) => {
  state.sort = event.target.value;
  state.page = 1;
  renderTable();
});
$("#page-size").addEventListener("change", (event) => {
  state.pageSize = Number(event.target.value);
  state.page = 1;
  renderTable();
});
$("#previous-page").addEventListener("click", () => {
  state.page--;
  renderTable();
});
$("#next-page").addEventListener("click", () => {
  state.page++;
  renderTable();
});
$("#trend-metric").addEventListener("change", renderAnalysis);
$("#chart-mode").addEventListener("change", renderAnalysis);
$("#second-metric").addEventListener("change", renderAnalysis);
$("#distribution-field").addEventListener("change", () =>
  renderDistribution(currentSource(), selectedSources()),
);
$("#compare-before").addEventListener("change", renderComparison);
$("#compare-after").addEventListener("change", renderComparison);
$("#run-status").addEventListener("change", renderRuns);
document.addEventListener("click", (event) => {
  const close = event.target.closest("[data-close]");
  if (close) {
    $("#" + close.dataset.close).close();
    return;
  }
  const task = event.target.closest("[data-task]");
  if (task) {
    openTask(Number(task.dataset.task));
    return;
  }
  const row = event.target.closest("[data-row]");
  if (row) openRow(state.rows[Number(row.dataset.row)]);
});
$("#detail-dialog").addEventListener("close", () => {
  state.detailRequest++;
  state.charts.get("#detail-chart")?.dispose();
  state.charts.delete("#detail-chart");
});
$("#export").addEventListener("click", () => {
  const blob = new Blob([csv(state.filtered)], {
      type: "text/csv;charset=utf-8",
    }),
    url = URL.createObjectURL(blob),
    link = document.createElement("a");
  link.href = url;
  link.download = `luaspider-${state.source}-${new Date().toISOString().slice(0, 10)}.csv`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
});
$("#collect").addEventListener("click", async () => {
  const button = $("#collect");
  button.disabled = true;
  try {
    const results = await createJDBatch(
      state.rules,
      `jd-batch-${crypto.randomUUID()}`,
    );
    const tasks = results
      .filter((result) => result.status === "fulfilled")
      .map((result) => result.value);
    const failures = results.length - tasks.length;
    tasks.forEach(trackTask);
    banner(
      failures
        ? `已同时提交 ${tasks.length} 个 JD 来源；${failures} 个未能入队。`
        : "四个 JD 来源已同时入队，正在等待 worker 接手。",
      failures > 0,
    );
  } catch (error) {
    banner(`无法启动四源 JD 采集：${error.message}`, true);
  } finally {
    renderNavigation();
  }
});
$("#collect-rule").addEventListener("change", updateCollectRule);
$("#collect-url").addEventListener("input", () => {
  state.submitKey = null;
});
$("#collect-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#submit-collect");
  button.disabled = true;
  $("#collect-error").textContent = "";
  state.submitKey ??= crypto.randomUUID();
  try {
    const task = await createTask({
      target: $("#collect-rule").value,
      url: $("#collect-url").value,
      requestKey: state.submitKey,
    });
    $("#collect-dialog").close();
    trackTask(task);
    pendingNotice();
  } catch (error) {
    $("#collect-error").textContent =
      `提交未确认：${error.message}。重试将使用相同请求编号。`;
  } finally {
    button.disabled = false;
  }
});
window.addEventListener("hashchange", applyView);
new ResizeObserver(() => {
  for (const chart of state.charts.values()) chart.resize();
}).observe($("#main"));
applyView();
load();

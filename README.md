<p align="center">
  <picture>
    <source media="(max-width: 640px)" srcset=".github/assets/readme-hero-mobile.svg">
    <img src=".github/assets/readme-hero.svg" alt="GoRelay — 自托管的 MCP 外部数据连接器服务" width="1200">
  </picture>
</p>

<p align="center">
  <a href="#快速开始">快速开始</a> &nbsp;·&nbsp;
  <a href="#适用边界">适用边界</a> &nbsp;·&nbsp;
  <a href="#架构与核心机制">架构与机制</a> &nbsp;·&nbsp;
  <a href="#mcp-工具">MCP 工具</a> &nbsp;·&nbsp;
  <a href="docs/architecture-v2.md">设计文档</a>
</p>

---

**GoRelay** 是一个自托管的 MCP 服务，用 Go、独立进程 Lua 沙箱、MySQL 和 Redis Streams 实现，面向 Claude Code、Cursor、Codex 等支持 MCP 的 Agent。

它解决的问题很具体：让 Agent 跨会话反复访问同一批公开接口或网页时，不必每次都重抓几百 KB 原始页面塞进上下文，也不必每次重写解析脚本。首次探测并验证通过的规则会存成带版本号的连接器（JSON 接口用声明式配置，复杂网页用隔离子进程 Lua），后续对话直接查本地缓存，在服务端算好过滤、分组汇总或链接提取后再返回；原始响应按 SHA-256 压缩留盘，支持 BM25 检索核对。

## 适用边界

- **适合用 GoRelay**：需要给 Agent 接一批**长期复用**的外部接口或页面；希望在服务端做聚合/裁剪、避免原始 HTML 占满上下文；需要字段漂移拦截、版本回退和原文追溯；接受自托管常驻服务。
- **不适合用 GoRelay**：
  - 只是临时抓一两篇网页文章 → 直接用客户端自带 `WebFetch` 或 Firecrawl，没必要为此部署 MySQL 和 Redis。
  - 需要绕过强反爬、打码或代理 IP 轮换 → GoRelay 默认遵守 `robots.txt`，不内置代理池或反爬绕过。
  - 想要零依赖单文件本地工具 → 当前版本面向多页异步任务和故障恢复，依赖 MySQL 8.4 与 Redis 7.4。

---

## 快速开始

### 1. 不装 Docker，10 秒运行离线仿真（需要 Go 1.26+）

```bash
git clone https://github.com/KamiNeko-pre/GoRelay.git
cd GoRelay
go run ./cmd/bench-mcp -mode sim
```

命令会在内存中跑完一条核心链路（接口识别自动升级 `http_api` → 第 2 页断点续跑 → `output_schema` 字段裁剪 → 伪 `200 OK` 拦截页回滚），实际输出如下：

```json
{
  "mode": "sim",
  "cold_build_round_trips": 2,
  "warm_reuse_round_trips": 1,
  "cache_hit_round_trips": 1,
  "baseline_raw_bytes": 26561,
  "gorelay_projected_bytes": 90,
  "payload_reduction_pct": 99.66115733594367,
  "drift_blocked_overwrite": true,
  "checkpoint_resumed_page_2": true,
  "http_api_promoted": true,
  "duration_ms": 1
}
```

### 2. Docker Compose 完整启动

会编译 Go 镜像并拉起 6 个容器（`mysql`、`redis`、`lua-runner`、`app`、`publisher`、`worker`），首次构建约 1–3 分钟。`.env.example` 已提供本地开发默认配置，可直接运行（公网或团队环境请务必修改密码与令牌）：

```bash
cp .env.example .env
docker compose up -d --build --wait
curl -fsS http://127.0.0.1:8080/healthz
```

启动后服务监听 `http://127.0.0.1:8080`：
- MCP 端点：`http://127.0.0.1:8080/mcp`
- 只读管理台：`http://127.0.0.1:8080/`（查看连接器、复用统计与漂移事件）
- 原文核查页：`http://127.0.0.1:8080/evidence`（按 SHA-256 哈希回查归档快照）

### 3. 接入 Claude Code / Cursor / Codex

在 MCP 客户端中加入以下配置（若客户端不支持环境变量展开，直接将 Token 写为 `.env` 中的 `local-dev-admin-token`；其他客户端模板见 [`configs/mcp/`](configs/mcp/)）：

```json
{
  "mcpServers": {
    "gorelay": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": {
        "Authorization": "Bearer local-dev-admin-token"
      }
    }
  }
}
```

也可直接用内置 CLI 验证握手与工具契约：

```bash
GORELAY_ADMIN_TOKEN=local-dev-admin-token go run ./cmd/mcp-check -url http://127.0.0.1:8080/mcp
```

---

## 架构与核心机制

<picture>
  <source media="(max-width: 640px)" srcset=".github/assets/architecture-mobile.svg">
  <img src=".github/assets/architecture.svg" alt="GoRelay 进程与数据流拓扑">
</picture>

- **固定 6 个 MCP 工具**：连接器持久化在 MySQL 中按域名或关键词检索，不向宿主动态注册新工具。握手指令控制在 `<1 KiB` 内并附带活跃连接器字段类型；单次查询支持最多 4 个服务端视图（过滤、排序、分组、URL 提取），大结果集直接走 CSV / JSON / Markdown 导出链接。
- **进程级沙箱与统一出网**：标准 JSON 接口走声明式 `http_api`（内置 `page` / `offset` / `cursor` 分页）；复杂页面解析在独立 `lua-runner -child` 子进程运行（只读根文件系统、`cap_drop: ALL`、无宿主机密钥、超时或取消强杀进程树）。沙箱不直连外网，所有请求经父进程 `SafeClient` 代理，统一执行内网/回环阻断、`robots.txt`、单域名令牌桶限速、429 熔断与 `5 MiB` 体积上限。
- **静默漂移拦截与版本回滚**：数据落库前校验必填字段、行数基线和伪 `200` 拦截页，异常时回滚本次数据事务保留旧记录，并独立记录 `DriftEvent`；历史版本不可变留存，支持通过 `rollback_to` 切回。入库前同时剥离 `<script>` 与不可见控制字符。
- **Outbox 投递与页级断点续跑**：多页任务与异步刷新走 `MySQL Outbox → Redis Streams → Worker`。任务创建与 Outbox 同事务写入；Worker 崩溃后由 `XAUTOCLAIM` 接管，配合租约心跳与 `run_token` CAS 校验防止旧节点回写，并从已落盘的 `checkpoint` 页码继续。原始响应按 SHA-256 异步 Gzip 归档，内置纯 Go BM25 检索。

GoRelay 不内置聊天模型（不在工具内部嵌套 Agent 循环），完整模块边界与表结构见 [`docs/architecture-v2.md`](docs/architecture-v2.md)。

---

## MCP 工具

| 工具名 | 性质 | 职责 |
| :--- | :---: | :--- |
| `gorelay_list_sources` | 只读 | 按域名、URL 或关键词查询已发布连接器与缓存新鲜度 |
| `gorelay_probe_source` | 只读 | 过 `SafeClient` 预检，识别 JSON API、内嵌 JSON、Sidecar XHR 或 DOM 选择器样本 |
| `gorelay_deploy_and_run` | 写入 | 试跑校验 `api_spec` 或 Lua 脚本，通过后发布不可变版本并保存首页数据，支持 `rollback_to` |
| `gorelay_run_connector` | 写入 | 复用连接器（默认 1 小时缓存命中 0 外网请求），支持多视图 `queries` 与隔离只读 `target_url` |
| `gorelay_get_job_data` | 写入 | 查询长任务状态、分页/多视图读数，或传 `cancel=true` 强杀沙箱进程 |
| `gorelay_search_evidence` | 只读 | 限定 `source_ids` 用纯 Go BM25 检索归档原文，或按 `snapshot_ref` 分页阅读快照 |

<details>
<summary><strong>HTTP 管理、流式导出接口与目录结构</strong></summary>

#### HTTP 接口（Bearer Token 鉴权）

| 路径 | 用途 |
| :--- | :--- |
| `GET /api/v1/connectors` | 连接器列表与缓存状态 |
| `GET /api/v1/metrics/summary` | 命中类型、延迟、入库量与漂移汇总 |
| `GET /api/v1/drift-events` | 漂移拦截事件与诊断样本 |
| `GET /api/v1/jobs/:id` | 单次任务状态与生命周期事件 |
| `GET /api/v1/sources/:id/export` | 导出数据源全量记录（`?format=csv\|json\|md`） |
| `GET /api/v1/jobs/:id/export` | 导出单次任务记录（`?format=csv\|json\|md`） |
| `GET /api/v1/evidence` | BM25 证据检索与 SHA-256 快照读取 |

#### 代码目录

| 目录 | 说明 |
| :--- | :--- |
| [`internal/mcpserver`](internal/mcpserver) | 固定 6 工具 MCP Server、JSON Schema 枚举、动态目录与结构化错误 |
| [`internal/probe`](internal/probe) | 目标探测、内嵌 JSON 识别与 60s 短缓存 |
| [`internal/connector`](internal/connector) | 连接器版本库、`http_api` 分页执行、试跑晋升与 CheckpointHub |
| [`internal/fetchgov`](internal/fetchgov) · [`cmd/lua-runner`](cmd/lua-runner) | 共享 `SafeClient` 出网代理与 `-child` 独立进程沙箱 |
| [`internal/pipeline`](internal/pipeline) | 漂移门禁、文本净化、多视图下推、异步 Gzip/BM25 与流式导出 |
| [`internal/worker`](internal/worker) · [`internal/repository`](internal/repository) | Outbox 投递、Redis Streams 消费组、租约续期、`XAUTOCLAIM` 与 CAS 提交 |
| [`cmd`](cmd) · [`configs/mcp`](configs/mcp) | 各进程入口、仿真/探活 CLI 与 MCP 客户端配置模板 |

</details>

---

<p align="center">GoRelay · <a href="LICENSE">MIT License</a></p>

# GoRelay v2 Architecture Specification

## 1. Design Goals & Hard Constraints

GoRelay v2 is a **deterministic, zero-LLM MCP External Connector & Sandboxed Execution Engine** written in Go (`gorelay` module).

1. **Zero Internal Chat LLM / Zero Vector Store**: The host Agent retains 100% of planning, script authoring, repair, and final synthesis. GoRelay runs no internal planner LLM, Eino graph, or Qdrant vector cluster.
2. **Fixed 6-Tool MCP Surface**: Connectors (`http_api` JSON specs or `gopher-lua` scripts) are persisted as immutable versions in MySQL rather than registered dynamically as new MCP tools, preventing host context-window bloat.
3. **Deterministic Lookup + Recursive Discovery**: Stored connectors are matched deterministically by `domain`, `url`, `keyword`, or `kind`. Unknown target lists are resolved explicitly by the host via `kind="discovery"` sub-connectors before collecting data.
4. **Single Reliable Execution Plane & Zero Double-Fetch**: Warm reuse and multi-page continuation run through `MySQL (crawl_tasks + task_outbox) -> Publisher -> Redis Streams -> Worker (lease + XAUTOCLAIM + CAS fencing token)`. On `gorelay_deploy_and_run`, validated trial page-1 data is atomically promoted inside the version publish transaction (single-page connectors complete in 1 upstream request; multi-page connectors seed `checkpoint_json` and enqueue Worker to resume at page 2).
5. **Shared Fetch Governance (`internal/fetchgov`)**: All HTTP probing, `http_api`, and sandboxed `lua_sandbox` executions pass through long-lived `SafeClient` instances configured uniformly via `GORELAY_*` env vars (SSRF IP/CIDR blocking, DNS pinning, `robots.txt` + `Crawl-delay`, per-domain token buckets, 429/WAF/Captcha circuit breaking, scoped `AuthProfiles`, `Content-Type` allowlisting, `5 MiB` `io.LimitReader` bounds, and optional CDP Sidecar `http_render`). Untrusted `lua-runner -child` processes hold zero credentials and open zero outbound sockets, routing every `http_request` / `http_render` / `checkpoint` call over stdin/stdout JSON-lines IPC to the parent `lua-runner`'s shared `SafeClient`.

## 2. System Topology

```mermaid
flowchart TD
    Host["Host Agent (Claude / Codex / Business Agent)"]
    MCP["/mcp Streamable HTTP (6 Fixed Tools)"]
    Probe["internal/probe (JSON / Embedded JSON / XHR Sidecar / Compact DOM)"]
    Gov["internal/fetchgov SafeClient (SSRF / robots.txt / Token Bucket / 429 Breaker)"]
    Sidecar["Optional Out-of-Process CDP Sidecar (Semaphore Bounded)"]
    DB[("MySQL: connectors, connector_versions, crawl_tasks, task_outbox, source_records, snapshot_blobs")]
    Pub["cmd/publisher"]
    RS[("Redis Streams + PEL")]
    Worker["cmd/worker (Consumer + Reclaimer + Lease Renewal)"]
    API["http_api Declarative Executor"]
    Lua["cmd/lua-runner (-child OS Isolated gopher-lua + checkpoint IPC)"]
    Pipe["internal/pipeline (Drift Gate -> Sanitize -> ContentHash Dedup -> Gzip + BM25 -> <=16KB Page)"]

    Host <--> MCP
    MCP --> Probe --> Gov
    Gov -. "Optional SPA XHR Sniff" .-> Sidecar
    MCP --> DB
    DB --> Pub --> RS --> Worker
    Worker --> API --> Gov
    Worker --> Lua --> Gov
    API --> Pipe
    Lua --> Pipe
    Pipe --> DB
```

## 3. Package Responsibilities

| Package | Responsibility |
| :--- | :--- |
| `internal/fetchgov` | SSRF-checked `Requester` wrapper, RFC 9309 `RobotsCache`, per-domain `rate.Limiter` + `CircuitBreaker`, `429/WAF/Captcha` signal classifier, scoped `AuthProfile` injection, optional semaphore-limited `PageRenderer` sidecar client. |
| `internal/probe` | API-first source inspection (`SummarizeJSON`, `ExtractEmbeddedJSON`, optional Sidecar XHR API capture, compact DOM selector samples) plus the compact `< 350` token `LuaSandboxContract`. |
| `internal/connector` | MySQL connector & immutable version registry (`rollback_to` support), single-fetch trial validation & atomic page-1 promotion, active-job singleflight dedup, `http_api` JSON pointer + pagination executor, independent-tx silent-drift recording, `CheckpointHub` IPC callback handler, and `hot` 14-day canary monitor. |
| `internal/pipeline` | Inline `CheckSilentDrift` gate (data tx rolls back to preserve last known good `source_records` while `DriftEvent` + `degraded` commit separately), indirect-prompt-injection text sanitization, nullable `is_current` `(source_id, entity_key)` + `content_hash` versioned upsert, gzip snapshot archival + pure-Go BM25 `SearchEvidence`, and `<= 16 KiB` response budget packing. |
| `internal/mcpserver` | Stateless Streamable HTTP `/mcp` server exposing 6 fixed tools with structured `ToolFailure{Code, RecoverySteps}` contracts. |
| `cmd/lua-runner` | OS-isolated `-child` process executor (`Windows Job Object` memory/termination guard, Linux `Setpgid` process group kill) with zero-socket stdin/stdout JSON-lines IPC routing `http_request`, `http_render`, lazy snapshot reuse, and `checkpoint.get` / `checkpoint.save` through the parent's shared `SafeClient`. |
| `cmd/publisher`, `cmd/worker` | Durable MySQL Outbox poller and Redis Streams worker (`XREADGROUP` + `XAUTOCLAIM` + lease heartbeat + CAS fencing token). |

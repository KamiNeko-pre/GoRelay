# GoRelay v2 部署与运维说明

GoRelay v2 是一个零内置 LLM、零向量库依赖的轻量级 MCP 数据接入与沙箱执行引擎，核心运行拓扑仅包含：`app`（MCP + Read-Only Console）、`publisher`（MySQL Outbox 投递）、`worker`（Redis Streams + CAS 执行与管道清洗）及可选进程隔离的 `lua-runner`（`gopher-lua` `-child` 沙箱）。

## 1. 快速启动

```bash
cp deploy/.env.example deploy/.env
# 生成独立随机密码与 Token（推荐 openssl rand -hex 24）
docker compose --env-file deploy/.env -f deploy/compose.yml up -d --build --wait
curl -f http://127.0.0.1:5088/healthz
```

## 2. 安全边界

- **进程隔离沙箱**：`lua-runner` 以只读根文件系统、Drop All Capabilities、`pids_limit: 32` 与独立内存上限运行，每次执行单独派发 `-child` 子进程，绝不向沙箱注入数据库或管理凭据。
- **可观测管理台与引用页**：浏览器访问 `http://127.0.0.1:5088/` 查看连接器目录、命中率与 Drift 告警；点击证据 `snapshot_url` 访问 `/evidence?ref=<sha256>` 查看压缩归档原文。

## 3. 备份与冷恢复

```bash
bash deploy/backup.sh gorelay ./backups/latest
bash deploy/restore.sh gorelay gorelay-restore ./backups/latest
```

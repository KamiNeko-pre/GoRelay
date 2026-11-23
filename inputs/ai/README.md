# 离线执行与模型适配检查

这些样本用于检查真实执行链路；它们是手写样本，不计入模型自主生成成绩。

## 不需要模型或外部网站的检查

```bash
go run ./cmd/ai-check -mode status
go run ./cmd/ai-check -mode contract -input inputs/ai/task-spec-example.json
go run ./cmd/ai-check -mode replay -input inputs/ai/replay-pagination-detail.json
```

`status` 只核对配置，不输出密钥或调用服务；`contract` 检查任务字段契约；`replay` 严格匹配请求方法、完整 URL 与顺序，执行两页列表和三条详情，预期输出三条记录。回放与真实网络执行分别标记。

## 单次隔离程序

```bash
go build -o build/lua-runner ./cmd/lua-runner
./build/lua-runner < inputs/ai/program-no-network.json
```

PowerShell 可使用 `Get-Content -Raw inputs/ai/program-no-network.json | ./build/lua-runner.exe`。CLI 提供独立进程和执行配额，容器另提供内存、PID、CPU 与文件系统隔离。

## 主动调用服务的检查

只有以下命令会访问配置的模型、Embedding、搜索服务或真实网页，执行前需配置相应环境变量：

```bash
go run ./cmd/ai-check -mode chat -input inputs/knowledge/context-notes.md
go run ./cmd/ai-check -mode embed -input inputs/knowledge/context-notes.md
go run ./cmd/ai-check -mode search -query "component release notes"
go run ./cmd/ai-check -mode execute -input inputs/ai/program-public-read.json
```

本地 `go run` 不会自动加载 `.env`；Docker Compose 会读取项目 `.env`。Chat 使用原生 Tool Calling，Embedding 按响应 index 对齐批次。适配器不自动重发可能计费的请求，由上层任务预算和恢复流程决定重试。缺失 Usage 保留未知。

网络执行只允许受控的公开 GET/HEAD。Fake-IP DNS、私有网段与被拒目标需要修正网络配置，不能靠扩大公网白名单绕过校验。

Agent API、任务恢复、版本化工具注册与独立知识索引现已接入，使用方式见 [项目说明](../../README.md)。完整受控实验结果见 [评测口径](../../docs/benchmarks/README.md)。

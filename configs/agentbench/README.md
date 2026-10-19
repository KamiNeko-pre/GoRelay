# Agent 任务评测器

`cmd/agent-eval` 调用实际服务 API，记录任务、模型与搜索请求、工具结果、字段匹配和人工介入。gold 只交给判分器，不进入模型请求或工具代码。

## 快速检查

无需模型和数据库即可运行语义判分器的固定反例：

```bash
go run ./cmd/agent-eval -mode validator -output output/validator-audit.json
```

离线执行样本见 [inputs/ai](../../inputs/ai/README.md)，已完成实验的汇总见 [docs/benchmarks](../../docs/benchmarks/README.md)。

## 使用自己的任务集

目录中的 `*.candidate.json` 为开发候选示例，不是已冻结的正式实验。运行前确认来源、字段和 gold，使用独立 namespace 与输入快照。gold 文件保存在不提交的私有目录。

```bash
go run ./cmd/agent-eval -mode tasks -endpoint http://localhost:8080 -manifest /path/to/manifest.frozen.json -gold /path/to/gold.private.json -batch example-cold-1 -variant code-as-action -repetitions 1 -output output/agent-eval/example-cold-1
go run ./cmd/agent-eval -mode summarize -trials output/agent-eval/example-cold-1/trials.jsonl -output output/agent-eval/example-cold-1/summary.json
```

管理员令牌通过进程环境变量提供，不写入命令或 manifest。任务键按 batch、variant、case 和 repetition 生成，重跑同一键观察原任务。不要换任务键来掩盖失败。

任务结束但后台索引尚未完成时，可只刷新账本，不重新调用模型：

```bash
go run ./cmd/agent-eval -mode refresh -endpoint http://localhost:8080 -trials output/agent-eval/example-cold-1/trials.jsonl -output output/agent-eval/example-cold-1/trials-settled.jsonl
```

## 统计边界

- `correct_tasks` 只计完成且独立字段判分正确的交付；`correct_verdicts` 可以包括正确停止。
- 字段覆盖率与字段精确率分别计算，空输出、未准备 gold 或未知用量不视作通过。
- 模型 Token 包含失败尝试及搜索适配器报告的模型用量，缓存输入另列；未知用量保持未知。
- L0 无介入、L1 方案批准、L2 范围或来源提示、L3 人工代写/代执行分别统计。
- 工具回合数、模型请求数、字段正确性、引用支持性属于不同指标。对照两组必须采用同一输入与判分规则。

`cmd/rag-eval` 提供独立的语料导入、索引和检索入口；使用隔离的 `gorelay_eval_*` schema 和 collection，不复用生产知识状态。支持 dense、bm25、hybrid 模式，命令参数可用 `go run ./cmd/rag-eval -h` 查看。

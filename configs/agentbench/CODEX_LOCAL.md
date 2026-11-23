# 本机 Codex 临时模型实验

用途：没有 API Key 时，用本机已登录的 Codex 账户临时替代对话与搜索 API，使用账户额度。GoRelay 仍负责 Agent Loop、工具权限和 Lua 执行；本机适配不作为服务器生产配置。

2026-10-09 最新验证：原十年任务的架构修复第 1 轮最终为 partial，9 条有效记录、无最终结论，已报告 Token 1290840（包含缓存输入；决策、搜索及 Embedding 分阶段统计）。CLI 原始错误为工作区额度耗尽；此次不算完整闭环成功。用户要求控制额度后停止真实实验，后续先做回执回放和离线检查。禁止自动重跑、人工填来源或以小任务替代原任务验收。

新 Run 可用 `GORELAY_RUN_MODEL_CALLS` / `GORELAY_RUN_SEARCH_CALLS` 冻结调用次数，默认 12 / 3，本机下一次诊断脚本为 3 / 1，且关闭无限预算。失败调用同样预留次数，恢复不重置。此上限不是 Token 保证；不能把预算停止写成成功。正式闭环验收仍待成本可接受的真实运行。

上下文装配以 32 KiB 为近期历史的缩减目标（包含工具定义），64 KiB 为请求硬上限。新 Run 冻结 `loop_version=2`：只有检查点已消化的完成回合才能移出历史；未消化回合保留调用/回复配对，大正文必要时退为原回执读取句柄。保留原目标、冻结合同、持久进度及最新动作/反馈；最新不可拆分的合法回合允许超过缩减目标。旧 Run 保留原协议，不重写历史。此为字节约束，不据此宣称真实 Token 收益或任务成功率。

```powershell
$env:GORELAY_AI_BACKEND='codex_cli'
$env:GORELAY_AI_MODEL='gpt-6.1-sol'
$env:GORELAY_CODEX_BIN='<本机 codex.exe 绝对路径>'
$env:GORELAY_CODEX_EVIDENCE_DIR='<原始模型证据目录>'
$env:GORELAY_SEARCH_BACKEND='codex_cli'
$env:GORELAY_CODEX_SEARCH_EVIDENCE_DIR='<原始搜索证据目录>'
```

项目的 `Service.Configure` 接入此后端。模型只返回结构化动作提案，工具由 GoRelay 校验与执行；禁用原生 shell、浏览器、插件、MCP 配置加载和多 Agent，独立临时目录，忽略用户配置，不委派开发。动作、请求 ID、输入/缓存/输出 Token、耗时和原始 CLI 事件逐次保存；错误尝试同样计入。价格未提供时人民币成本保持未知。配置依据：[Codex 配置参考](https://developers.openai.com/codex/config-reference)。

搜索适配器仅在 Agent 提出 `discover_sources` 时启动独立 CLI 搜索请求，返回候选 URL 和标题，不读取正文、不写结论；正文由 GoRelay 生成的 Lua 获取。模型推理请求禁用原生搜索。额度耗尽按明确原因终止本次运行，保留已有交付和未知用量，不继续按网络故障重试。

任务进度通过 `update_task_progress` 保存，详情 API 的 `progress` 返回经过当前引用有效性检查的模型评估。工具观察提供完整标识和 `reads` 参数；正文、集合按原回执分页。每轮统一组装当前状态，并将工具定义计入请求大小；`limitations` 表示已披露的数据局限，`gaps` 表示未完成交付，两者分别记录。

2026-10-09 任务控制修复：新 Run 的检查点由宿主记录已消化操作位置和成功模型轮数，可保存 `explored_paths`。执行/证据变化或连续三轮未检查时，继续扩展搜索/采集会收到可恢复的检查点拒绝；检索、原回执读取和交付仍允许。模型可在同一批提议中先检查再行动。两个没有交付进展的检查周期后提供一次改变方法的恢复周期，第三次仍无进展则停止；修改备注或查询措辞不重置停滞。首次读取新的原始正文页可以暂停停滞计数，重复读取不算；这不会重置计数或满足需求。有效证据、成功验证的能力与需求支持变化可以推进，但不冒充独立语义验收。

最后一次模型调用预留给交付，探索准入关闭；新协议将决策调用与检索 Embedding 尝试分别计数，检索默认最多 12 次，避免检索吃掉最后的交付决策。旧 Run 的计数协议保留。HTTP 模型的永久错误立即停止，临时错误的退避时间写入原预算，重建服务不重置；请求 Context 受原 Run deadline 限制。429 的余额/额度/消费限制与临时限流分别处理，参照[官方错误分类](https://developers.openai.com/api/docs/guides/error-codes)。

## 模型就绪预检（2026-10-11）

当前本地和 HTTP 适配器均以受限推理验证就绪。HTTP 不要求 `/models` 目录接口；正常 Chat 请求携带一条短消息、无工具、maximum completion 512，并使用配置的 HTTP timeout。成功缓存 10 分钟、失败缓存 30 秒；同一服务并发提交共享探针。就绪结果暴露 checked_at、method 和已报告 usage，未报告不记零；它不证明后续额度、业务成功或全部工具能力。

预检可能消耗额度。它在 Run 之外，不能算入某个任务的预算或当作免费操作，实验应另记环境启动/就绪开销。CLI login status 只能证明本地登录信息存在，远程权限仍须推理探针确认。阶段 0 未通过时不凭本地登录开始业务实验。

## 请求前成本准入

原生 HTTP 模型正式创建任务前必须配置 `GORELAY_SPEND_POLICY_PATH`，该 JSON 的限额、币种、核价日期、模型价表和输入额外开销在新 Run 中冻结。模板 `spend-policy.example.json` 的零限额与 null 价格故意不可用，需核价后填入；不猜测任何付费模型价格。货币单位为百万分之一：限额 1000000 表示所填币种的 1 单位，某模型每百万 Token 价格 1 单位则相应 rate 填 1000000。

决策请求、按次计费搜索、检索 Embedding、后台索引 Embedding 共享同一 SQL 预算。先预留，再调用外部服务；报价与原始操作同事务保存为 `cost_reservation_json`。失败、用量未知和成功请求均保留全部预留，重放不再扣费；它是保守准入账本，实际费用仍由上游用量与价表独立核算，不把预留额冒充账单。后台索引不能绕过原 Run 的预算或取消；未提供预算归属的手工导入在正式严格索引模式下拒绝付费调用。

HTTP 文本适配器以完整 UTF-8 请求字节数加宿主声明的 tokenizer/framing 额外量作为输入预留，输出使用 `max_completion_tokens`。这个边界依赖所选服务的分词及协议假设，**不保证上游账单硬封顶**，也不覆盖代理内部额外模型调用；正式接入需核对实际使用量是否始终处于声明边界内。CLI 的固定上下文、内部搜索/推理和费用无法可靠预报价，启用严格策略后会拒绝 CLI；空策略的 CLI 实验继续仅执行次数限制，不能宣称 Token/金额封顶。

旧 Run 不会被新价表重新定价。已有未结束的原生 API Run 缺少冻结价表时可以重放已提交响应，但新付费请求会被拒绝；需要用已配置预算的新 Run 验证。当前仍未接正式付费 API，也未重跑原十年任务。

2026-10-09 恢复边界修复：模型和工具原始回执同时保存结构化失败分类及原始退避时间；错误回执已提交、Run 状态尚未提交时中断，恢复仍能停止鉴权/额度错误或等待限流，不将错误降为普通字符串。工具服务限流按工具隔离，修改查询不能绕过，读取本地状态/证据继续可用。模型选择空动作且仍有服务冷却时宿主等待，不重复调用模型。永久服务/成本拒绝会停止同批后续派发，并保存配对的未派发回复；普通来源失败保留给模型判断，不全局终止。重放已保存工具操作不重复占次数；外部工具受原 Run deadline 约束。

**目前的 CLI 固定上下文仍明显偏大。** 2026-10-09 的单任务用了 10 次模型请求、238630 Token、195279 ms，包含规划纠错、一个失败 Lua 候选和后续正式执行。仅作带官方入口提示的公网连通基线，不宣称一般成功率、成本优势或完整源发现能力。正式成本优化优先使用普通 HTTP 模型 API 做同模型/同场景对照。

可选付费实验测试默认跳过。先启动私有独立 runner，再设置 `MYSQL_INTEGRATION_DSN`、`GORELAY_RUNNER_URL/TOKEN`、`GORELAY_ADMIN_TOKEN`、`GORELAY_TEST_EVIDENCE_DIR`、`GORELAY_LIVE_MANIFEST` 与私有 `GORELAY_LIVE_GOLD`，执行：

```powershell
go test ./internal/agentbench -run '^TestLiveModelTaskExperiment$' -count=1 -v
```

默认模式通过真实应用 API/Agent Loop，在隔离 MySQL schema 泵送实际子任务；不含 Redis。可选设置 `GORELAY_LIVE_INDEXING=1` 接通真实 Embedding/Qdrant（自动创建并清理独立 collection）；设置 `GORELAY_LIVE_REDIS_ADDR` 则使用真实 Publisher/Redis/Worker/提交/ACK。`GORELAY_LIVE_RECOVERY_CASE=<case-id>` 仅在该开发实验的一项子任务已成功后，注入下一子任务的一次执行故障，再重建 Agent 服务实例；checkpoint 原样记录 Run/预算/任务，完整进程或服务器重启仍需另外验证。完整实验仍按 TASK_EXPERIMENTS.md。

## 已安装的仅推理适配器与本地 Embedding

本机已安装 OpenViking 的 Codex inference adapter 和 llama-cpp，可选实验脚本复用这两个库，不复制认证数据、不执行模型提议。标准 HTTP 模型入口无需改 Go 代码；只有用户显式启动 `--enable-codex` 才使用当前登录账户。BGE 使用真实 512 维向量和 tokenizer 用量，超长输入报错，不静默截断。

```powershell
& 'D:\OpenViking\.venv\Scripts\python.exe' scripts/experiments/local_ai_bridge.py `
 --model-path 'D:\OpenViking\models\bge-small-zh-v1.5-f16.gguf' `
 --enable-codex --evidence-dir '<实验原始请求目录>'
$env:GORELAY_AI_BACKEND='http'
$env:GORELAY_AI_BASE_URL='http://127.0.0.1:6348/v1'
$env:GORELAY_AI_MODEL='gpt-6.1-sol'
$env:GORELAY_AI_API_KEY='loopback-experiment'
$env:GORELAY_EMBED_BASE_URL='http://127.0.0.1:6348/v1'
$env:GORELAY_EMBED_MODEL='bge-small-zh-v1.5-f16'
$env:GORELAY_EMBED_DIMENSIONS='512'
$env:GORELAY_EMBED_API_KEY='loopback-experiment'
```

这里只监听 loopback，示例值不是账户密钥；无原生执行器、自动重试或多 Agent。模型名是请求配置值，用量来自上游返回；本地 X-Request-ID 用于对账，不冒充上游请求 ID。适配器未返回 cached Token 时不估缓存账单，订阅 Token 也不折算 API 人民币费用。`--tls-max-1-2` 是本机代理/Python TLS 1.3 握手失败时的可选兼容配置，保留证书校验；默认不启用，不修改全局代理或 OpenViking 配置。

原始证据：`output/agent-evidence-20261009-continued/`。规划修正前/后使用同一 HTTP 推理适配器，不能把 CLI→HTTP 的差异计入 Agent 规划优化收益。

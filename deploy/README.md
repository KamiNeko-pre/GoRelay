# GoRelay Agent Loop 部署与恢复

用途：给一个公开信息整理目标，Agent 检查已有知识和 Lua 能力，发现缺源后以 Code as Action 生成或组合采集动作，消费真实反馈继续调整，最终交付有出处的记录或明确停止。可靠队列、版本合同、索引与运行预算支撑这条主线。

## 干净 Linux 主机启动

安装 Docker Engine + Compose v2，从仓库根目录执行。建议先用 2 CPU / 4 GiB 内存做小规模验收，实际规格写进实验 manifest；这不是容量或 SLA 承诺。

```bash
cp deploy/.env.example deploy/.env
# 编辑四个独立随机凭证；建议 openssl rand -hex 24。
docker compose --env-file deploy/.env -f deploy/compose.yml config --quiet
docker compose --env-file deploy/.env -f deploy/compose.yml up -d --build --wait
curl -f http://127.0.0.1:5088/healthz
```

浏览器打开 `http://127.0.0.1:5088/agent`，输入本机 .env 的管理员 token。数据库、Redis、Qdrant、runner 和 pprof 不公开。入口也保护旧任务 API，避免绕过管理员接口。Go 进程非 root；runner 无宿主文件或 Docker socket 挂载，限制 1 CPU、192 MiB、32 PID，生成代码在一次性子进程运行。内存耗尽可能使整个 runner 重启：Worker 记录失败/未知结果，不能宣称单请求绝对隔离。

默认未配置模型、Embedding、搜索，不发付费请求；四个核心函数已有实现，业务端到端是否可用以实际验收记录为准。配置真实 provider 后才启用 `--profile indexing`；向量长度必须填写模型实际输出长度。不要把挂起状态或测试驱动当完整 Agent 演示。

正式付费 API 还必须配置费用策略：复制 `configs/agentbench/spend-policy.example.json` 到 `deploy/spend-policy.json`，填写已核实的模型价格、价格日期、Token 和费用上限；在 `deploy/.env` 设置 `GORELAY_SPEND_POLICY_FILE=./spend-policy.json`、`GORELAY_SPEND_POLICY_PATH=/app/configs/spend-policy.json`。Compose 将文件只读挂载给应用、Worker、Publisher、Indexer；文件缺失会拒绝启动挂载，不自动创建空目录。默认路径为空、模板限额为 0，不能误当成已批准付费配置。`GORELAY_RUN_MODEL_CALLS` 和 `GORELAY_RUN_SEARCH_CALLS` 单独限制调用次数；次数上限不能替代 Token／费用预留。临时 Codex CLI 测试只在本地授权后运行，不作为此 Linux API 部署的默认传输。

生产域名在 .env 设置 `GORELAY_SITE_ADDRESS=你的域名`、`GORELAY_BIND=0.0.0.0`、端口 `80/443`，DNS 指向服务器；Caddy 自动申请 HTTPS。当前只验收本地同构容器，公网 DNS、TLS、服务器性能需另测。

`GORELAY_DEMO_DAILY_RUNS=20` 按 UTC 日在 MySQL 原子限制新目标；重复 request_key 不再扣额度，重启不清零。每目标已有 15 分钟及模型/搜索/工具/HTTP 预算。日额度是 **新任务数**，不是精确人民币限额；管理员知识问答/手动导入不在此额度内，公开演示不要共享管理员 token。

## 交付检查

1. 给两类目标各一条，只给目标/范围，不手工给选好的 URL；检查 AI 计划、来源、缺口、多个子任务、结果和字段出处。
2. 让一个来源失败后中断并重启 app/worker，检查原 Run/成功子任务复用、预算未清零及下一轮真实调整。
3. 新任务复用已经注册的参数化 Lua 能力及当前数据；停用来源后旧知识不能当当前证据。
4. 运行 [评测说明](../configs/agentbench/README.md)，保存首创建/复用、失败成本和坏结果。本地夹具通过与真实模型／上游业务通过分别记录。

## 持续任务工作台

1. `/agent` 的“持续任务与机会待办”支持手填目标或导入任务配置 JSON。示例为 [Steam 中国区机会配置](../inputs/steam-opportunities-monitor.json)：优惠至少 80% **或**现价 ≤30 元，币种 CNY 且可购买，不固定游戏，不要求中文。业务字段只在配置中。
2. 导入后点击“创建暂停的持续任务”。创建、导入和列表读取均不执行模型；“手动刷新一次”才受理 Run。首次由 Agent 自主找公开源并验证／注册 Lua，不人工补执行结果。
3. 成功采集形成可复用计划；后续刷新仍走原预算、Outbox、Redis、Worker 和字段证据校验。事实任务的有效计划不再调用模型；来源隔离、版本变化或结果失效会回到 Agent Loop，仍受任务预算约束。需要语义总结的任务不会套用无模型完成捷径。
4. 工作台每个任务显示最近 10 条待办，不截断持久化证据。空 `spec.entities` 表示动态发现，不是首轮对象白名单。当前显示为最近事件，不宣称最优性价比排名或全商店覆盖。
5. 价格单位在合同中声明（示例为整数分：3000 = 30 元）。通用必需条件用 AND；`condition_groups` 为 OR 的 AND 分组。修改纯筛选条件或间隔保留采集程序，改变目标／字段／来源范围清除旧计划。
6. 缺价、过期或失败不会变成免费／已停售；旧机会会标为“未确认当前有效”。新鲜不匹配才撤销。已看／忽略带当前 Run 版本校验，避免把未看到的新结果确认掉。
7. 完成真实业务验收和成本授权后再点“启用周期任务”。默认 24 小时，每项任务有自己的周期；暂停只阻止后续调度，不取消已受理 Run。周期失败持久化并退避，不启动无限补调循环。

本地验证：`output/monitor-closure-20261010/monitor-reuse-closure.json` 使用脚本化模型响应及 HTTP 回放，真实 SQL、Redis、Lua、Worker 和运行循环完成首轮＋复用（第二轮模型调用 0）。它验证实现链路，**不证明真实 Agent 自主找到 Steam 数据，也不能作为实际模型 Token 降幅写入简历**。真实业务、直接 Agent 对照与服务器运行观察仍须另验收。

镜像标签使用已验收的构建标识或 digest，保存 `docker compose images --format json`、`git rev-parse HEAD` 和工作区差异（当前未提交改动不能只用 commit 表示）。应用回滚修改 `GORELAY_IMAGE_TAG` 并 `up -d --no-build`；含 schema 变更先确认兼容性。连接器回滚使用工作台已有版本操作，保留历史任务快照，不用覆盖 Lua 文件代替版本切换。

## 一致备份与独立恢复

```bash
bash deploy/backup.sh gorelay ./backups/20261005
bash deploy/restore.sh gorelay gorelay-restore-20261005 ./backups/20261005
# audit 比较表数量及关键 Run/预算/版本，然后以新的端口启动恢复实例。
GORELAY_HTTP_PORT=5188 GORELAY_HTTPS_PORT=5543 \
 docker compose --env-file deploy/.env -f deploy/compose.yml -p gorelay-restore-20261005 up -d --wait
```

备份短暂停止入口和所有写入进程，SQL 包括正文版本、连接器、预算、任务关联、索引缓存/工作/用量、每日额度；Qdrant 停写后备份卷。Redis 是派生队列：恢复到空 Redis，再按 SQL 补投 queued/running 且所属 Run 未取消、未终止、未过期的任务。原预算/执行快照/租约不重置；等原租约过期接管。未知模型调用只保留未知回执，不自动重复。

恢复脚本只接受全新项目和卷，不覆盖原实例，不执行全局 prune。MySQL 与 Qdrant 必须来自同一暂停点；若 Qdrant 缺失或备份不完整，保持应用关闭，不把 SQL ready 状态当恢复成功。此版本提供成对备份恢复，尚未提供在线空 Qdrant 自动全量重建。备份含原始数据和运行内容，保存在受限目录，不提交到 Git。

无需删除原卷的回滚：停止恢复实例，继续原实例。不要使用 `down -v` 清理生产数据。

# 亲手抓火焰图：先留基线，再定位，最后只改一处

这一步由用户亲自执行采样、读图和选择优化点，助手只准备负载/命令并协助核对。四核心及真实模型未接通，当前不进行付费Agent压测。

## 已留与未留的基线

- 工程原始记录：`docs/verification/ai-day6-7-20261005` 的请求样本/故障/构建，以及 `ai-final-gaps-20261005` 的当前验证记录。
- 源码封存：`docs/verification/profiling-baseline-20261005/manifest.json`、`source.zip`、`tracked-changes.patch`。包含未提交实现及本页负载，密钥和运行数据不进入源码包。封存的是**当前优化前**版本；不能称精确的“国庆升级前全量快照”。Git HEAD是历史参考，10月1日备份只有三个文件。
- Agent业务基线：未测。T0质量/真实调用费用、新源接入与能力/知识复用收益仍须真实服务及冻结任务/初态/真值；对照工具准备好不等于数据已经取得。

## 第一次采样选这条路径

固定公开资料任务中的“页1→两条详情→页2→一条详情→类型化结果”，使用已有HTTP回放数据，运行真实 `RunProgram`、Lua VM与解析函数。任务必须始终正确输出3条记录、5个读取动作。

它是**离线Code as Action执行内核**，不包括模型、真实网络、数据库、队列或容器进程启动。它能让你练习定位CPU/分配瓶颈；收益只属于此负载，不能直接写成Agent端到端提速。真实业务运行后再确认该热点是否影响交付时延。

生成代码在独立runner子进程运行：抓API/Worker的CPU不会自动抓到子进程Lua CPU。以下benchmark让相同执行内核跑在被pprof采样的测试进程中，避免抓错进程。

## 你亲手执行（PowerShell，仓库根目录）

先关闭占用CPU的其他实验，保持同Go版本/机器/GOMAXPROCS和输入。不要同时运行两个基线。以下不调用模型或公网服务：

```powershell
$env:GOMAXPROCS='2'
New-Item -ItemType Directory -Force output/profiling/before | Out-Null

# 不带采样测性能，保存多轮原始值。
go test ./internal/engine -run '^$' -bench '^BenchmarkCodeAsActionReplay$' `
 -benchtime=3s -count=5 -benchmem |
 Tee-Object output/profiling/before/benchmark.txt

# 单独一次CPU + 分配采样，不把带采样的速度当正式性能基线。
go test ./internal/engine -run '^$' -bench '^BenchmarkCodeAsActionReplay$' `
 -benchtime=30s -count=1 -cpuprofile output/profiling/before/cpu.pprof `
 -memprofile output/profiling/before/heap.pprof `
 -o output/profiling/before/engine.test.exe |
 Tee-Object output/profiling/before/profile-run.txt

go tool pprof -top output/profiling/before/engine.test.exe output/profiling/before/cpu.pprof
go tool pprof -http=127.0.0.1:6068 -no_browser `
 output/profiling/before/engine.test.exe output/profiling/before/cpu.pprof
```

浏览器打开 `http://127.0.0.1:6068/flamegraph`。本机Go自带pprof默认支持火焰图；调用图视图若要求Graphviz是另一视图的依赖，当前本机未发现dot，不先安装额外工具。Ctrl+C关闭pprof浏览器服务。

分配问题用：

```powershell
go tool pprof -alloc_space -top `
 output/profiling/before/engine.test.exe output/profiling/before/heap.pprof
```

`alloc_space`看累计分配，`inuse_space`看采样时仍存活对象；不能拿累计分配量冒充峰值RSS。

## 读图后只回答当前一步

沿调用链找占比最大的分支：**为什么该任务经过这里 → 是重复工作、数据转换、分配还是VM执行 → 哪层拥有它 → 能改什么且不损害结果**。先记录假设，再改一处。火焰图宽度代表样本占比，横向排列不代表时间先后；CPU图看不到模型/网络/SQL等待耗时，墙钟慢但CPU低时要查阶段计时/阻塞或trace，不能硬找CPU优化。

把图和你认为最宽的调用链发给助手，我们在当前链路上分析；助手不提前替你选瓶颈或改优化。

## 优化后对照

1. 保存变更diff及原因，确认正常/分页/预算/取消/输出边界测试。
2. 使用相同fixture、Go、机器和GOMAXPROCS，在`output/profiling/after`重复不采样5轮及单独采样；保留before文件与二进制。
3. 比较分布、B/op、allocs/op及结果正确性；用`go tool pprof -base before/cpu.pprof after/cpu.pprof`辅助看变化。热点或差分火焰图不能替代正式速度对照。
4. 只有收益稳定、质量不退步且在代表任务上仍成立，才进入简历。没有真实热点/收益就保留结论，不制造慢基线。

真实Agent性能实验另用 [TASK_EXPERIMENTS.md](TASK_EXPERIMENTS.md)，将模型往返、执行/排队、索引/检索、每正确任务费用分开。火焰图优化是其中一条工程证据，不替代Agent业务亮点。

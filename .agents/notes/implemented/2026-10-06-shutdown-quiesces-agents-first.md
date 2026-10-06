# 关闭时先让所有 agent 静止，再撤销工具与资源

- Status: implemented
- Date: 2026-10-06

## Context

整体审查发现，插件按启动顺序逆序 cleanup，而 agent registry、root bootstrap、subagents 和 goals 都在工具 provider 与 jobs 之前启动，所以关闭时工具先撤销、jobs 先停、shell 临时目录先删，agent turn 要到更晚的 subagents、root 或 registry cleanup 才被取消。后果：

- 被 jobs teardown 终止的前台 `bash` 返回后，turn 继续下一个 step，关闭期间还会再发模型请求。
- 之后的工具调用得到 unknown tool 结果。
- 后台 continuable 子代理和 goal 轮次扩大了这个窗口。

`Registry.stop` 逐个关闭 agent，`Bootstrap` 也先单独关闭 root，所以一个 agent 排空时，其他 agent 仍可能被 notice 唤醒并开启新 turn。

非目标：shell provider 对 job 上限回退路径进程的跟踪见[前台 job 交接 Note](2026-10-06-foreground-job-handoff-and-shell-cleanup.md)；本 Note 的决定不改变工具、job 或 agent 的运行期语义。

## Decision

权威描述在[架构](../../../docs/architecture.md#plugin-与-scope-生命周期)的启动顺序和关闭步骤中。

- `cmd/nano-harness` 把 agent registry、root bootstrap 和 goal driver 放到最后启动（只排在前端之前）；subagents、goals、全部工具 provider 和 jobs 都在 registry 之前启动。逆序关闭因此依次是：前端、goal driver（停止轮次）、registry（关闭全部 agent）、工具、jobs、shell 临时目录，然后才是 delegation 与 goal 服务、spill、session、engine。所有工具 provider 的 `Start` 只注册工具或上下文，不依赖 registry，所以提前启动没有影响。依赖仍由构造函数显式注入。
- `Registry.stop` 并发关闭所有 agent：每个 worker 立即取消在途 turn、丢弃排队的 turn 与 notice，registry 再等待全部 worker 回收。
- `Bootstrap` 的 cleanup 只撤销 root 的发布，root 由 registry 与子代理一起关闭；`Start` 发布失败时仍立即关闭 root。
- 子代理和 goal 服务在 registry 之后关闭时，对已关闭 agent 的 `Close` 返回 not found（已被忽略），jobs 已停止时 `Release` 立即返回，registry 停止后的 `Notify` 被丢弃，这些路径都已存在，不需要修改。

## Consequences

关闭期间不再有 turn 在工具撤销之后继续运行。前台 `bash` 总是随 turn 取消被终止并回收，然后才删除临时目录；关闭后没有新的模型请求，也没有 unknown tool 结果。

代价与风险：

- registry 同时关闭全部 agent，慢 agent 不再按顺序排队，关闭耗时取最慢的 agent。
- 关闭出错时的错误聚合顺序随并发而变。
- 子代理与 goal 服务现在晚于工具关闭，它们的清理只做句柄与 watcher 回收。
- 新增运行时组件时必须遵守顺序契约：会开启 turn 的组件放在 registry 之后，被 turn 使用的组件放在 registry 之前。结构测试会拒绝打乱 agent 层位置的修改。

## Verification

- `TestComposition_ShutdownQuiescesAgentsBeforeToolsAndTemporaryFiles`（真实 composition + 主机 bash）：root 第一步启动一个 continuable 子代理（其模型请求阻塞到被取消），并运行写 `$TMPDIR` 的前台心跳；写失败时会在 workspace 留下 `lost`。关闭后断言：
  - 子代理请求在 `Shutdown` 返回前被取消；root 只发出一次模型请求，turn 结果为 canceled；
  - `bash` 结果为 `Error: tool call aborted`，transcript 中没有 unknown tool；
  - `lost` 不存在，临时目录已删除，心跳 PID 已不存在（`kill(pid, 0)` 返回 ESRCH）。
  - `-count=8 -race` 稳定。
- 旧顺序反例：同一行为测试在旧插件顺序下 3 次中失败 1 次（root turn 以 `context canceled` 错误结束，而不是取消），失败是竞态造成的。`TestComposition_StartOrderEncodesShutdownQuiescence` 在旧顺序下必定失败，打印出完整插件顺序；新顺序下通过。
- `TestRegistry_StopCancelsEveryAgentBeforeWaiting`：两个 agent 的 turn 都要等两者都被取消后才能结束。改回逐个关闭时测试在 5 s 超时失败（"registry stop waited on one agent before cancelling the others"），并发关闭时通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0，lint 0 issues，逐文件 coverage 100%，22 个 mutation 用例全部 killed，build 通过。
- `make tui-e2e`：通过（19 个根工具调用、子代理、审批、打断、恢复与退出清理）。
- 未覆盖：子代理内部的前台 bash（delegated 策略为 `never`，子代理不能运行 bash），以及 job 上限回退路径的进程（WP3 负责）；这两种情况 turn 取消后的等待路径与 root 相同。

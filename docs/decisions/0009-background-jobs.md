# ADR-0009：后台任务运行时、job 工具与完成通知

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers

## 背景

这是[工具对齐计划](../../.agents/notes/proposed/2026-10-04-upstream-tool-parity.md)的 WP3。[ADR-0007](0007-upstream-base-tool-definitions.md) 让 `bash` 暂用上游 `enableRunInBackground: false` 的前台变体：`timeoutMs` 到期即终止进程组，没有 `run_in_background`，也没有 `job_*` 工具。

参考提交 `5badb15009ae` 的 Base 组合加载 `dsh-jobs-local` 与 `dsh-tool-jobs`，`tool-bash` 使用默认配置（`enableRunInBackground: true`、`promoteOnTimeout: true`）。上游语义：

- job 属于启动它的 agent session，ID 为 `<kind>-N`，状态为 `running`、可选 `stopping`，再到 `completed`、`killed` 或 `failed` 之一；settle 先到先得。
- 每个 job 有一个有界输出环，模型通过注册表保存的游标做消费式读取；settle 后第一次读取还交出 producer 的值结果。
- `job_output` 的 `wait` 默认 30 s、上限 10 min；超时的 wait 不终止 job。`job_kill` 的 reason 并入终止 detail。
- 每次 `bash` 调用都在启动时注册为 job：后台调用立即返回 ID；前台调用等待到超时，到期后同一个 job 继续在后台运行；注册被拒（达到 owner 上限）时退回到期即终止的执行方式。
- job settle 后，若没有 wait 收走结果、不是模型自己 kill 的、也不是 owner 或服务销毁，`tool-jobs` 构造 `user/message`（source kind `tool-jobs`）：owner 忙时 `agent.inject()` 到下一个 step 边界，空闲时默认 `followup()` 开启新 turn。上游 inbox 持久化为 `agent/inbox/spliced` 事件。
- 上游进程内注册表中的 job 随 harness 进程结束；恢复后不保留 job。

本仓约束：所有运行时 effect 由插件 Scope 回收；模型可见信息必须能从权威会话事件重建；单个 tool result 上限 256 KiB；followup 与 steer 当前是内存队列，不持久化。

非目标：完整输出 spill 文件（WP2）、subagent 后台运行（WP7）、job 的 UI 列表与人工 kill、跨进程或可恢复的 job。

## 决策

### 运行时

`internal/app/job.Service` 是插件 `jobs`，提供进程内注册表：

- producer 用 `Launch(Spec)` 提交 `Kind`、`Label`、`Owner` 和阻塞式 `Run(ctx, *Output) Outcome`。服务在自己拥有的 goroutine 中运行 `Run`，`Kill` 和关闭会取消 `ctx`；producer 在资源释放后返回结果。producer panic 被收敛为 `failed`。
- 所有读写和控制操作都带调用方 session；访问他人 job 返回 `job <id> belongs to another session`，未知 ID 返回 `unknown job <id>`。ID 可预测，所以边界是所有权而不是保密。
- ID 计数按 kind、在一个服务实例内递增。前台 `bash` 调用也会消耗编号，所以第一个后台 job 可能是 `bash-2`，与上游一致。
- 每个 owner 最多 10 个 `running` 或 `stopping` 的 job。输出环在运行中保留 128 KiB；上游保留 256 KiB，但本仓一次读取连同状态行必须放进 256 KiB 的 tool result，减半后完整读取永远不会被统一截断。settle 时保留全部未读字节，settle 后第一次读取把保留量裁到 16 KiB。游标落到保留窗口之前时，读取追加 `[some output was dropped from memory; full output: <文件>]`，列出 job 当前声明的完整输出文件（见 [ADR-0008](0008-tool-output-spill-and-observation-policy.md#bash-完整输出)），没有文件时为 `(unavailable)`。
- `Output` 的 stdout/stderr writer 暂存被拆开的 UTF-8 尾部，字符跨进程写入时仍整体进入输出环。
- 关闭顺序：拒绝新 job，把活动 job 标为 `stopping` 并取消，等待全部 producer goroutine 返回，然后丢弃记录。job 的 context 来自插件启动 context，进程收到终止信号时运行中的命令同样停止。
- 已结束的 job 一直列出，直到前台调用把它移除或服务关闭；没有保留数量上限，与上游相同。每次 `bash` 都需要用户审批，job 数量受人工节奏约束。

本仓暂不实现上游的 controller 挂载检查、非消费式观察读取、progress 行和 owner 销毁时的 job 清理：当前 composition 总是同时注册 job 工具；没有 UI 观察者；`bash` 没有 progress；WP3 中能启动 job 的只有根 agent，它在 `jobs` 之后关闭。WP7 引入可在服务运行期间关闭的 owner 时，必须同时增加 owner 释放。

> 已被取代：暂缓 owner 销毁清理的部分由 [ADR-0013](0013-background-continuable-subagents.md#6-job-的-owner-释放) 取代，child 关闭时释放其 job；其他暂缓项保留。

### job 工具

`internal/adapter/tool/job` 是插件 `job-tools`，注册与上游逐字节一致的 `job_output`、`job_list`、`job_kill`：

- `job_output` 可选先 wait（默认 30 s、上限 10 min，超过上限按上限，非正值报 `invalid wait timeout`；wait 被取消返回 `tool call aborted`），然后消费式读取。文本依次为 stdout、`[stderr]` 段、丢失提示、只交出一次的值结果；为空时显示 `(no new output)`，最后一行是 `[status: <status>]` 或 `[status: <status>, <detail>]`。
- `job_list` 每行一个 `<id> [<kind>] <status> — <label>`，没有 job 时显示 `(no background jobs)`。
- `job_kill` 对活动 job 返回 `requested cancellation of job <id>`，对已结束 job 返回 `job <id> had already finished <status line>`。reason 在 job 以 `killed` 结束时并入 detail，例如 `signal: SIGKILL; not needed`；调用参数本身也作为 `tool/call` 持久化。
- 三个工具都是 exclusive，与上游未声明并发安全一致。上游 `tool:jobs` 段落以 section order 1600（`appTool.OrderJobs`）逐字采用，挂在 `job_output` 上，只在该工具可见时出现。

### bash

`bash` 改为 Base 的后台变体：在 `workdir` 之后声明 `run_in_background`，`timeoutMs` 使用 “moves to the background as a job instead of being killed” 描述，升级字段排在其后。

- 每次调用在审批和执行点检查之后注册为 kind `bash`、label 为命令文本的 job。job 中的进程没有 runner 截止时间，只在自行结束、`job_kill` 或关闭时停止；取消时 runner 终止整个进程组并等待退出。
- `run_in_background: true` 立即返回 `started background job <id>`，不应用 `timeoutMs`。审批原因为 `run a background shell command in the workspace sandbox: <description>`；升级请求仍使用升级原因。
- 前台调用等待 `timeoutMs`（默认 60 s、上限 10 min）。及时结束时移除 job 记录，按原有前台格式渲染（stdout/stderr 各保留最后 64,000 字节、退出码、信号和 sandbox 标记）。超时时做一次消费式读取，返回已有输出和 `[still running after <N>ms; moved to background job <id>]` 及上游的后续说明；之后的 `job_output` 从这次读取之后继续。
- 调用被取消（或服务在等待期间关闭）时，以 reason `tool call aborted` kill 该 job，等待其结束并移除，返回 `tool call aborted`。owner 达到 job 上限时，后台调用返回上限错误，前台调用退回到期即终止的执行方式并可能返回 `[timed out after Nms]`。
- job 结局：信号终止为 `killed`（`signal: <name>`，未启动即取消为 `killed before exit`），正常退出为 `completed`（`exit code: N`，sandbox 拒绝时追加拒绝标记和升级提示），无法启动或 sandbox 不可用为 `failed`。
- approval、sandbox、`danger-full-access` 升级与 delegated 拒绝语义不变，后台命令同样经过它们。
- 后台命令使用 shell provider 的临时目录作为 `TMPDIR`。composition 中 `jobs` 在 `shell-tools` 之后启动，所以关闭时先结束全部后台进程，再删除该目录。

### 完成通知

job settle 时，如果有正在进行的 wait 收走了结果、settle 由 `Kill` 引起、服务正在关闭或插件启动 context 已取消，则不发通知。否则服务通过消费方接口 `job.Notifier` 调用 agent `Registry.Notify`，文本为：

```text
background job <id> (<kind>: <label>) finished <status line>. Read its output with job_output.
```

通知是 role 为 user、source kind 为 `tool-jobs` 的消息，由 `Agent.Notify` 投递：

- agent 忙时进入内存队列，在下一个边界作为 `user/message` 追加到当前 turn：turn 开始后、工具 step 结束后（steer 之后），以及模型给出无工具调用的回答之后。最后一种情况下 turn 不结束，而是再开一个 step 回应通知；已经是最后一个允许的 step 时通知留在队列中。
- agent 空闲时，或一个 turn 结束后队列中仍有通知且没有排队的 turn，worker 以最早的通知开启新 turn，其余通知在该 turn 开始时追加。这对应上游默认的 `wakeup` 投递；本仓不设 `maxConsecutiveWakes`，与 Base 默认相同。
- 被取消的 turn 留下的通知等待下一个 turn，不会在用户 interrupt 后立即自动开 turn。`WhenIdle` 把已唤醒但尚未开始的通知 turn 视为忙。
- 待投递的通知与 followup、steer 一样只在内存中，agent 停止时丢弃；此时 job 本身也已被终止。

> 后续约束：[ADR-0013](0013-background-continuable-subagents.md#6-job-的-owner-释放) 规定 one-shot agent 只在唯一 turn 运行期间接受通知，turn 结束后不再由通知唤醒；本节的通用唤醒规则受此限制。

> 后续停止契约：[ADR-0018](0018-goal-stop-outcomes.md) 规定输出截断的 turn 不消费待投递通知，通知留给下一个 turn；本节的提交点受此停止规则约束。

模型可见的通知只通过已提交的 `user/message` 进入 surface。session v2 的记录类型、字段和校验都不变：source kind 本来就是开放字符串，order validator 已允许活动 turn 内任意位置的 `user/message`。新出现的因果形态（无工具调用的 step 之后出现 `user/message` 并继续 step）也由现有 validator 接受，resume 修复规则不变。TUI 把这类消息显示为 `job> `，而不是 `you> `。

> 后续格式：[ADR-0016](0016-long-running-goals.md#领域与记录) 为目标轮次增加 `MessageSource` 归属字段和严格折叠校验；`tool-jobs` 通知仍沿用本节形态。

### 恢复与身份

job、计数器和待投递通知都不持久化。恢复后旧 transcript 中的 job ID 对 `job_*` 工具是 `unknown job`，新进程的编号从 1 重新开始。composition ID 改为绑定 `shell-tools-v2` 和新增的 `job-tools-v1`，旧会话按 composition mismatch 拒绝恢复；本仓尚无发布 tag，没有已发布的用户会话需要迁移。

> 已被取代：本节的 `shell-tools-v2` 由 [ADR-0008](0008-tool-output-spill-and-observation-policy.md#身份) 提升为 `shell-tools-v3`；此处保留后台任务落地时的身份。

### Subagent

delegated agent 可以调用对其可见的 `job_*` 工具，但只能访问自己的 job；它的审批策略固定为 `never`，所以无法通过 `bash` 启动 job。WP7 的 subagent 后台运行复用 `Launch`，用 `Outcome.Result` 交出报告，并负责上文的 owner 释放。

## 后果

模型在本仓与上游看到相同的 `bash` 与 `job_*` 定义，长命令不再因超时被杀，完成后无需轮询即可得知。job 运行时与种类无关，WP7 可以直接接入 subagent。

代价与风险：

- 通知可以在没有用户输入时开启 turn 并消耗模型调用；每个通知最多触发一个 turn，自激链需要模型反复启动新 job，而每次启动都需要用户审批。
- 进程退出、崩溃或 shutdown 会终止全部 job，并丢弃尚未投递的通知；用户需要重新运行命令。
- 前台命令现在多一次 job 注册和输出复制（输出环最多 128 KiB）；内存上限为每 owner 10 个活动 job。
- 输出环保留量与上游不同；超出保留窗口的输出在 WP2 的 spill 落地前无法找回。

> 后续决定：[ADR-0008](0008-tool-output-spill-and-observation-policy.md#bash-完整输出) 已保存完整 shell 输出并补齐定位符；[ADR-0013](0013-background-continuable-subagents.md#2-生命周期) 增加无需用户审批即可启动的后台 subagent job，因此“每次启动都需要用户审批”只适用于 `bash`，不覆盖所有 job producer。

## 被否决方案

- 持久化 job 记录或通知队列：job 进程随 harness 结束，没有可恢复的执行；持久化的待投递通知会在恢复后描述一个已不存在的 job。若未来需要可恢复 job，应连同执行后端重新设计。
- 新增 `job/notice` 记录类型：上游本身以 `user/message` 送达，现有 replay、compaction 和 provider 映射无需改动即可处理；新记录类型只增加格式面。
- 只在前台超时时才注册 job：输出缓冲需要在提升时迁移，命令在超时前不可见，也改变了 job 编号与上游的一致性。
- 输出环保留 256 KiB：读取加状态行可能超过 tool result 上限，被统一截断后丢掉状态行。
- 由 job 工具适配器投递通知（上游 `tool-jobs` 的做法）：适配器需要依赖 agent registry，且通知是否已被 wait 或 kill 收走只有注册表知道；由服务通过消费方 `Notifier` 投递只有一个 owner。
- 把 job 工具声明为并发：上游没有声明，`job_output` 的 wait 也会让同批次的其余调用与之重叠，改变调用顺序语义。

## 复审触发条件

- WP7 接入 subagent job，需要 owner 释放、progress 或值结果的新约束。
- WP2 的 spill 存储落地，丢失提示和截断提示需要报告完整输出路径。
- 增加 job 的 UI 列表、人工 kill 或观察读取。
- 观察到通知引起的连续自动 turn，需要上游的 `maxConsecutiveWakes` 或 `quiet` 投递。
- 参考指针更新改变了 jobs、tool-jobs 或 tool-bash 的定义或语义。

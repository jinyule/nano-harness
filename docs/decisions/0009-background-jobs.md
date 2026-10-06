# ADR-0009：后台任务运行时、job 工具与完成通知

- 状态：Accepted（待投递完成通知的持久化与恢复部分被 ADR-0023 取代）
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

前台完成通知必须覆盖注册到等待、超时到输出读取的交接空档，不能依赖 producer 与调用方的调度顺序。达到 job 上限的回退执行也必须有 Scope 所有者，保证受管 runner 与输出收尾在临时目录删除前结束；后代进程回收还受平台与执行模式限制。

非目标：完整输出 spill 文件（WP2）、subagent 后台运行（WP7）、job 的 UI 列表与人工 kill、跨进程或可恢复的 job。

## 决策

### 运行时

`internal/app/job.Service` 是插件 `jobs`，提供进程内注册表：

- producer 用 `Launch(Spec)` 提交 `Kind`、`Label`、`Owner`、可选 `Foreground` 和阻塞式 `Run(ctx, *Output) Outcome`。服务在自己拥有的 goroutine 中运行 `Run`，`Kill` 和关闭会取消 `ctx`；producer 在资源释放后返回结果。producer panic 被收敛为 `failed`。`Foreground` 在注册锁内预留完成收集权，不计入实际 `Wait` 调用数，详见[完成通知](#完成通知)。
- 所有读写和控制操作都带调用方 session；访问他人 job 返回 `job <id> belongs to another session`，未知 ID 返回 `unknown job <id>`。ID 可预测，所以边界是所有权而不是保密。
- ID 计数按 kind、在一个服务实例内递增。前台 `bash` 调用也会消耗编号，所以第一个后台 job 可能是 `bash-2`，与上游一致。
- 每个 owner 最多 10 个 `running` 或 `stopping` 的 job。输出环按解码后的 UTF-8 文本字节计量，运行中保留 128 KiB；上游保留 256 KiB。本仓较小的窗口为结果包装留出空间，但不保证值结果、状态 detail 或完整输出定位符也能全部放入 256 KiB。`job_output` 单独预算最终信封：状态行最多 4 KiB，过长 detail 用 `…]` 收尾；丢失提示最多 4 KiB，定位符超限时显示 `(unavailable)`；剩余预算先给双流输出（为丢失提示留出位置），再给值结果，按 rune 边界截断并追加 `[output truncated]`；双流输出已占满预算时省略值结果。状态与丢失提示总能保留，spill 不可用时也不会被统一字节截断挤掉。settle 时保留全部未读字节，settle 后第一次读取把保留量裁到 16 KiB。游标落到保留窗口之前时，读取追加 `[some output was dropped from memory; full output: <文件>]`，列出 job 当前声明的完整输出文件（见 [ADR-0008](0008-tool-output-spill-and-observation-policy.md#bash-完整输出)），没有文件时为 `(unavailable)`。
- `Output` 的 stdout/stderr writer 各自暂存不完整的 UTF-8 尾部，字符跨写入时整体进入输出环；非法序列按解码器的最大合法前缀替换为 U+FFFD，最后的不完整序列在 settle 前替换。游标与保留量均使用替换后的文本字节，不使用原始进程字节。完整 shell spill 仍保存原始字节。
- 关闭顺序：拒绝新 job，把活动 job 标为 `stopping` 并取消，等待全部 producer goroutine 返回，然后丢弃记录。job 的 context 来自插件启动 context，进程收到终止信号时运行中的命令同样停止。
- 已结束的 job 一直列出，直到前台调用把它移除或服务关闭；没有保留数量上限，与上游相同。每次 `bash` 都需要用户审批，job 数量受人工节奏约束。

owner 销毁清理由 `job.Service.Release` 实现：先取消该 owner 的活动 job，等待 settle 后删除它的全部记录，不发完成通知；context 提前结束时返回取消错误，剩余记录由服务关闭清理。child 释放路径见 [ADR-0013](0013-background-continuable-subagents.md#6-job-的-owner-释放)。

本仓暂不实现上游的 controller 挂载检查、非消费式观察读取与 progress 行：当前 composition 同时注册 job 工具，没有 UI 观察者，`bash` 没有 progress consumer。

### job 工具

`internal/adapter/tool/job` 是插件 `job-tools`，注册与上游逐字节一致的 `job_output`、`job_list`、`job_kill`：

- `job_output` 可选先 wait（默认 30 s、上限 10 min，超过上限按上限，非正值报 `invalid wait timeout`；wait 被取消返回 `tool call aborted`），然后消费式读取。先检查 job 是否存在与 owner，再校验 wait timeout；未启用 wait 时忽略 timeout。文本按上游 `readBody` 的顺序依次为 stdout、`[stderr]` 段、丢失提示、只交出一次的值结果；三者都为空时显示 `(no new output)`，只有丢失提示时直接显示提示，最后一行是 `[status: <status>]` 或 `[status: <status>, <detail>]`。
- `job_list` 每行一个 `<id> [<kind>] <status> — <label>`，没有 job 时显示 `(no background jobs)`。
- `job_kill` 对活动 job 返回 `requested cancellation of job <id>`，对已结束 job 返回 `job <id> had already finished <status line>`。reason 在 job 以 `killed` 结束时并入 detail，例如 `signal: SIGKILL; not needed`；活动 job 连续 kill 时，省略 reason 保留旧意图，显式值（包括 `""`）替换旧意图。与上游 `jobs-local` 的 `${detail}; ${reason}` 相同，显式空 reason 也并入 detail，例如 `signal: SIGTERM; `；producer 没有 detail 时，空 reason 就是整个 detail，状态行为 `[status: killed, ]`；已结束 job 不更新理由。调用参数本身也作为 `tool/call` 持久化。
- 三个工具都是 exclusive，与上游未声明并发安全一致。上游 `tool:jobs` 段落以 section order 1600（`appTool.OrderJobs`）逐字采用，挂在 `job_output` 上，只在该工具可见时出现。

### bash

`bash` 改为 Base 的后台变体：在 `workdir` 之后声明 `run_in_background`，`timeoutMs` 使用 “moves to the background as a job instead of being killed” 描述，升级字段排在其后。

- 每次调用在审批和执行点检查之后注册为 kind `bash`、label 为命令文本的 job。job 中的进程没有 runner 截止时间，只在自行结束、`job_kill` 或关闭时停止；取消时 runner 向整个进程组发 SIGTERM，最多等待 3 s 后发 SIGKILL，并等待退出与输出收尾；TERM trap 可以执行。
- `run_in_background: true` 立即返回 `started background job <id>`，不应用 `timeoutMs`。审批原因为 `run a background shell command in the workspace sandbox: <description>`；升级请求仍使用升级原因。
- 前台调用从注册时起预留完成收集权，等待 `timeoutMs`（默认 60 s、上限 10 min）。及时结束时移除 job 记录，按原有前台格式渲染（stdout/stderr 各保留最后 64,000 字节、退出码、信号和 sandbox 标记）。超时时做一次消费式读取：读取时已结束的 job 仍按前台结果返回并移除，不发通知；仍活动时才返回已有输出和 `[still running after <N>ms; moved to background job <id>]` 及上游的后续说明，并允许后续完成通知。之后的 `job_output` 从这次读取之后继续。
- 调用被取消（或服务在等待期间关闭）时，以 reason `tool call aborted` kill 该 job，等待其结束并移除，返回 `tool call aborted`。`Wait` 超时返回活动状态之后、首次消费式 `Read` 之前再次检查取消；已取消的调用保留前台完成预留，走同一终止与结算路径，不交出后台 ID，也不发完成通知。owner 达到 job 上限时，后台调用返回上限错误，前台调用退回到期即终止的执行方式并可能返回 `[timed out after Nms]`。
- job 结局：信号终止为 `killed`（`signal: <name>`，未启动即取消为 `killed before exit`），正常退出为 `completed`（`exit code: N`，sandbox 拒绝时追加拒绝标记和升级提示），无法启动或 sandbox 不可用为 `failed`。sandbox runner 的致命诊断优先于文件拒绝：非零退出时匹配当前后端的 `sandbox-exec: ` 或 `bwrap: `，返回 `process.ErrSandboxUnavailable`。前台错误文本逐字采用上游 `SandboxUnavailableError` 在 workspace-write 模式下的消息 `sandbox mode "workspace-write" is requested but no sandbox backend is usable on this host; refusing to run the command unconfined. Install bubblewrap or run a Landlock-enforcing kernel (Linux), ensure sandbox-exec is usable (macOS), or ensure the ACL restricted-token runner can start (Windows) — otherwise switch the consumer to danger-full-access.`，runner 失败时追加 ` Runner failure: <匹配行>`（只去掉行尾 CR，与上游的 `/\r?\n/` 分行一致），以 `Error: ` 包装；没有可用后端时只有该消息。runner 可执行文件启动失败同样保留 sandbox 分类，` Runner failure: ` 之后是 Go 的启动错误，而不是上游 Node 的 `String(error)`。后台携带 `RunnerFailed` 事实，detail 按上游 `processOutcome` 先写 `exit code: N`（runner 未能启动时为上游同样的 `killed before exit`），再接 `; [sandbox: the sandbox runner itself failed under workspace-write mode — the command did not run; this is a sandbox problem, not a command failure]`，不建议权限升级；状态保留本仓的 `failed`，见参考分析。普通命令非零退出仍是 completed，不是 tool error。
- 已知限制：runner 失败的判定与上游 `classifyRunnerFailure` 相同，只要求非零退出且 stderr 某一行含当前后端前缀。普通命令自己向 stderr 打印含 `sandbox-exec: ` 或 `bwrap: ` 的行并以非零退出时，也会被报告为 sandbox 故障，stdout 与退出码不返回给模型。收紧匹配（例如只认行首）会偏离上游，需要单独决策。
- approval、sandbox、`danger-full-access` 升级与 delegated 拒绝语义不变，后台命令同样经过它们。
- 后台命令使用 shell provider 的临时目录作为 `TMPDIR`。job 上限回退执行由 shell provider 在自己的 Scope 下跟踪，保留调用方取消；准入与 cleanup 共用锁，关闭开始后不再增加执行贡献。`cmd/nano-harness` 先关闭 agent，再关闭 jobs，最后关闭 shell provider：等待全部 job producer 后，provider 拒绝新回退、取消并等待全部回退 runner 与 spill 收尾，再删除该目录。
- Scope 的 join 只证明受管执行静止。Linux workspace sandbox 有 PID namespace；macOS `sandbox-exec` 没有，`killpg` 无法保证终止调用 `setsid()` 或离开原进程组的后代。host 模式同样没有 namespace。完整平台与模式边界归[安全规则](../security.md#approvalshell-与进程)，更强回收保证需要独立容器、VM 或执行后端。

### 固定预算与托管环境

上游可配置的这些预算在本仓保持固定常量，不提供启动或热配置：

| 预算 | 本仓不变量与理由 |
|---|---|
| bash 前台等待 / 回退超时 | 默认 60 s、cap 600 s；限制一次 exclusive 工具占用的等待时间，后台 job 自管生命周期，前台超时交接不终止 job |
| 进程终止 | bash/job 的 TERM 宽限固定 3 s；进程退出或被 KILL 后，仍持有管道的后代输出再排空最多 3 s，与上游 `spawn.ts` 用 `graceMs` 作排空窗口相同，正常退出同样适用；给清理 trap 和后台子进程的输出时间，同时限制受管进程关闭等待；搜索零宽限立即 KILL，排空最多 1 s，理由见 ADR-0007 |
| 前台输出 | stdout/stderr 各保留 64,000 字节尾部；避免任意命令无限占用内存，完整流按 ADR-0008 保存 |
| job 输出 | 活动环 128 KiB，首次终态读取后 16 KiB，按解码后的 UTF-8 字节计量；最终信封按上文独立预算 |
| 活动 job 数 | 每 owner 最多 10 个 running/stopping job；限制一个会话同时拥有的执行与输出内存，不限制已结束记录数量 |
| job_output wait | 默认 30 s、cap 600 s；给消费式读取有界阻塞期限，超时不取消 producer |
| 最终工具文本 | session 固定 256 KiB；包含编码替换、标记与 metadata，不能只预算内部输出环 |

这些值固定产品的资源与交接契约，并让模型可见的边界、恢复结果和静止证据可复现。当前没有需要另一套预算的生产部署 consumer；增加配置会扩大组合验证面。调整预算须同时更新 owning 常量、边界测试与本文，不能静默放宽安全或信封上限。

关闭先同时取消所有活动 job；shell provider 同时取消全部回退执行，再等待各自返回。每个真实 runner 的终止阶段由 3 s 宽限加 3 s 管道排空约束，等待不按 job 数串行累加。前台取消另有 7 s 的结算等待预算（宽限、排空再加 1 s），不复用可能长达 10 min 的命令等待。该期限针对 OS 进程终止与管道回收，不承诺外部文件系统同步或不遵守取消契约的自定义 producer 的硬期限；Scope 仍等待全部受管执行静止。

托管变量只提供 `DSH_SHELL=1`、当前 `DSH_SESSION_ID` 和固定环境 allowlist。上游 `DSH_HOME` 指统一 Harness home，本仓 settings、credentials、session、spill、attachments 根均可独立部署，没有唯一等价目录；不把其中一个根伪装成 home，也不从父环境继承该变量。上游 `DSH_PROFILE` / `DSH_PROFILE_DIR` 描述启动器选择的 profile 及安装包目录，本仓使用编译时 composition，没有 profile 启动上下文，因此省略两者。它们不是仅因凭据隔离而被删除：首先缺少可诚实映射的托管事实。已有 workspace 事实由 `NANO_WORKSPACE` 给出。未来出现 profile 或统一 home consumer 时应按显式注入、allowlist 与启动校验重新评估，不建立无 consumer 的 contributor registry。

### 完成通知

job settle 时，如果前台完成收集权尚未释放、有正在进行的 wait 收走了结果、settle 由 `Kill` 引起、服务正在关闭或插件启动 context 已取消，则不发通知。前台预留覆盖 `Launch` 到 `Wait`，以及超时/取消的 `Wait` 返回到首次 `Read`/`Remove`，不依赖 producer 与调用方的调度顺序。否则服务通过消费方接口 `job.Notifier` 调用 agent `Registry.Notify`，文本为：

```text
background job <id> (<kind>: <label>) finished <status line>. Read its output with job_output.
```

label 是 `bash` 的命令原文或子代理的 description，长度只受工具参数预算约束，可能超过一个文本块（`session.MaxTextBytes`，256 KiB）。完整文本超出时按上游 `fitCompletionNotice` 截断：保留 `background job <id>`，接着在 UTF-8 字符边界截取 ` (<kind>: <label>) finished <status line>` 的开头，最后追加 `\n[notice truncated]\nDone; job_output.`，总长恰好不超过一个文本块。被截掉的状态行仍可由 `job_output` 读取。上游由 producer 声明 `outputLimitBytes`；本仓的上限就是 durable 文本块上限，不另设 producer 参数，也不在准入时拒绝长命令。

首次消费式 `Read` 在同一把锁内获取输出与状态并释放前台预留：终态由前台收集并 `Remove`，仍活动才交出后台 ID，后续 settle 可以通知一次。正常前台结束或取消通过 `Remove` 丢弃预留，consumer 必须读取或移除，不能直接遗弃。其他 producer 的 `Foreground` 零值行为与 `job_output` 的 wait 收集语义不变；这些修复不改变工具 schema、session v2、composition ID 或持久化格式，也不新增部署参数。

通知是 role 为 user、source kind 为 `tool-jobs` 的消息，由 `Agent.Notify` 投递：

- agent 忙时进入内存队列，在下一个边界作为 `user/message` 追加到当前 turn：turn 开始后、工具 step 结束后（steer 之后），以及模型给出无工具调用的回答之后。最后一种情况下 turn 不结束，而是再开一个 step 回应通知。最后一个允许的 step（无论是否有工具调用）和因输出上限被截断、以 `max_tokens` 结束的 step 都不取通知，通知留在队列中，由 turn 结束后的唤醒回应。
- agent 空闲时，或一个 turn 结束后队列中仍有通知且没有排队的 turn，worker 以最早的通知开启新 turn，其余通知在该 turn 开始时追加。这对应上游默认的 `wakeup` 投递；本仓不设 `maxConsecutiveWakes`，与 Base 默认相同。
- 每个取出队列输入的边界先检查取消：被取消的 turn 不取出通知或 steer，以 `canceled` 结束；取出后的提交使用不继承取消的 context，取消与提交竞态时输入已提交而不是丢失，下一个边界再观察到取消。中断前排队的通知等待下一个 turn，不因 interrupt 单独自动开 turn；中断生效后接受的新通知保留唤醒请求，按 [ADR-0013 的消息规则](0013-background-continuable-subagents.md#3-消息与通知) 在旧 turn 退出后开启下一 turn，一并处理旧通知；留下的 steer 在下一个 turn 的第一个工具 step 边界投递。目标轮次的暂停和子代理的 “stopped” 结算都依赖这里记录的 `canceled`。`WhenIdle` 把已唤醒但尚未开始的通知 turn 视为忙。
- 完成通知在入队前先提交为 `notice/queued` 事实，投递是带同一 `notice_id` 的 `user/message`；agent 停止时仍欠着的通知在会话恢复后的下一个 turn 投递一次。此条取代原先“待投递通知只在内存中、agent 停止时丢弃”的决定，记录格式、恢复与去重规则见 [ADR-0023](0023-durable-job-notices.md)。

> 后续约束：[ADR-0013](0013-background-continuable-subagents.md#6-job-的-owner-释放) 规定 one-shot agent 只在唯一 turn 运行期间接受通知，turn 结束后不再由通知唤醒；本节的通用唤醒规则受此限制。

> 后续停止契约：[ADR-0018](0018-goal-stop-outcomes.md) 规定输出截断的 turn 不消费待投递通知，通知留给下一个 turn；本节的提交点受此停止规则约束。

模型可见的通知只通过已提交的 `user/message` 进入 surface。投递记录沿用 `user/message`（[ADR-0023](0023-durable-job-notices.md) 另增入队记录 `notice/queued` 和来源字段 `notice_id`）：source kind 本来就是开放字符串，order validator 已允许活动 turn 内任意位置的 `user/message`。新出现的因果形态（无工具调用的 step 之后出现 `user/message` 并继续 step）也由现有 validator 接受，resume 修复规则不变。TUI 把这类消息显示为 `job> `，而不是 `you> `。

> 后续格式：[ADR-0016](0016-long-running-goals.md#领域与记录) 为目标轮次增加 `MessageSource` 归属字段和严格折叠校验；`tool-jobs` 通知仍沿用本节形态。

### 恢复与身份

job 与计数器不持久化；尚未投递的完成通知自 [ADR-0023](0023-durable-job-notices.md) 起持久化并在恢复后投递。新进程的编号从 1 重新开始；旧 transcript 中的 job ID 只在编号尚未复用时返回 `unknown job`。同 owner 新建同名 ID 后，旧文本可指向新 job，ID 不构成跨进程身份。composition ID 改为绑定 `shell-tools-v2` 和新增的 `job-tools-v1`（ADR-0023 升为 `job-tools-v2`），旧会话按 composition mismatch 拒绝恢复；本仓尚无发布 tag，没有已发布的用户会话需要迁移。

> 已被取代：本节的 `shell-tools-v2` 由 [ADR-0008](0008-tool-output-spill-and-observation-policy.md#身份) 提升为 `shell-tools-v3`；此处保留后台任务落地时的身份。

### Subagent

delegated agent 可以调用对其可见的 `job_*` 工具，但只能访问自己的 job；它的审批策略固定为 `never`，所以无法通过 `bash` 启动 job。WP7 的 subagent 后台运行复用 `Launch`，用 `Outcome.Result` 交出报告，并负责上文的 owner 释放。

## 后果

模型在本仓与上游看到相同的 `bash` 与 `job_*` 定义，长命令不再因超时被杀，完成后无需轮询即可得知。job 运行时与种类无关，WP7 可以直接接入 subagent。

代价与风险：

- 通知可以在没有用户输入时开启 turn 并消耗模型调用；每个通知最多触发一个 turn，自激链需要模型反复启动新 job，而每次启动都需要用户审批。
- job 不随进程恢复；正常 shutdown 取消并等待受管执行，尚未投递的完成通知留在日志中，恢复后投递（ADR-0023）。异常退出和脱离进程组的后代能否回收取决于[平台与执行模式](../security.md#approvalshell-与进程)，不能假定所有 host 进程都已终止。
- 前台命令现在多一次 job 注册和输出复制（输出环最多 128 KiB）；内存上限为每 owner 10 个活动 job。
- 前台预留避免未交出的 job ID 引发通知或额外 turn，交接按读取时状态描述命令；consumer 必须完成读取或移除。
- 每个回退执行增加一个取消句柄与等待贡献，cleanup 等待 runner 与输出收尾；不遵守取消契约的 runner 会阻塞关闭。
- 输出环保留量与上游不同；超出保留窗口的输出在 WP2 的 spill 落地前无法找回。

> 后续决定：[ADR-0008](0008-tool-output-spill-and-observation-policy.md#bash-完整输出) 已保存完整 shell 输出并补齐定位符；[ADR-0013](0013-background-continuable-subagents.md#2-生命周期) 增加无需用户审批即可启动的后台 subagent job，因此“每次启动都需要用户审批”只适用于 `bash`，不覆盖所有 job producer。

## 被否决方案

- 持久化 job 记录：job 进程随 harness 结束，没有可恢复的执行。若未来需要可恢复 job，应连同执行后端重新设计。原先一并否决的“持久化通知队列”已由 [ADR-0023](0023-durable-job-notices.md) 改为采纳：已完成的结果即使 job 不在了仍是有效事实。
- 新增 `job/notice` 记录类型：上游本身以 `user/message` 送达，现有 replay、compaction 和 provider 映射无需改动即可处理；新记录类型只增加格式面。
- 只在前台超时时才注册 job：输出缓冲需要在提升时迁移，命令在超时前不可见，也改变了 job 编号与上游的一致性。
- 输出环保留 256 KiB：读取加状态行可能超过 tool result 上限，被统一截断后丢掉状态行。
- 由 job 工具适配器投递通知（上游 `tool-jobs` 的做法）：适配器需要依赖 agent registry，且通知是否已被 wait 或 kill 收走只有注册表知道；由服务通过消费方 `Notifier` 投递只有一个 owner。
- 把 job 工具声明为并发：上游没有声明，`job_output` 的 wait 也会让同批次的其余调用与之重叠，改变调用顺序语义。
- 仅依赖 `Wait` 的 waiter 数：无法覆盖注册到等待或超时到读取的空档。
- 超时返回时立即释放前台预留：producer 可在读取前通知，前台随后又报告同一终态。
- 只依赖 agent 先关闭取消回退执行：不能证明 provider 自身的 Scope 清理顺序，也不能约束其他合法 consumer 的在途调用。

## 复审触发条件

- WP7 接入 subagent job，需要 owner 释放、progress 或值结果的新约束。
- WP2 的 spill 存储落地，丢失提示和截断提示需要报告完整输出路径。
- 增加 job 的 UI 列表、人工 kill 或观察读取。
- 增加多个同 owner 的前台 consumer，需要重新界定读取与交接权限。
- 引入独立容器、VM 或平台后代跟踪，能够提供更强回收保证。
- 观察到通知引起的连续自动 turn，需要上游的 `maxConsecutiveWakes` 或 `quiet` 投递。
- 参考指针更新改变了 jobs、tool-jobs 或 tool-bash 的定义或语义。

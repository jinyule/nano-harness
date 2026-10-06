# 后台任务运行时、job 工具与 bash 后台运行

- Status: implemented
- Date: 2026-10-04

## Context

这是[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP3。WP1 让 `bash` 使用上游 `enableRunInBackground: false` 的前台变体：`timeoutMs` 到期即终止进程组，没有 `run_in_background`，也没有 `job_*` 工具。参考提交 `5badb15009ae` 的 Base 组合加载 `dsh-jobs-local`、`dsh-tool-jobs`，`tool-bash` 使用默认配置，因此模型看到后台变体，前台命令超时后转为后台 job，job 完成后通过 `agent.inject()`/`followup()` 以 `user/message` 通知 owner。

调查了上游 `packages/jobs/{jobs,jobs-local,tool-jobs}`、`packages/shell/tool-bash`、`packages/core/agent-loop` 的 inbox 与 turn 循环，以及 Base 组合。上游 inbox 持久化为 `agent/inbox/spliced`；本仓 followup、steer 当前都是内存队列，模型可见事实只来自已提交事件。`tool-call-timeout-policy` 与本工作无关：`job_output` 自己管理 wait 期限，`bash` 的超时由工具自身处理。

非目标：spill 文件（WP2）、subagent 后台运行（WP7）、job 的 UI 列表与人工 kill、可恢复的 job。没有修改 search 包。

## Decision

job 名额测试的结算屏障证据见[边界修复 Note](2026-10-06-spill-question-and-call-validation.md)；本 Note 保留后台任务的产品决定与原始验证。

前台完成收集、超时原子交接、回退执行所有权与平台回收边界由[前台 job 交接 Note](2026-10-06-foreground-job-handoff-and-shell-cleanup.md)补充；本 Note 保留后台任务的其余设计与实施证据，当前契约以 [ADR-0009](../../../docs/decisions/0009-background-jobs.md) 为准。

长期契约记录在 [ADR-0009](../../../docs/decisions/0009-background-jobs.md)，当前事实分别归[架构](../../../docs/architecture.md#后台任务)、[安全](../../../docs/security.md#approvalshell-与进程)和[测试](../../../docs/testing.md#agent-与工具证据)文档。本次实施：

- 新增 `internal/app/job`（插件 `jobs`）。producer 用 `Launch(Spec)` 提交阻塞式 `Run(ctx, *Output) Outcome`，服务在自己的 `WaitGroup` goroutine 中运行它；`Kill` 与关闭取消 `ctx`。effect/cleanup 对：`Start` 登记 `stop`，后者拒绝新 job、把活动 job 标为 `stopping` 并取消、等待全部 producer 返回、丢弃记录。所有操作校验调用方 session；每 owner 最多 10 个活动 job；输出环运行中 128 KiB、首次终态读取后 16 KiB；writer 暂存被拆开的 UTF-8 尾部；producer panic 收敛为 `failed`。
- WP7 使用的 Go API（`internal/app/job`）：

  ```go
  func New(notifier Notifier) (*Service, error)
  func (*Service) Launch(spec Spec) (string, error)          // ErrInvalidConfig, ErrNotRunning, ErrLimit
  func (*Service) List(owner string) []View
  func (*Service) Get(owner, id string) (View, error)        // ErrUnknownJob, ErrForeignJob, ErrNotRunning
  func (*Service) Read(owner, id string) (Read, error)       // 消费式；首次终态读取带 Result
  func (*Service) Wait(ctx context.Context, owner, id string, timeout time.Duration) (View, error)
  func (*Service) Kill(owner, id, reason string) (View, bool, error)
  func (*Service) Remove(owner, id string) error             // ErrStillRunning
  type Spec struct { Kind, Label, Owner string; Foreground bool; Run func(context.Context, *Output) Outcome }
  type Outcome struct { Status Status; Detail, Result string }
  type Notifier interface { Notify(sessionID string, message session.Message) error }
  ```

  `Output.Writer(Stdout|Stderr)` 返回不会失败的 writer。subagent producer 可在 `Run` 中驱动 child、用 `Outcome.Result` 交出报告；需要 owner 关闭时释放 job 的生产者须增加 owner 释放（ADR 已记录）。
- 新增 `internal/adapter/tool/job`（插件 `job-tools`），注册与上游逐字节一致的 `job_output`、`job_list`、`job_kill`，三者 exclusive；上游 `tool:jobs` 段落以 `appTool.OrderJobs = 1600` 挂在 `job_output` 上。
- `bash` 切换到 Base 后台变体。每次调用在审批之后注册为 kind `bash` 的 job（进程无 runner 截止时间）：`run_in_background` 立即返回 ID，审批原因注明 background；前台从注册时预留完成收集并等待 `timeoutMs`，及时结束时移除记录并按原前台格式渲染；超时的消费式读取若仍活动才返回 `[still running after Nms; moved to background job <id>]`，已结束则按前台结果返回并移除；取消或关闭时 kill 并移除，返回 `tool call aborted`；达到上限时退回到期即终止。approval、sandbox、升级和 delegated 拒绝不变。
- `platform/process.Request` 追加 `Stdout`/`Stderr` 观察者，`Timeout` 为零表示只受 ctx 约束；改动保持追加式，以便与 WP1 的 `StdoutLimit` 合并。
- agent engine 的注入语义：`Agent.Notify(message)` 与 `Registry.Notify(sessionID, message)`。忙时通知进入内存队列，由 engine 在三个边界追加为 `user/message`：turn 开始后、工具 step 结束后（steer 之后）、无工具调用的回答之后；最后一种情况下 turn 继续一个 step，已到 step 上限时留在队列。空闲时 worker 以最早通知开启新 turn（`woken`），turn 结束后仍有通知且没有排队 turn 也会唤醒；被取消的 turn 留下的通知等待下一个 turn；`WhenIdle` 把已唤醒的通知 turn 视为忙；agent 停止时丢弃队列。
- 通知由 `app/job` 在 settle 时决定：前台完成收集预留尚在、有 wait 收走、由 `Kill` 引起、服务关闭或启动 context 已取消时不发；否则文本为 `background job <id> (<kind>: <label>) finished <status line>. Read its output with job_output.`，source kind `tool-jobs`。session v2 不变，没有新增记录类型，因此不需要持久化固定样本或迁移；TUI 把这类消息显示为 `job> `。
- composition 新增 `jobs` 与 `job-tools`，顺序为 `shell-tools → jobs → job-tools → subagent-tools`，使后台进程在 shell 临时目录删除前结束；composition ID 改为 `shell-tools-v2` 并加入 `job-tools-v1`。两份目录 fixture 更新，parity 测试的上游工具数加上三个 job 工具（与 WP4 的 `todo_write`、WP5 的 `web_search`/`web_fetch` 合并后为 12）。
- mutation 新增 `job-owner-fence`：去掉 owner 比较后 `TestService_FencesOwnersAndUnknownJobs` 必须失败。
- `scripts/tui-e2e.py` 增加一个在第一个 turn 结束后才完成的后台 job，验证终端 `job>` 通知和通知开启的 turn。

### 整体审查后的取消修复

整体审查发现：工具 step 结束后，engine 先取出 steer 和通知，再用已取消的 ctx 追加。JSONL `Append` 首先检查 `ctx.Err()`，所以 interrupt 落在工具执行期间时，追加失败，已取出的输入丢失，turn 记为 `error`，下游目标轮次被解除 armed 而不是暂停，子代理结算显示 failed 而不是 stopped。agent 包的内存日志原先忽略 ctx，因此测试没有暴露它。另一处不一致：有工具调用的最后一步会提交通知，随后 turn 以 `step_limit` 结束，通知没有得到回应。

修复后，三个取出队列输入的边界（turn 开始、工具 step 结束、无工具调用的回答之后）先检查取消，取消时以 `canceled` 返回、不取出任何输入；取出后的 `user/message` 提交和无工具调用路径的 `step/end`、完成 `turn/end` 使用不继承取消的 context，竞态时输入已提交，下一个边界观察取消。turn 打开前的 `Events` 与 admission 失败也按 `outcomeFor` 区分取消。最后一个允许的 step 无论有无工具调用都不取通知，turn 结束后的唤醒回应它；steer 仍在最后一个边界提交。与集成分支的 `max_tokens` 结局合并后，被截断的 step 同样不取通知，它的 `step/end` 也改为不继承取消地提交，取消与截断竞态时记为 `canceled`（`truncated step` 用例）。内存日志的 `Append` 与 `Events` 改为先拒绝已取消的 context；唯一依赖旧行为的用例（预先取消的 turn）现在在首次 `Events` 处得到 `canceled`。

目标服务的 `Settle` 和子代理的结算文案只读取 `turn/end` 的 outcome，取消映射已有各自的测试；修复保证 interrupt 落在工具 step 时记录的是 `canceled`。

## Consequences

模型看到的 `bash` 与 `job_*` 定义与上游 Base 一致；长命令不再因超时被杀；job 完成后无需轮询。job 运行时与种类无关，WP7 可以直接复用。

代价与风险：

- 通知可以在没有用户输入时开启 turn 并消耗模型调用；每个通知最多触发一个 turn，而启动新 job 需要用户审批。
- 正常关闭取消并等待受管执行，丢弃尚未投递的通知；脱离进程组的后代回收受[平台与模式边界](../../../docs/security.md#approvalshell-与进程)限制；恢复后旧 job ID 为 `unknown job`，编号从 1 重新开始。
- 前台命令多一次 job 注册与输出复制；每 owner 内存上限为 10 个活动 job 的输出环。
- 输出环保留量（128 KiB）小于上游（256 KiB），超出窗口的输出在 spill 落地前无法找回。
- 旧会话按 composition mismatch 拒绝恢复；本仓尚无发布 tag。
- 与其他 WP 的冲突热点：`engine.go`（新增 `notices` 字段与三个投递点）、`agent.go`、`application.go`、`main.go`、两份 fixture、`tui-e2e.py`、`mutation-cases.json`、`docs/testing.md` 的 mutation 数量。

复杂度观察（`make quality BASE_REF=11e1042`，阈值 10，仅观察）：新包 `app/job`、`adapter/tool/job` 中没有超过阈值的函数。改动函数中 `(*Engine).runTurn` 为 40（增加通知的三个投递点和失败分支）、`composeApplication` 21、`(*model).applyEvent` 26、`(*Runner).Run` 20、`(*Provider).bash` 12；前台等待、提升、退回和取消路径拆到 `foreground`、`finish`、`outcome`、`promoted`，各自低于阈值。新增代码没有跨包重复候选。

## Verification

- 以下结果在 rebase 到集成分支 `095ff95`（含 WP1 ripgrep、WP4 `todo_write` 与 WP5 web 工具）之后重新获得。`platform/process.Request` 同时保留 WP1 的 `StdoutLimit`、host 模式可省略临时目录和本次的输出观察者、零截止时间。
- `go test -race -count=1 ./...`：通过。job、agent、shell、job 工具包另以 `-count=3` 重复运行通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0，覆盖 fmt、mod、vet、race test、architecture、submodule、agent-notes、skills、workflow-tools、lint（0 issues）、逐文件 100.0% coverage、mutation（与 WP5 合并后十一个用例全部 killed，含 `job-owner-fence`）和 build。
- `make tui-e2e`：编译后的真实二进制、PTY 和 macOS `sandbox-exec` 通过：根会话 14 次工具调用（含 `todo_write`）全部成功，四次 approval 均为 `allowed-once`；后台 `bash-2` 在第一个 turn 结束后完成，终端显示 `job> background job bash-2`，通知开启的 turn 中 `job_output` 返回 `JOB_PROOF\n[status: completed, exit code: 0]`；transcript 中只有一条 `tool-jobs` 通知。
- `TestComposition_BackgroundJobsEndToEnd`：真实 composition、真实 host 进程和 loopback provider，用文件确定因果顺序。磁盘 transcript 证明后台启动、`job_list`、`job_kill` 的 `requested cancellation of job bash-1`、`one\n[status: killed, signal: SIGKILL; not needed]`、`job_output` wait 超时返回 `[status: running]`，以及唯一一条第 2 个 turn 的完成通知；provider 收到的第 5 个请求包含该通知。
- `TestBash_RealBackgroundAndPromotedProcesses`：真实 host 进程的双流后台输出；提升后的命令被 `job_kill` 后，其后台子进程在进程组终止后不再存在。
- 门禁反例：把 `job_output` 的 `wait` 描述改一个词，或把 `run_in_background` 移到 `workdir` 之前，`TestComposition_ToolCatalogGolden` 与 `TestComposition_MatchesUpstreamBaseTools` 均失败；恢复后通过。
- `upstream-base-tools.json` 的新条目由一次性脚本从 submodule 的 `docs/tool-catalog.md` 抽取原文，`bash` 在目录的后台变体后追加已审查的升级字段；未参考 Go 实现，随后与真实 composition 比对一致。
- 取消修复：`TestAgent_InterruptDuringToolKeepsNoticesAndSteers`（审查者的场景：工具阻塞到取消，期间 Notify、Steer、Interrupt，再 Submit）、`TestAgent_LastToolStepLeavesNoticesForNextTurn`、`TestEngine_LastToolStepTakesNoNotices` 和 `TestEngine_CancellationAtBoundariesKeepsQueuedInput`（turn 开始、工具边界、回答之后和提交竞态四个场景）。在保留新内存日志语义的前提下换回修复前的 `engine.go`，这些测试全部失败（`Outcome:error`、通知提交到 step_limit 的 turn、取出后丢失）；恢复修复后通过。
- 未验证：Linux `bwrap` 下的后台命令只经过单元与 host 模式测试；没有进行 live provider 调用；Windows 不提供 workspace sandbox。

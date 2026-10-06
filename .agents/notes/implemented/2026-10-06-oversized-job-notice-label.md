# 超长 job label 的完成通知按上游截断，投递失败写入 job 状态

- Status: implemented
- Date: 2026-10-06

## Context

第三轮 wiring 审查 B2 指出两个提交的冲突：`726a95f` 把工具参数预算提高到 768 KiB，`bash` 的 `command` 没有单独上限，而 bash job 的 label 就是命令原文；`internal/app/job.Notice` 把完整 label 拼进一个文本块，`Agent.QueueNotice` 用 `validUserMessage` 校验时单个文本块上限是 `session.MaxTextBytes`（256 KiB），超限返回 `ErrInvalidConfig`；`Service.settle` 用 `_ =` 丢弃了这个错误。命令在 256 KiB 到 768 KiB 之间时，后台或超时转后台的 job 完成后，模型永远收不到通知，空闲 agent 也不会被唤醒，违背 [ADR-0023](../../../docs/decisions/0023-durable-job-notices.md)。

job 的来源只有 `bash`（label 为命令）和后台子代理（label 为 description，K1 已限制在 128 KiB）。通知的 detail 来自退出码、信号或错误文本，理论上也没有固定上限。

上游 `packages/jobs/tool-jobs/src/index.ts` 的 `fitCompletionNotice` 不限制 label，而是按 producer 声明的 `outputLimitBytes` 截断整条通知：保留 `background job <id>`，截取 ` (<kind>: <label>) finished <status line>` 的开头，追加 `\n[notice truncated]\nDone; job_output.`。

非目标：`job_list` 一行渲染完整 label，超长 label 会让整个工具结果被 runtime 截断，与上游相同，本次不改。agent 包的 `QueueNotice` 也不改，避免与并行的唤醒修复冲突。

## Decision

权威描述在 [ADR-0009 完成通知](../../../docs/decisions/0009-background-jobs.md#完成通知)与 [ADR-0023 写入与投递](../../../docs/decisions/0023-durable-job-notices.md#写入与投递)。

- `job.Notice` 采用上游的截断形态，上限固定为 `session.MaxTextBytes`，在 UTF-8 字符边界截断。本仓不引入 producer 的 `outputLimitBytes`：唯一必须满足的上限就是 durable 文本块上限，截断放在 job 服务里，对所有 producer 同时生效。上游为极小上限准备的分支（连前缀都放不下）在固定的 256 KiB 下不可达，不实现。
- 不在准入时拒绝长命令：长命令本身合法，上游也接受；拒绝会让模型重写一个可以运行的命令。
- `QueueNotice` 失败不再被丢弃，而是以 `completion notice not delivered: <错误>` 并入 job 的 detail，`job_output` 与 `job_kill` 的状态行显示它。截断后通知总能通过校验，剩下的失败只来自 owner（不再 live、one-shot 已不在唯一 turn 内、日志写入失败）。前两种没有 reader，诊断无人可见也无害；日志写入失败时，仍能读取的 owner 可以从状态行发现问题。job 服务不区分错误种类，因此不依赖 agent 包的哨兵错误；诊断不进日志、不重试。错误文本来自 agent 和 session 存储，不含 token 或凭据。

## Consequences

超长后台命令的完成事实总能被提交并唤醒 owner；通知没有送达时，job 状态留有可见原因。代价是超长通知会丢失状态行的结尾，模型需要用 `job_output` 看完整状态，与上游一致。

没有额外的进程或用户可见日志通道：仓库目前没有诊断输出设施，owner 已不能读取时，失败仍然无人观察。若以后引入诊断或 telemetry 插件，这里应改为上报。Linux 上超过 128 KiB 的单个 argv 会让 `bash -c` 以 E2BIG 失败；失败的 job 同样走截断后的通知，这是平台限制，本次不处理。

## Verification

- 修复前：`go test -count=1 -run 'TestNotice_|TestService_Undelivered' ./internal/app/job/` 失败（409,707 字节等三个超长通知不被接受；失败未写入状态行）；`go test -count=1 -run TestComposition_OversizedBackgroundCommandNoticeIsDelivered ./cmd/nano-harness/` 失败于 “the completion notice was not delivered”。
- 修复后三项均通过。`TestNotice_FitsOneTextBlock` 覆盖恰好等于上限不截断、超出一个字节、两字节字符两种奇偶的切点，并用 `session.Record.Validate` 证明通知被接受。assembled 测试用真实 composition、host bash 和 JSONL，让 300 KiB 的后台命令在首个 turn 结束后完成，从磁盘确认 `notice-1` 入队并投递一次、文本带截断标记且不超过上限、provider 收到它。
- `scripts/mutation-cases.json` 新增 `job-notice-fits-text-block`、`job-notice-rune-boundary`、`job-notice-failure-visible`，`python3 scripts/mutation-check.py --manifest <仅这三项>` 三项均 killed。
- rebase 到 `8ea52b1` 后，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：逐文件 100% coverage，默认 mutation 清单全部 killed（含上述三项）。`make tui-e2e` 通过（真实二进制与 PTY，含后台 job 通知）。只在 macOS 上运行；Linux 上的 E2BIG 分支只由 assembled 测试的说明覆盖，未实测。

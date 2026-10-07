# 工具批次的取消与失败收尾配对每个未决问题和调用

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查发现 B2：模型一次回答多个工具调用时，engine 逐条以调用 context 提交 `tool/call`。第一条提交后、第二条提交前发生取消，第二条追加因 context 已取消失败，函数直接返回；收尾 defer 只追加 `step/end` 与 `turn/end`，而第一条调用没有结果，严格 order validator 以 `step/end has unfinished work`、`turn/end has unfinished work` 拒绝。live 日志停在开着的 turn 中，下一个 turn 的 `turn/start 2` 也被拒绝，直到重新打开会话、由 resume 修复补写中断结果。`main`（`9a75ea1`）的 `engine.go:222` 同样以调用 context 提交 `tool/call`，defer 也只关闭 step/turn，所以这是 main 既有缺陷，不是本轮对齐引入的。

同一缺口也出现在其他异常路径：结果记录中途失败或批次 panic 时，已提交调用同样没有结果，step 无法关闭。

首版修复（`90591e3`）只在内存中跟踪调用，复审发现 RB2：审批问题同样会留在 step 中。真实 approval 服务的 `approval/decided` 追加失败时，`Decide` 返回错误，工具结果为 `Error: approval could not be recorded`；该调用的结果被 validator 以 `tool/result precedes approval decision` 拒绝，收尾补写的 interrupted 结果同样被拒，随后是 `step/end has unfinished work` 和 `invalid turn/start 2`，只有重开会话才能恢复。复审还指出“失败后下一轮直接开启”的文档保证过宽：收尾补写本身也失败时，同一 live 日志仍需重开；会话写满 64 MiB 后，重开的修复也会报 `repair tool result: session size limit reached`。

参考提交 `5badb15009ae` 的 `packages/core/agent-loop/src/tool-calls.ts` 在取消后为每个未分发的调用补写 `tool/call` 与 `Error: tool call aborted before dispatch`（`AbortError/ABORTED_BEFORE_DISPATCH`）结果对，README 称之为合成的 call/result 对；`core/session/src/repair.ts` 的恢复把已有 `tool/call` 的未决调用记为 `ToolOutcomeUnknownError/TOOL_OUTCOME_UNKNOWN`。

非目标：tool runtime 的取消检查点、resume 修复规则、session 格式与 validator。

## Decision

`internal/app/agent/engine.go` 做两处改动，与[wake-turn 开场修复](2026-10-06-wake-turn-opening.md)的 A1 规则一致：已提交的开场事实不受取消影响，每个已提交的 `turn/start` 都有 `turn/end`。

1. 一次回答的全部 `tool/call` 以 `context.WithoutCancel` 作为一个整体提交。随后的 `ExecuteBatch` 在调度检查点观察到取消，为每个调用返回 runtime 已有的 `ABORTED_BEFORE_DISPATCH` 结果；结果与 `step/end` 不受取消影响地提交，之后的取消检查使 turn 以 `canceled` 结束。日志形态与上游跳过调用的 call/result 对相同，分类沿用 [ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 第 2 节的 runtime 取消检查点，不新增码。
2. step 因记录失败或 panic 异常结束时，defer 从日志读出仍未决的审批问题和调用，按 resume 修复的顺序补写：先为每个问题追加 `cancelled` 决定（形态与 resume 相同，不带来源），再为每个调用追加 `session.InterruptedToolResult`，最后追加 `step/end` 和 `turn/end`。这些调用可能已经执行，分类与 resume 修复相同，为 `TOOL_OUTCOME_UNKNOWN`；resume 修复改用同一个 `core/session` 构造函数，文本与分类只有一个定义。日志只在 step 配对完问题和调用后接受 `step/end`，所以日志中所有未决项都属于这个开着的 step，不需要按 turn/step 过滤。读取失败与原错误一起返回。

   首版在内存中跟踪调用，看不到 approval 服务生成的问题 ID；改为读取日志后，问题和调用由同一来源找出，不再需要 engine 侧的跟踪状态。与 approval 服务的决定串行化（`287e199`）不冲突：工具批次同步返回后才运行收尾，此时没有进行中的 `Decide`；`Decide` 的决定追加失败后直接返回、不重试，engine 也只为日志中仍未决的问题补写，日志的 validator 会拒绝同一问题的第二个决定，所以不会产生重复决定。

文档同步：[架构](../../../docs/architecture.md)的 engine 取消语义（含收尾以日志可追加为前提）、approval 段的配对保证（决定追加失败时由 engine 收尾补写，同样只在日志仍可追加时成立）、工具运行时与 `tool/result` 分类说明，ADR-0019 映射表的 `TOOL_OUTCOME_UNKNOWN` 行，以及[测试策略](../../../docs/testing.md)的 engine 证据。按协调者要求不新增 ADR。

composition 身份不变。session 格式、order validator 与 resume 规则不变；新写下的都是现有格式已接受、runtime 或 resume 已经产生的记录，旧日志的含义与恢复结果不变。没有新组件、goroutine 或配置，Scope 与生命周期不变。

## Consequences

取消落在批次记录中途时，live 日志保持合法：被取消的 turn 以 `canceled` 闭合，模型在下一个 turn 看到每个调用都有 `tool call aborted before dispatch` 结果，不需要重开会话。记录失败或 panic 时，turn 仍以 `error` 结束，但未决问题得到 `cancelled` 决定、已提交调用得到中断结果，step 与 turn 正常关闭。

这些保证以日志仍可追加为前提，resume 修复也一样，不能保证总能修好。收尾补写本身也失败时，补写失败与原错误一起返回，同一 live 日志不再接受新 turn，须在恢复可写后重新打开会话由 resume 修复。会话写满单 session 上限时（复审在 `call-1` 提交后只留 40 字节复现），补写结果、`step/end` 与 `turn/end` 都返回 `session size limit reached`，重新打开时修复同样失败，`Open` 报 `repair tool result: session size limit reached`：该会话无法再通过产品打开，日志仍保留在磁盘上，只能新开会话。还有约 150 KiB 余量时收尾有效，下一个 turn 正常完成。容量耗尽是 main 既有行为，本修复没有使它恶化。

后续项（未实施，交维护者决定）：为闭合记录预留容量。可以仿照 [ADR-0015](../../../docs/decisions/0015-multimodal-tool-results.md) 的 `imageReserveBytes`，让 jsonl 对开启新工作的记录保留最后一段固定字节，只允许闭合类记录使用：`approval/decided`、interrupted `tool/result`、`compaction/end`、`step/end`、`turn/end`。或者在剩余容量低于阈值时拒绝新 turn。这会改变哪些追加返回 `ErrSessionSize`，属于持久化语义变化，需要 ADR 确定预留大小（与一个 step 可能的未决问题和调用数量相关）和已有满会话的处理，所以不在本次提交内。审批决定追加失败时，turn 报告的是随后被拒绝的结果追加（`tool/result precedes approval decision`），原始存储错误只体现在工具结果的 `approval could not be recorded` 中。

代价是取消后仍会写满一次回答的全部 `tool/call` 与对应 abort 结果，最多比修复前多写一个批次的记录；调用参数已经过 `LimitArguments` 截断，记录有界。异常结束的 step 在收尾时多读一次日志。

## Verification

- 修复前：`go test -count=1 -run 'CancelWhileRecording|FailedToolRecording' ./internal/app/agent/` 失败。取消用例得到 `Outcome:error`，错误为 `context canceled` 加 `step/end has unfinished work`、`turn/end has unfinished work`；两个记录失败子用例同样带 unfinished work，且已提交调用没有结果。输出保存在被忽略的 `.cache/b2/red.log`。
- `TestEngine_CancelWhileRecordingToolCallsClosesTheStep` 使用真实 JSONL Manager 与日志，在 `call-1` 的 `tool/call` 提交后的回调中取消。它要求 outcome 为 `canceled`、工具未执行、两个调用都是 `AbortError/ABORTED_BEFORE_DISPATCH`，以及 turn 结局依次为 `canceled`、`completed`。被取消 batch 的 seq 6–11 必须逐字节等于人工审查的 `internal/app/agent/testdata/cancelled-batch-tail.jsonl`。下一个 turn 在同一 live 日志上开启，turn 编号为 2；关闭后重新打开，事件数不变，没有修复记录。
- `TestEngine_FailedToolRecordingResolvesCommittedCalls` 覆盖两种失败。第一种让 `call-2` 的 `tool/call` 追加失败，此时只有 `call-1` 得到 interrupted 结果。第二种让 `call-2` 的结果追加失败，此时 `call-1` 保留 `ok`，`call-2` 得到 interrupted 结果。两种情况的结果都是 `TOOL_OUTCOME_UNKNOWN`，turn 只报告注入的失败，即恢复记录本身没有被拒绝，下一个 turn 正常开启。`TestInterruptedToolResult_IsAValidDetachedUnknownOutcome` 校验构造的记录，并证明它不共享分类值。
- RB2 修复前：`go test -count=1 -run 'FailedDecisionRecording|CloseoutReports' ./internal/app/agent/` 失败。审批用例得到 `tool/result precedes approval decision`（两次）、`step/end has unfinished work` 和 `turn/end has unfinished work`；读取失败用例中 turn 只报告 `disk full`。输出保存在被忽略的 `.cache/rb2/red.log`。
- `TestEngine_FailedDecisionRecordingCancelsTheQuestion` 使用真实 approval 服务（broker 一律批准）、tool runtime 与 JSONL，只让第一次 `approval/decided` 追加失败。它要求：
  - turn 只报告被拒绝的结果追加，即收尾本身没有错误；工具只执行一次（第二个调用）；
  - 两个问题各有且只有一个决定：第一个是 `cancelled`、不带来源，第二个保留 `allowed_once`/`operator`；
  - 两个调用都得到 `TOOL_OUTCOME_UNKNOWN` 的 interrupted 结果，turn 结局依次为 `error`、`completed`，下一个 turn 编号为 2；
  - 关闭后重新打开，事件数不变，没有修复记录。
- `TestEngine_CloseoutReportsAnUnreadableLog` 在 `call-2` 追加失败后让日志读取也失败，要求 turn 同时报告两个错误。
- 定向 mutation 用单项清单运行 `scripts/mutation-check.py`，均被拒绝（killed）：
  - `agent-batch-calls-cancellable`：`tool/call` 恢复为以调用 context 提交。
  - `agent-step-skips-call-recovery`：跳过调用补写循环（RB2 后变异点改为新的循环）。
  - `agent-result-keeps-call-unresolved`：结果不再消去调用，导致重复结果（RB2 后变异点移到日志扫描）。
  - `agent-closeout-skips-open-questions`：跳过问题补写循环。
  - `agent-decision-keeps-question-open`：决定不再消去问题，导致重复决定。
  - `agent-closeout-drops-read-failure`：丢弃日志读取错误。
- 中断结果的分类改由 `core/session` 构造后，原 `tool-repair-unknown-outcome` 的变异点在 `jsonl.go` 中已不存在，首次 `make check` 将它报为 `stale-site`。该项改为在 repair 调用点换成 `TOOL_NOT_STARTED` 结果，仍由 `TestLog_RepairClassifiesAnUnknownOutcome` 拒绝。新增的 `session-interrupted-result-unknown-outcome` 在构造函数中替换分类，由 `TestInterruptedToolResult_IsAValidDetachedUnknownOutcome` 拒绝。两项单独运行都是 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：
  - 全仓 race tests；
  - 逐产品文件 100% coverage；
  - 架构检查与 Agent Note 检查；
  - lint 0 issues；
  - 首版时 193 项定向 mutation 全部 killed；
  - 真实 cmd build/smoke。
- `make tui-e2e` 通过：真实二进制与 PTY 下 19 个 root 工具调用，包括中断与恢复。PTY 脚本没有构造批次记录中途的取消；这个交错只由上面的真实 JSONL 测试固定。没有 live provider 或其他操作系统的证据。
- RB2 修复后 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 再次通过：全仓 race tests、逐产品文件 100% coverage、架构与 Agent Note 检查、lint 0 issues、209 项定向 mutation 全部 killed、真实 cmd build/smoke；`make tui-e2e` 同样通过。PTY 脚本不注入存储故障，审批决定追加失败只由真实 approval 服务与 JSONL 上的注入测试固定；没有物理故障磁盘的证据。

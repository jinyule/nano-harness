# 工具批次的取消与失败收尾配对每个已提交调用

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查发现 B2：模型一次回答多个工具调用时，engine 逐条以调用 context 提交 `tool/call`。第一条提交后、第二条提交前发生取消，第二条追加因 context 已取消失败，函数直接返回；收尾 defer 只追加 `step/end` 与 `turn/end`，而第一条调用没有结果，严格 order validator 以 `step/end has unfinished work`、`turn/end has unfinished work` 拒绝。live 日志停在开着的 turn 中，下一个 turn 的 `turn/start 2` 也被拒绝，直到重新打开会话、由 resume 修复补写中断结果。`main`（`9a75ea1`）的 `engine.go:222` 同样以调用 context 提交 `tool/call`，defer 也只关闭 step/turn，所以这是 main 既有缺陷，不是本轮对齐引入的。

同一缺口也出现在其他异常路径：结果记录中途失败或批次 panic 时，已提交调用同样没有结果，step 无法关闭。

参考提交 `5badb15009ae` 的 `packages/core/agent-loop/src/tool-calls.ts` 在取消后为每个未分发的调用补写 `tool/call` 与 `Error: tool call aborted before dispatch`（`AbortError/ABORTED_BEFORE_DISPATCH`）结果对，README 称之为合成的 call/result 对；`core/session/src/repair.ts` 的恢复把已有 `tool/call` 的未决调用记为 `ToolOutcomeUnknownError/TOOL_OUTCOME_UNKNOWN`。

非目标：tool runtime 的取消检查点、resume 修复规则、session 格式与 validator。

## Decision

`internal/app/agent/engine.go` 做两处改动，与[wake-turn 开场修复](2026-10-06-wake-turn-opening.md)的 A1 规则一致：已提交的开场事实不受取消影响，每个已提交的 `turn/start` 都有 `turn/end`。

1. 一次回答的全部 `tool/call` 以 `context.WithoutCancel` 作为一个整体提交。随后的 `ExecuteBatch` 在调度检查点观察到取消，为每个调用返回 runtime 已有的 `ABORTED_BEFORE_DISPATCH` 结果；结果与 `step/end` 不受取消影响地提交，之后的取消检查使 turn 以 `canceled` 结束。日志形态与上游跳过调用的 call/result 对相同，分类沿用 [ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 第 2 节的 runtime 取消检查点，不新增码。
2. engine 记录当前 step 中已提交、尚无结果的调用（结果按调用顺序提交，每次提交消去最早一项）。step 因记录失败或 panic 异常结束时，defer 先为它们补写 `session.InterruptedToolResult`，再追加 `step/end` 和 `turn/end`。这些调用可能已经执行，分类与 resume 修复相同，为 `TOOL_OUTCOME_UNKNOWN`；resume 修复改用同一个 `core/session` 构造函数，文本与分类只有一个定义。

文档同步：[架构](../../../docs/architecture.md)的 engine 取消语义、工具运行时与 `tool/result` 分类说明，ADR-0019 映射表的 `TOOL_OUTCOME_UNKNOWN` 行，以及[测试策略](../../../docs/testing.md)的 engine 证据。按协调者要求不新增 ADR。

composition 身份不变。session 格式、order validator 与 resume 规则不变；新写下的都是现有格式已接受、runtime 或 resume 已经产生的记录，旧日志的含义与恢复结果不变。没有新组件、goroutine 或配置，Scope 与生命周期不变。

## Consequences

取消落在批次记录中途时，live 日志保持合法：被取消的 turn 以 `canceled` 闭合，模型在下一个 turn 看到每个调用都有 `tool call aborted before dispatch` 结果，不需要重开会话。记录失败或 panic 时，turn 仍以 `error` 结束，但已提交调用得到中断结果，step 与 turn 正常关闭；如果日志本身已无法追加，补写失败与原错误一起返回，未闭合的尾部仍由 resume 修复。

代价是取消后仍会写满一次回答的全部 `tool/call` 与对应 abort 结果，最多比修复前多写一个批次的记录；调用参数已经过 `LimitArguments` 截断，记录有界。

## Verification

- 修复前：`go test -count=1 -run 'CancelWhileRecording|FailedToolRecording' ./internal/app/agent/` 失败。取消用例得到 `Outcome:error`，错误为 `context canceled` 加 `step/end has unfinished work`、`turn/end has unfinished work`；两个记录失败子用例同样带 unfinished work，且已提交调用没有结果。输出保存在被忽略的 `.cache/b2/red.log`。
- `TestEngine_CancelWhileRecordingToolCallsClosesTheStep` 使用真实 JSONL Manager 与日志，在 `call-1` 的 `tool/call` 提交后的回调中取消。它要求 outcome 为 `canceled`、工具未执行、两个调用都是 `AbortError/ABORTED_BEFORE_DISPATCH`，以及 turn 结局依次为 `canceled`、`completed`。被取消 batch 的 seq 6–11 必须逐字节等于人工审查的 `internal/app/agent/testdata/cancelled-batch-tail.jsonl`。下一个 turn 在同一 live 日志上开启，turn 编号为 2；关闭后重新打开，事件数不变，没有修复记录。
- `TestEngine_FailedToolRecordingResolvesCommittedCalls` 覆盖两种失败。第一种让 `call-2` 的 `tool/call` 追加失败，此时只有 `call-1` 得到 interrupted 结果。第二种让 `call-2` 的结果追加失败，此时 `call-1` 保留 `ok`，`call-2` 得到 interrupted 结果。两种情况的结果都是 `TOOL_OUTCOME_UNKNOWN`，turn 只报告注入的失败，即恢复记录本身没有被拒绝，下一个 turn 正常开启。`TestInterruptedToolResult_IsAValidDetachedUnknownOutcome` 校验构造的记录，并证明它不共享分类值。
- 定向 mutation 用单项清单运行 `scripts/mutation-check.py`，三项都被拒绝（killed）：
  - `agent-batch-calls-cancellable`：`tool/call` 恢复为以调用 context 提交。
  - `agent-step-skips-call-recovery`：跳过补写循环。
  - `agent-result-keeps-call-unresolved`：结果提交后不消去调用，导致重复结果。
- 中断结果的分类改由 `core/session` 构造后，原 `tool-repair-unknown-outcome` 的变异点在 `jsonl.go` 中已不存在，首次 `make check` 将它报为 `stale-site`。该项改为在 repair 调用点换成 `TOOL_NOT_STARTED` 结果，仍由 `TestLog_RepairClassifiesAnUnknownOutcome` 拒绝。新增的 `session-interrupted-result-unknown-outcome` 在构造函数中替换分类，由 `TestInterruptedToolResult_IsAValidDetachedUnknownOutcome` 拒绝。两项单独运行都是 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：
  - 全仓 race tests；
  - 逐产品文件 100% coverage；
  - 架构检查与 Agent Note 检查；
  - lint 0 issues；
  - 193 项定向 mutation 全部 killed；
  - 真实 cmd build/smoke。
- `make tui-e2e` 通过：真实二进制与 PTY 下 19 个 root 工具调用，包括中断与恢复。PTY 脚本没有构造批次记录中途的取消；这个交错只由上面的真实 JSONL 测试固定。没有 live provider 或其他操作系统的证据。

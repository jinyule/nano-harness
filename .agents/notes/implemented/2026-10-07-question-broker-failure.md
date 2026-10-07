# 提问 broker 故障不再归为 NO_PROVIDER

- Status: implemented
- Date: 2026-10-07

## Context

最终联合评审（`_coord/reports/final-latest.md` 第 4 项）发现，[question 批](2026-10-07-structured-goal-question-results.md)把 `ErrUnavailable` 改为带 `NO_PROVIDER` 的值后，`Ask` 原有的折叠路径会把已注册 broker 返回的任何普通错误也变成 `UserQuestionError/NO_PROVIDER`，混淆“没有回答者”与“回答者故障”。调查基于集成分支 `357b51e`。

上游对照（`packages/interaction/user-questions/src/index.ts`，参考提交 `5badb15009ae`）：`NO_PROVIDER` 只由 waterfall 没有处理者时的 `noAnswerer` 产生（约 318 行）；catch 中只重抛已是 `UserQuestionError` 的错误，信号已中止时改为 `ASK_ABORTED`，其余普通错误原样传播（约 331–337 行）。阻塞的 `ask()` 不校验 broker 返回的答案批；`BAD_ANSWER` 只出现在待答问题的 `answer()` 回复入口（约 160–184 行），本仓没有该入口。`plan-mode` 的 `exit_plan_mode` 只把 `ASK_CANCELLED` 改写为关闭文本，其余提问错误原样抛出，与本仓一致。

逐项复核 question 与 exit_plan_mode 的其余错误路径：调用前已取消（`ASK_ABORTED`）、空请求（`EMPTY_QUESTIONS`）、委派调用方（`DELEGATED_CALLER`）、intent 违规（`BAD_INTENT`）、未注册 broker（`NO_PROVIDER`）、等待中取消（`ASK_ABORTED`）与用户关闭（`ASK_CANCELLED`）都与上游在同一条件下产生的码一致；题数、id、选项与长度检查、服务停止、plan 的关闭、继续规划、反馈、未激活与标题错误都没有分类。除评审项外，`ErrInvalidAnswer` 的 `BAD_ANSWER` 也属于同类过度分类，一并修正。

## Decision

- 已注册 broker 返回 `ErrCancelled` 以外的错误（包括普通错误和其他已分类错误）时，返回未导出的 `errBrokerFailed`：文本与 `ErrUnavailable` 相同，不实现分类，不展开原因，避免把 broker 内部的分类带进结果。
- `ErrInvalidAnswer` 改回普通错误，文本不变。
- `ErrUnavailable` 只表示没有注册 broker，仍带 `NO_PROVIDER`。
- 模型可见文本、`errors.Is(err, ErrCancelled)` 与 exit_plan_mode 的行为不变；question 与 plan 的 composition token 不再提升，因为这些 token 在 H+I 批之后尚未发布任何会话数据，且持久化格式未变，只是少写了两类分类。

ADR-0019 的映射行、架构与测试文档同步更新。

## Consequences

transcript 中的 `NO_PROVIDER` 现在可以可靠地表示“没有回答面”；回答面故障与答案批不符不带分类，这一点与上游一致。但上游会原样重抛 broker 返回的 `UserQuestionError`，并保留普通错误的原 message；本仓在 context 有效时把 `ErrCancelled` 以外的 broker 错误一律改为固定文案、不分类。生产环境唯一的 broker（TUI）只返回 `ErrCancelled`、`ErrNotRunning` 或 context 错误，目前对运行时没有影响；该差异记录在 [ADR-0014](../../../docs/decisions/0014-user-questions-and-plan-mode.md) 与参考分析的偏差表中。代价是本仓不再保留 broker 故障的原因；之前同样没有保留。仍有一处有意保留的差异：broker 返回已分类错误且调用随后被取消时，本仓先报告 `ASK_ABORTED`，上游会先重抛 broker 的分类；改变它会改变模型可见文本，不在本次范围。

## Verification

- 修复前 `go test -count=1 ./internal/app/question/ ./internal/adapter/tool/question/ ./internal/adapter/tool/plan/`：`TestService_BrokerFailuresStayUnclassified` 的四个子用例分别被分类为 `NO_PROVIDER` 或 `BAD_ANSWER`，`TestAskUserQuestion_FailuresBecomeErrorResults/broker_failure` 与 `TestExitPlanMode_KeepPlanningDismissalAndFailures` 的 unavailable、invalid answer 子用例失败；修复后全部通过，文本断言未改。
- 新增 mutation `question-broker-failure-unclassified`（恢复返回 `ErrUnavailable`）与 `question-invalid-answer-unclassified`（恢复 `BAD_ANSWER`），均 killed。
- 基于 `357b51e`，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出码 0：lint 0 issues，每个产品源文件 100% coverage，清单 173 个 mutation 全部 killed；`make tui-e2e` 通过（19 次 root 工具调用，含提问回答与 plan review）。

# goal 与提问失败的结构化分类

- Status: implemented
- Date: 2026-10-07

## Context

这是 [WP12 实施计划](../proposed/2026-10-06-structured-tool-results-plan.md)的 H（goal）与 I（question）批，基于集成分支 `fc0022f`，[基础批](2026-10-07-structured-tool-results-base.md)已提供 `tool.Failure` 与 tool/result 的 `error` 字段。goal 的领域错误和工具错误已有 Code，提问接缝只有普通哨兵与 `RequestError`，分类在 runtime 渲染文本时丢失。映射由 [ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 拥有。

上游对照（参考提交 `5badb15009ae`）：`GoalError` 子类携带 `GOAL_*`；tool-goal 直接 `new HarnessError(message, 'GOAL_TOOL_*')`，name 为 `HarnessError`；`interaction/user-questions` 的 `UserQuestionError` 码为 `ASK_ABORTED`、`DELEGATED_CALLER`、`EMPTY_QUESTIONS`、`BAD_INTENT`、`NO_PROVIDER`，`ui-user-questions` 的取消为 `ASK_CANCELLED`，答案校验为 `BAD_ANSWER`；题数、id、选项等检查是本仓额外限制，上游没有对应码。

## Decision

- `app/goal.Error` 增加 `ToolError()`，返回 `GoalError` 与原 Code；goal 工具的 `toolError` 用 `ToolError()` 替换只供测试的 `Code()`，返回 `HarnessError` 与原码。
- `app/question` 新增 `Error{Code, Message}`，五个可分类哨兵改为 `*Error` 指针值，`errors.Is` 按身份成立；`ErrNotRunning`、`ErrInvalidBroker` 不对应模型可见的上游码，保持普通错误。空请求与三种 intent 违规返回带 `EMPTY_QUESTIONS`/`BAD_INTENT` 的 `*Error`，`Unwrap` 到原 `RequestError`，其余 `RequestError` 不分类。本仓额外拒绝的未知 intent kind 同属 intent 违规，沿用 `BAD_INTENT`，不新造码。
- `exit_plan_mode` 代码不变：它原样传播提问错误，因此带同样分类；关闭（`ErrCancelled` 被改写为上游的 dismiss 文本）、继续规划、反馈与未激活错误没有分类。
- composition：`goal-tools-v3`、`question-tools-v2`、`plan-tools-v2`。模型可见文本、schema 与插件生命周期不变，新类型都是纯值。

## Consequences

goal 与提问失败在 transcript 中可按上游分类检索，UI 卡片工作可以直接使用。三个 token 提升使这些组合下的旧本地会话按 composition 不匹配被拒绝；当前无发布数据。哨兵从 `errors.New` 改为指针值后，比较仍按身份，调用方无需改动。

## Verification

- 修复前（暂存三处产品改动后）`go test -count=1 ./internal/app/question/ ./internal/app/goal/ ./internal/adapter/tool/goal/ ./internal/adapter/tool/question/ ./internal/adapter/tool/plan/`：`TestError_ClassifiesTheUpstreamFailures`、`TestError_ClassifiesAsGoalError`、`TestRender_ToolErrorsKeepTheirCodes`、`TestTools_PersistUpstreamClassifications`、`TestAskUserQuestion_FailuresBecomeErrorResults`、`TestExitPlanMode_KeepPlanningDismissalAndFailures` 全部以缺失分类失败；恢复后通过。
- `go test -race -count=1 ./cmd/nano-harness/`：goal 与提问 assembled 测试从磁盘读出分类，provider 请求不含分类；旧 goal/question/plan composition 会话被拒绝且文件不变。
- 定向 mutation `goal-error-classification`、`goal-tool-error-classification`、`question-cancel-classification`、`question-intent-classification` 全部 killed。
- rebase 到 `287e199` 后，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出码 0：lint 0 issues，每个产品源文件 100% coverage，清单 161 个 mutation 全部 killed，真实 cmd 构建与 version smoke 通过。
- `make tui-e2e` 通过：真实 binary/PTY、19 次 root 工具调用，含提问回答、plan review 与 `/goal` 轮次完成。

# 工具调用取消只替换成功结果

- Status: implemented
- Date: 2026-10-07

## Context

[subagent 正确性记录](2026-10-06-subagent-correctness.md)给 tool runtime 加了取消截止线：轮到调度时和即将进入 Execute 时发现取消，返回 `Error: tool call aborted before dispatch`；Execute 返回后再检查一次。第二次检查放在错误分支之前，所以 Execute 返回错误时也会被替换成 `Error: tool call aborted`。

参考提交 `5badb15009ae` 的 `packages/core/tools/src/index.ts` 有两处用取消结果替换原结果，前提都是 `!isError`：一处在 dispatch 后，一处在 post-execute 后。body 抛出的错误经 `toolErrorResult` 保留原文本和分类。本仓的做法会抹掉领域错误，例如被取消的规划审查本应返回提问服务自己的 `ask_user_question was aborted before the user answered`。[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 的错误分类以对齐后的行为为准，所以这项修复作为 WP12 之前的独立变更完成。

非目标：两个调度前截止线、工具自身的取消处理、结构化错误字段。

## Decision

`internal/app/tool/runtime.go` 先处理 Execute 返回的错误，再检查取消：错误一律渲染为 `Error: <message>`；只有 Execute 成功返回、而调用已被取消时，才替换为 `Error: tool call aborted`，并丢弃成功文本和图片。规则记录在 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md) 和[架构](../../../docs/architecture.md)的工具运行时一节。

composition 身份不变。改动只影响被取消调用此后写下的结果文本，不改变已持久化记录的含义；K1 引入原行为时同样没有提升 `tool-runtime`。没有新组件、goroutine 或配置，Scope 与生命周期不变。

## Consequences

模型在取消后看到的失败原因与上游一致：返回 `context canceled` 或领域取消文案的工具保留自己的文本，bash 和 job_output 本来返回的 `tool call aborted` 不变。ADR-0019 可以据此持久化 `FS_ABORTED`、`SEARCH_ABORTED`、`ASK_ABORTED` 等领域分类，不会被 runtime 改写。

代价是被取消调用的结果文本不再统一：原样返回 Go 错误的工具会显示 `Error: ... context canceled` 一类文本。这与上游 body 抛出原始错误时的形态相同。成功结果的替换保持不变，迟到的取消仍会把已提交副作用（如已发布的文件）的成功结果显示为 `tool call aborted`，这同样是上游语义。

## Verification

- 修复前：`go test -count=1 -run 'TestRuntime_CancellationKeepsTheBodyFailure' ./internal/app/tool/` 失败，结果是 `Output:"Error: tool call aborted"`，测试期望 body 返回的 `Error: write aborted: context canceled`。输出保存在被忽略的 `.cache/wp12-runtime/red.log`。
- 修复后，`go test -race -count=1 ./internal/app/... ./internal/adapter/... ./cmd/...` 中只有 `TestComposition_CancelledPlanReviewCannotScheduleExit` 断言旧文本；它经真实组装取消审查，现在断言提问服务的 `ask_user_question was aborted before the user answered`，并仍检查没有提交退出规划。`TestRuntime_CancellationStopsDispatchAndSupersedesSuccess` 的四个阶段与 10 槽并发上限测试不变并通过。
- 定向 mutation `tool-cancel-keeps-body-failure` 让错误分支在取消时让位给 abort 文本，用单项清单运行 `scripts/mutation-check.py`，被 `TestRuntime_CancellationKeepsTheBodyFailure` 拒绝（killed）。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race、逐产品文件 100% coverage（含 `runtime.go`）、架构、Agent Note、lint、全部定向 mutation（含本项）和真实 cmd build/smoke。
- `make tui-e2e` 通过：真实二进制与 PTY 下 19 个 root 工具调用，包括规划审查、中断与恢复。没有 live provider 或其他操作系统的证据。

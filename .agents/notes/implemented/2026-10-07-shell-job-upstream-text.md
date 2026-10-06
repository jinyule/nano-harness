# shell 与 job 的模型可见文案按上游字节对齐

- Status: implemented
- Date: 2026-10-07

## Context

第四轮 tools 审查（S1、S2、N1、N2、N3）发现，shell 与 job 有几处模型可见的文本或行为偏离上游 `5badb15009ae`，参考分析和 ADR-0009 却记为已采纳，或者没有记录：

- S1：前台 sandbox 不可用是 `Error: SANDBOX_UNAVAILABLE: workspace sandbox is unavailable: <行>`。上游 `packages/sandbox/sandbox/src/index.ts:132-145` 的 `SandboxUnavailableError` 是一段处置说明，后接 ` Runner failure: <行>`。后台 runner 失败的 detail 缺少上游 `packages/shell/tool-bash/src/background.ts:50-54` 的 `exit code: N; ` 前缀。
- S2：`exec.Cmd.WaitDelay` 固定 1 s，命令正常退出后同样生效。上游 `packages/subprocess/subprocess-local/src/spawn.ts:409-415` 以 `graceMs` 作排空窗口，bash 为 3 s（`packages/shell/bash-local/src/index.ts:35,167`）。`(sleep 2; echo late) & echo early` 在本仓丢失 `late`。
- N1：显式空 kill reason 被当作没有 reason。上游 `packages/jobs/jobs-local/src/index.ts:584-587` 仍拼成 `${detail}; ${reason}`。
- N2：`job_output` 把值结果放在丢失提示之前。上游 `packages/jobs/tool-jobs/src/index.ts:183-188` 是 delta（以丢失提示结尾）、值结果、状态。
- N3：stderr 任一行含 `sandbox-exec: ` 或 `bwrap: ` 的普通失败命令会被判为 runner 失败。上游 `diagnostics.ts:65-87` 判定相同，不是偏差。

范围限定在 `internal/platform/process`、`internal/adapter/tool/shell`、`internal/adapter/tool/job` 与 `internal/app/job`；不改 agent 包，不改错误码的结构化通道（WP12）。

## Decision

权威描述在 [ADR-0009](../../../docs/decisions/0009-background-jobs.md) 的 job 工具、bash 与固定预算三节，逐项对照见[参考分析](../../../docs/reference-deepseek-harness.md#shell-与-job-边界复核)。

- `process.ErrSandboxUnavailable` 的文本改为上游 workspace-write 模式的原文，runner 失败追加 ` Runner failure: <匹配行>`。匹配行只去掉行尾 CR，与上游 `/\r?\n/` 分行一致，不再 `TrimSpace`。runner 启动失败追加 Go 的启动错误；Node 的 `String(error)` 无法复现，这是唯一的文字差异。文本放在 platform：它对应上游 sandbox provider 这一层，shell 适配器直接渲染 `Error: <message>`。`SANDBOX_UNAVAILABLE` 不再出现在模型可见文本中，与上游一致，调用方用 `errors.Is` 识别。
- 后台 runner 失败的 detail 按上游 `processOutcome`：runner 退出时为 `exit code: N; [sandbox: …]`，runner 未能启动时为上游同样的 `killed before exit; [sandbox: …]`。状态仍是本仓的 `failed`，这个偏差沿用原有记录。
- runner 的管道排空窗口等于请求的 `TerminationGrace`，bash/job 因此为 3 s，正常退出和 KILL 之后都适用。零宽限的只读搜索保留 1 s，因为 Go 的零 `WaitDelay` 会无限等待管道，而搜索立即 KILL 的决定归 ADR-0007。前台取消的结算等待由 5 s 改为宽限、排空再加 1 s 的 7 s，避免脱离进程组的后代占住管道时提前放弃。
- `job.Service` 的 kill reason 改为 `*string`，显式空值也并入 detail。producer 没有 detail 时，空 reason 就是整个 detail；`View` 用未导出的标记把它渲染成上游的 `[status: killed, ]`。
- `renderOutput` 改为上游顺序：双流输出、丢失提示、值结果、状态。只有丢失提示时不再显示 `(no new output)`。预算上，状态与丢失提示总能保留；双流输出先截断，值结果用剩余预算，双流已占满时省略值结果。
- N3 不改代码，在 ADR-0009 与 security.md 记为已知限制。

## Consequences

模型在这些路径上看到的文字与上游一致：sandbox 不可用时得到完整处置说明，后台 detail 带退出码，后台子进程在 3 s 内的输出不再丢失，空 reason 与读取顺序可按上游预期解析。

代价与风险：

- 有后代持有管道时，bash 命令最多多等 2 s 才返回；终止阶段的上界从 4 s 变为 6 s。
- 前台错误文本变长，旧文本中的 `SANDBOX_UNAVAILABLE` 字样消失；依赖该字样的外部脚本需要改用结构化信息（目前没有）。
- `[status: killed, ]` 和 `signal: SIGTERM; ` 的尾随分隔符看起来像缺字，这是有意的上游字节。
- 目前没有 producer 同时产生流输出和值结果，N2 的预算分配只在单元测试中触发。
- N3 的误判仍然存在。收紧匹配要偏离上游，需要单独决策。

## Verification

- 修复前失败：
  - `go test -count=1 ./internal/platform/process/`：`TestRunnerRun_RunnerFailureOutranksDenial`（旧文本 `SANDBOX_UNAVAILABLE: …`）、`TestRunnerRun_FailedSandboxSpawnPreservesCause`（旧 `start sandbox runner:`）、`TestRunnerRun_DrainsDescendantOutputForTheGrace`（3 s 宽限只得到 `early\n`，1.008 s 返回）。
  - `go test -count=1 ./internal/adapter/tool/shell/`：`TestBash_RunnerFailureHasInfrastructureExplanation`（前台旧文本、后台缺 `exit code: 1; `）、`TestOutcome_RunnerSpawnFailureHasNoExitCode`。
  - `go test -count=1 ./internal/app/job/ ./internal/adapter/tool/job/`：`TestService_ExplicitEmptyKillReasonReplacesPriorIntent` 与 `TestJobKill_ExplicitEmptyReasonReplacesPriorIntent` 得到 `[status: killed, signal: SIGTERM]`；`TestJobOutput_LossNoticePrecedesValueResult` 得到 `out\nvalue\n[some output …]`。
- 修复后上述测试通过；process、shell、search、job 工具与 app/job 的覆盖率均为 100%。零宽限请求在 1.9 s 内只返回 `early\n`，证明搜索的立即 KILL 与 1 s 排空不受影响。
- `scripts/mutation-cases.json` 新增 7 项：`shell-runner-failure-exit-code`、`process-unavailable-upstream-text`、`process-drain-follows-grace`、`job-empty-kill-reason-joins`、`job-empty-detail-rendered`、`job-output-loss-before-value`、`job-output-loss-reserved`。单独运行时全部 killed。
- 既有用例同步：`job-output-status-envelope` 改指新的预算语句，`job-explicit-empty-reason` 改指改名后的测试。为保持 `process-term-grace` 的变异位置唯一，排空赋值没有再写一个 `TerminationGrace > 0` 判断。`ErrSandboxUnavailable` 末尾的句号是上游原文，用带 ST1005 理由的局部 `//nolint:staticcheck` 保留。
- rebase 到 `d45dc6f` 后，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，lint 0 issues，逐文件 100% coverage，默认 mutation 清单全部 killed；`make tui-e2e` 通过。
- 只在 macOS 上运行。Linux 的 `bwrap` 路径由 fake runner 的 goos 参数覆盖，未在真实 Linux 上验证排空窗口。

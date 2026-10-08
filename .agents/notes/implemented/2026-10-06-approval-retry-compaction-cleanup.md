# approval、retry、compaction 的 cleanup 取消并等待进行中的调用

- Status: implemented
- Date: 2026-10-06

## Context

[plan cleanup Note](2026-10-06-plan-cleanup-joins-calls.md) 排查同类插件时发现，`approval.Service`（`SetPolicy`、`Decide`）、`retry.Service.Do` 和 `compaction.Service.Maybe` 都是先在锁内检查 `active`，再在锁外写调用方的日志，cleanup 只翻转 `active`。因此 `Scope.Close` 返回后，进行中的调用仍可能追加记录：

- `SetPolicy` 追加 `approval/policy`，还会把策略写进 cleanup 刚清空的表。
- `Decide` 在 broker 回答后补写 `approval/decided`。
- `Do` 在退避结束后追加 `llm/retry-started` 并发起下一次尝试。
- `Maybe` 在 cleanup 之后才收尾已开始的 compaction 事务。

产品 composition 的正常关闭不会触发这些问题，判断依据如下：

- `Decide`、`Do` 和 engine 内的 `Maybe` 只在 agent turn 内运行，turn 在 registry cleanup 中被取消并等待。
- `SetPolicy` 和手动 `/compact` 由 TUI 命令触发，前端最先关闭，并等待已开始的命令。
- 三个插件都在 registry 之前启动，因此在 registry 关闭全部 agent 和日志之后才关闭。

缺陷在于插件自身契约：它们依赖调用方另行 join，违反 AGENTS.md 中“关闭必须达到静止”的要求。非目标：不改变正常运行时的 approval 规则、retry 策略和 compaction 选择。

## Decision

三个插件沿用 `web.Service` 与 `plan.Service` 的在途登记模式：`begin` 在服务锁内检查运行状态、派生可取消的调用 context、登记 cancel 并 `group.Add(1)`；cleanup 置 `active = false`、取消全部登记的调用，释放锁后 `group.Wait()`。停止后不再有 `Add`，所以 `Wait` 不会与 `Add` 竞争。各插件关闭时的取消语义分别如下，当前描述在[架构](../../../docs/architecture.md)与 [ADR-0020](../../../docs/decisions/0020-tool-result-pruning.md#摘要发布与失败)：

- approval：`SetPolicy` 与 `Decide` 整体登记，cleanup 以内部停止原因取消。问题尚未提交时直接返回错误；已提交的问题在不可取消、5 秒有界的提交中补齐 `approval/decided`。最终结局与停止状态切换的串行化由[决定提交修补 Note](2026-10-06-approval-decision-commit.md)补充；停止先发生时，落盘与返回都为 `cancelled`（来源 `cancellation`），不能授权工具执行。broker 和策略表在等待结束后才清空，迟到的 `SetPolicy` 不会把策略留在已停止的服务里。`Broker` 文档要求 `Ask` 在 context 结束后及时返回，TUI broker 满足。
- retry：只登记“重试决定”这一段，即读取策略、追加 `llm/retry`、退避等待、追加 `llm/retry-started`。模型尝试由调用方的闭包使用调用方的 context 发起，属于调用方，cleanup 不能取消也不等待；尝试在 cleanup 之后失败时，`Do` 返回 `ErrNotRunning`。退避中被取消的重试只留下 `llm/retry`，与 turn 取消时相同。
- compaction：`Maybe` 整体登记，摘要模型请求由服务自己发起，所以同样被取消。事务开始后的失败沿用不可取消的错误收尾；本变更把摘要提交后的成功 `compaction/end` 也改为不继承取消，这样摘要落盘之后被取消的请求仍关闭事务并返回成功，cleanup 返回前不会留下打开的事务。这一改动同样适用于调用方取消，此前该窗口要靠 resume repair 关闭。

当时没有引入跨包的共享 tracker；后来五份相同结构由 `plugin.Calls` 统一，取舍见[在途调用登记统一](2026-10-07-plugin-calls.md)。

## Consequences

五个调用日志的服务（plan、web、approval、retry、compaction）现在都在 cleanup 返回前达到静止，不依赖调用方的 join；正常运行路径不变。代价是每次调用多一次服务锁、一个派生 context 和一次 map 登记，approval 与 compaction 的锁从 RWMutex 改为 Mutex。

风险：broker、provider 或日志若忽略取消且长时间阻塞，cleanup 会随之阻塞，这与 `web` 的取舍相同。compaction 的成功收尾不再被取消打断，被取消的请求最多多等一次日志追加。

与 [工具结果裁剪 Note](2026-10-06-tool-result-pruning.md) 和[截断摘要 Note](2026-10-06-compaction-truncated-summary.md) 部分重叠：它们拥有裁剪与失败收尾的决定，本 Note 拥有 cleanup 静止和成功收尾不可取消的证据。[plan cleanup Note](2026-10-06-plan-cleanup-joins-calls.md) 拥有 plan 的修复证据。均不归档。

## Verification

- 永久屏障测试（channel 构造交错，不依赖 sleep）：
  - `internal/app/approval/service_test.go`：`TestService_CleanupCancelsInFlightAppends/{policy,question}`、`TestService_CleanupWaitsForInFlightPolicyChange`、`TestService_CleanupSettlesPendingDecisions`（broker 在关闭时回答 `allowed_once`，cleanup 返回时日志已是 asked + decided `cancelled`/`cancellation`）。
  - `internal/app/retry/service_test.go`：`TestService_CleanupCancelsRetryWait`、`TestService_CleanupWaitsForRetryRecords`、`TestService_CleanupLeavesModelAttemptsToTheCaller`。
  - `internal/app/compaction/service_test.go`：`TestService_CleanupClosesInFlightCompaction`、`TestService_CleanupWaitsForCompactionCommit`（日志像 JSONL 一样拒绝已取消 context 的追加）。
- 修复前证据：在私有源码副本中把三个 `service.go` 恢复为 `8ea52b1` 的版本，运行 `go test -race -count=1 -run Cleanup ./internal/app/approval/ ./internal/app/compaction/`：
  - approval 报 `a call committed after cleanup returned: records [approval/policy], policies 1`、`records [approval/asked approval/decided]`、`cleanup did not cancel the in-flight policy change`（10 s 守卫）和 `cleanup returned before the decision was paired: [approval/asked]`；
  - compaction 报 `cleanup returned with the compaction open: [compaction/start]` 和 `cleanup did not cancel the in-flight compaction`。
  - retry 的旧测试引用了新的 `begin`，副本无法编译，所以使用实现前的同一组测试结果：`a retry continued after cleanup returned: records [llm/retry llm/retry-started], attempts 2` 和 `cleanup did not cancel the in-flight retry record`。
- 修复后：三个包的 race 测试通过，`service.go` 均为 100.0% coverage；`go test -race -count=100 -run Cleanup` 覆盖四个包，通过；`./internal/app/... ./internal/adapter/tui ./internal/adapter/session/... ./cmd/nano-harness` 的 race 测试通过。
- 新增 mutation：`approval-cleanup-cancels-calls`、`approval-shutdown-cancels-decision`、`retry-cleanup-cancels-waits`、`compaction-cleanup-cancels-calls`、`compaction-summary-closes-transaction`。它们和这些文件上的既有用例一起由 `scripts/mutation-check.py` 运行，全部 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`（Go 1.27.0、darwin/arm64，rebase 到 `d79501d` 后）退出码 0：race 测试、架构、submodule、Agent Note、skills、lint（0 issues）、逐产品文件 100.0% coverage、101 个 mutation 全部 killed 与真实 binary smoke 均通过。改动只影响关闭和取消时的日志收尾，不改变模型请求内容，未运行 `make tui-e2e`。

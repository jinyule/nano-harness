# plan-mode cleanup 取消并等待进行中的调用

- Status: implemented
- Date: 2026-10-06

## Context

第二轮审查指出：`plan.Service` 的锁拆分为“服务锁只管生命周期和会话表，会话锁串行化该会话的日志读取与追加”之后，Scope cleanup 只翻转 `running` 并丢弃会话表，不等待已经取得会话锁的调用。阻塞在 `journal.Events` 中的 `Select` 或 `Step` 可以在 `Scope.Close` 返回后继续执行，追加 `plan/mode` 或切换提示。审查者用仓库外 overlay 屏障测试复现了“关闭后追加一次”，违反 AGENTS.md 中“关闭必须达到静止”的插件契约。

产品 composition 下的正常关闭不会触发这个问题，判断依据如下：

- `cmd/nano-harness` 中 `plan-mode` 先于 `agents`（registry）启动，逆序关闭时 registry 先关闭全部 agent，`TestComposition_StartOrderEncodesShutdownQuiescence` 固定了 agent 层最后启动的顺序。
- 用户入口 `Registry.SetPlanMode` 在 agent worker 状态锁内调用 `Select`。agent cleanup 的 `Interrupt` 和 worker 退出路径都要取得这把锁，再等待 worker 结束，因此进行中的 `Select` 会先于 registry cleanup 结束；之后 `agent.active` 为假，新的选择返回 `ErrNotRunning`。
- `Step` 只由 engine 在 worker 的 turn 内调用，worker 退出前 turn 已经结束。
- agent cleanup 最后关闭 JSONL 日志，之后 `Append` 返回 `ErrNotRunning`，到 plan cleanup 运行时已不存在可写日志。

因此这是 `plan-mode` 插件自身的契约缺陷：它依赖调用方另行 join，换一个不持 agent 锁的调用方（例如未来的协议桥）或手工组装时就会暴露。非目标：不改变选择、边界和退出的语义，也不改变锁顺序（agent worker 状态锁 → 会话锁）。

## Decision

修补 [ADR-0014](../../../docs/decisions/0014-user-questions-and-plan-mode.md) 的规划模式读写契约，[架构](../../../docs/architecture.md)同步一句。实现沿用 `web.Service` 的 in-flight 登记模式：

- `begin(ctx, id, create)` 在服务锁内检查运行状态、查找或创建会话状态，派生可取消的调用 context，登记 cancel 并 `group.Add(1)`。返回的 `done` 取消 context、注销登记并 `group.Done()`。`Select`、`Step`、`Exit` 都经过它；`Exit` 保持原有错误次序：已停止返回 `ErrNotRunning`，从未出现的会话返回 `ErrInactive`。
- cleanup 在服务锁内置 `running = false`、丢弃会话表并取消全部登记的调用，释放锁后 `group.Wait()`，然后才返回。停止后不会再有 `Add`，所以 `Wait` 不会与 `Add` 竞争。
- 进行中的调用通过 context 感知关闭：排队等待会话锁或阻塞在日志读取中的调用收到取消。已越过日志最后一次取消检查的读取或追加照常完成，但一定在 cleanup 返回之前。等待时长取决于日志对取消的响应；与 `web` 相同，cleanup 不在 shutdown context 到期时提前返回，因为提前返回会重新打开同一个窗口。
- `Active` 只读内存，直接查会话表，cleanup 丢弃会话表后返回假，不参与登记。

同类插件排查（“服务级或会话级状态锁 + 日志读写”及“检查运行状态后再写日志”）：

- `goal.Service`：单把服务锁覆盖读取、校验和追加，cleanup 也要取得这把锁，因此会等待进行中的变更和轮次准入，之后的调用在锁内看到 `running = false`。无此缺陷，未修改。
- `todo` 工具：无进程内状态，追加属于 turn 内的工具调用，由 agent 关闭 join。`skill` 工具：只注册工具和上下文 provider，不写日志。`question.Service`：不写日志。三者都无此缺陷。
- `approval.Service`（`SetPolicy`、`Decide`）、`retry.Service.Do`、`compaction.Service.Maybe` 属于另一种模式：先检查 `active` 再在锁外写调用方的日志，cleanup 不等待。单元层面同样可能在 cleanup 返回后追加；产品关闭中这些日志已由更早的 registry cleanup 关闭，所以不可触发。修复需要逐个决定关闭时的取消语义，例如 `Decide` 正在等待 broker、配对的 `approval/decided` 使用不可取消 context，超出本 WP 范围，未修改。`compaction` 另由 B3 分支修改。

## Consequences

`plan-mode` 现在自身满足关闭静止，不再依赖调用方的 join；只有 cleanup 窗口内的进行中调用可能以 `context.Canceled` 失败，正常运行路径不变。代价是每次调用多一次服务锁、一个派生 context 和一次 map 登记；会话之间仍互不等待。

风险：日志实现若忽略取消且长时间阻塞，cleanup 会随之阻塞，与 `web` 的已知取舍相同。approval、retry、compaction 的同类窗口留给后续工作包统一处理；新增不持 agent 锁的调用方前应先补齐它们的等待。

与[提问与规划初始实现 Note](2026-10-04-ask-user-question-and-plan-mode.md)和[取消修补 Note](2026-10-06-spill-question-and-call-validation.md)部分重叠：前者拥有插件组装与格式决定，后者拥有 `Exit` 取消证据，本 Note 拥有 cleanup 静止的修复证据。三者都保留，不归档。与[关闭顺序 Note](2026-10-06-shutdown-quiesces-agents-first.md)互补：那里的顺序保证解释了产品关闭为何不可触发本缺陷。

## Verification

- 永久屏障测试（`internal/app/plan/plan_test.go`）：
  - `TestService_CleanupCancelsInFlightCalls/{select,step}`：调用在会话锁内阻塞于日志读取时同步执行 `scope.Close`，之后才释放读取。断言没有追加任何记录，调用返回 `context.Canceled`。
  - `TestService_CleanupWaitsForInFlightCalls`：日志读取在观察到取消后仍阻塞，释放后成功返回。断言：cleanup 取消了读取；cleanup 期间新调用返回 `ErrNotRunning`；读取释放前 cleanup 未返回；释放后的追加发生在 cleanup 返回之前。
- 修复前（只加测试）`go test -race -count=1 ./internal/app/plan/` 退出码 1：`select` 与 `step` 两个子测试都报 `an in-flight call committed after cleanup returned: [plan/mode=on]`，等待测试在 10 s 守卫后报 `cleanup did not cancel the in-flight journal read`。
- 修复后：`go test -race -count=1 -coverprofile=... ./internal/app/plan/` 通过，`plan.go` 100.0%；`go test -race -count=200 -run 'Cleanup|Lifecycle|Sessions|CancelledExit' ./internal/app/plan/` 通过；`./internal/app/agent ./internal/adapter/tool/plan ./internal/adapter/tui ./cmd/nano-harness` 的 race 测试通过。
- 新增 mutation `plan-cleanup-cancels-calls`（删除 cleanup 中的取消循环）被 `TestService_CleanupWaitsForInFlightCalls` 杀死；既有 `plan-mode-boundary`、`plan-exit-cancellation` 仍被杀死。删除 `group.Wait()` 的变异在本机 100/100 次被同一测试拒绝，但它依赖 cleanup goroutine 先于测试的非阻塞检查返回，不是确定性证据，因此没有加入 mutation 清单。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`（Go 1.27.0、darwin/arm64）：第一次在 `go test -race -count=1 ./...` 阶段失败于 `internal/app/subagent` 的 `TestService_SendMessageRoundTripAndColdResume`（`resumed notices` 只有一条），该测试不经过本次改动的路径，单独 `-race -count=40` 全部通过，属于负载相关的既有偶发失败，已报告给协调者。第二次完整运行退出码 0：race 测试、架构、submodule、Agent Note、skills、lint（0 issues）、逐产品文件 100.0% coverage、全部 mutation killed 与真实 binary smoke 均通过。
- 改动不影响模型可见行为或 TUI，未运行 `make tui-e2e`。

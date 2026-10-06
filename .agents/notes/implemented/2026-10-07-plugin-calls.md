# 在途调用登记统一为 plugin.Calls

- Status: implemented
- Date: 2026-10-07

## Context

web、plan、approval、retry 与 compaction 五个 app 服务各有一份约 20 行的在途调用登记：用计数器为每个调用登记 `CancelFunc` 并 `group.Add`，释放时在服务锁内删除并 `Done`；cleanup 在服务锁内清除运行状态、取消全部登记，释放锁后 `group.Wait`。它们来自 web 的原有实现、plan cleanup（`005a3a8`）与 cleanup 静止（`f0b343e`）；[当时的记录](2026-10-06-approval-retry-compaction-cleanup.md)以“需要扩展 `core/plugin` API”为由暂缓抽象。调查基于 `f39e9fe`，按 nano-find-simplifications 的流程核对了五份代码的差异：

- 取消原因：只有 approval 使用 `WithCancelCause` 并以 `errStopped` 取消；其余用 `WithCancel`。`cancel(nil)` 与 `WithCancel` 对 `Err()` 和 `Cause()` 的观察完全相同。
- 错误顺序与状态：每个服务先在自己的锁内检查运行状态并返回自己的 `ErrNotRunning`；plan 随后才查找或创建会话状态并可能返回 `ErrInactive`，然后登记。approval cleanup 在 Wait 之后清空 broker 与策略，plan cleanup 在同一临界区重置会话表。这些都在登记之前或 Wait 之后，与登记本身无关。
- 登记、取消、释放、等待四步在五处逐行相同，同一个服务锁同时守护运行状态与登记表。

结论是同一个不变量；差异只在登记前后的服务逻辑和取消原因，后者可以作为参数。web 中另一个 `WaitGroup` 是单次检索内的并发查询扇出，不属于在途调用登记，未纳入。

## Decision

新增 `plugin.Calls`（`internal/core/plugin/calls.go`，只依赖标准库）：`NewCalls(owner sync.Locker)`、`Admit(ctx)` 返回调用 context 与释放函数、`Cancel(cause)`、`Wait()`。它不持有自己的运行标志或锁：准入仍由服务在自己的锁内决定，`Admit` 与 `Cancel` 要求调用方持有该锁，释放函数自己获取它，避免与服务的 `active`/`running` 形成镜像状态。放在 `core/plugin` 是因为它描述的是插件 cleanup 的静止契约，与 `Scope` 同属生命周期原语；五个服务都是 app 层，可以依赖 core。

五个服务删除 `nextCall`/`calls`/`group` 三个字段（retry 为 `waits`，web 为 `nextID`/`operations`），`begin` 中的登记段与 cleanup 中的取消循环改为一次调用；approval 以 `Cancel(errStopped)` 保留原取消原因，其余传 nil。各服务的错误顺序、plan 的会话创建、approval 的 broker 清理与注释说明的等待界限保持原样。

web 测试原先读取服务内部登记表断言释放后为空；该断言移到 `plugin.Calls` 自己的测试，web 测试保留屏障与结果断言。四个“cleanup 取消在途调用”的 mutation 改为删除新的 `Cancel` 调用（web 的同类 mutation 与有界测试见[终审收尾记录](2026-10-07-final-review-polish.md)）；另增 `plugin-calls-release-unregisters`，删除释放时的反登记。

## Consequences

净删除五份重复的登记、释放和取消代码，新增约 55 行带文档的类型和测试；以后再有服务需要静止的在途调用时直接复用。`core/plugin` 的公开面多一个类型，它没有 goroutine、注册或 Scope 贡献，不是运行时组件。行为不变：取消原因、错误顺序、等待界限和屏障测试都与之前一致。

## Verification

- `go test -race -count=1 ./internal/app/web/ ./internal/app/plan/ ./internal/app/approval/ ./internal/app/retry/ ./internal/app/compaction/`：现有屏障与 cleanup 测试全部通过，未修改它们的断言（除 web 的内部登记表读取）。
- `go test -race -count=3 ./internal/core/plugin/`：`plugin.Calls` 的取消原因与 nil 原因、父 context 取消、释放后反登记与 Wait 解除、32 个并发调用均通过，`calls.go` 覆盖率 100%。
- 定向 mutation `plan-cleanup-cancels-calls`、`approval-cleanup-cancels-calls`、`retry-cleanup-cancels-waits`、`compaction-cleanup-cancels-calls` 与新增的 `plugin-calls-release-unregisters` 全部 killed。
- 基于集成分支 `f39e9fe`，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出码 0：架构检查通过，lint 0 issues，每个产品源文件 100% coverage，清单 165 个 mutation 全部 killed。

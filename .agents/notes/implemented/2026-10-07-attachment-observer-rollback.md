# 附件 observer 注册回滚与关闭静止

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查 S1（`_coord/reports/finalastra-latest.md`）：`internal/adapter/attachment` 的 `ObserveUnavailable` 先把 observer 放进 map 再调用 `Scope.Defer`；Scope 已关闭时 `Defer` 返回 `ErrScopeClosed`，observer 却留在 map 中，之后的 `ReadImage` 仍会调用它，违反启动失败回滚契约。原测试只检查错误返回且使用空回调。审查还要求核对回调快照与关闭并发时的静止：读取方在锁外调用快照中的 observer，observer 的 cleanup 可能在回调执行期间返回。

同类排查：`internal/adapter/model/provider` 的 `Start` 先 `settings.Watch` 并发布目录，再直接 `return scope.Defer(...)`；`Defer` 失败时 watch 与目录都会残留。其余 `Register*`/`Watch`/`UseSpill` 已在 `Defer` 失败时回滚；goal watcher 在回调期间持有读锁，撤销天然等待回调结束。

## Decision

- `ObserveUnavailable` 的 `Defer` 失败时调用 `withdraw` 撤销注册并返回联合错误。
- 每个注册是一个 `observer` 记录：读取方在 store 锁内为捕获的每个 observer 计数，回调返回后递减；`withdraw` 先从 map 删除（之后不会再被捕获），再等待计数归零或 cleanup context 结束。没有新增 goroutine，等待点就在 cleanup 中。observer 仍须非阻塞（TUI 的回调是非阻塞发送）。
- provider `Start` 的 `Defer` 失败时执行同一个 stop：dispose watch 并清空目录。
- 复审追加（`finalrecheck` Suggestion 2）：settings watch 的 dispose 原先只删除注册，不等 commit 已捕获的回调，cleanup 返回、目录清空后，在途回调仍能重新发布目录。现在 `settings.Service.Watch` 返回 `dispose(ctx) error`：先撤销注册（之后的 commit 不再调用），再按与附件 observer 相同的计数等待在途回调结束或 ctx 结束；不得在回调内部调用。provider 的 stop 先等待 dispose，再在 `installMu` 下标记停止并清空目录，`install` 在同一锁下检查停止标记，即使有回调晚于 cleanup 也不会重新发布。settings.Watch 在产品中只有 provider 一个订阅方；文件后端测试的回调改为非阻塞发送，避免 dispose 等待测试自身。
- ADR-0017 与测试文档补充 observer 的撤销与等待语义。

## Consequences

失败启动不再留下 observer 或 settings watch；observer cleanup 返回后不会再有回调。代价是 cleanup 可能等待一个正在运行的回调；回调本身受“不阻塞”约束，超出 cleanup context 时返回错误而不是无限等待。

## Verification

- 修复前的永久测试失败：`TestStore_FailedObserverRegistrationLeavesNoCallback`（`failed registration still received 1 callbacks`）、`TestStore_ObserverCleanupWaitsForRunningCallbacks`（`observer cleanup returned while its callback ran`）、`TestProviderStartLoginRefreshAndStreamFailures`（`failed start left the settings watch (disposed 0)`）。修复后通过，且测试在断言失败时释放被阻塞的回调，不会拖住 store cleanup。
- mutation 新增 `attachment-observer-rollback`、`attachment-observer-quiescence`、`provider-watch-rollback`，均 killed。
- 复审追加：修复前 `TestServiceWatch_DisposeWaitsForRunningCallbacks`（屏障让回调停在执行中，报 `dispose returned while the watcher's callback ran`）与 `TestProviderStop_IgnoresCallbacksThatOutliveCleanup`（cleanup 后调用捕获的回调，报 `a late callback republished the catalog`）失败，修复后通过；`TestServiceWatch_DisposeReportsAnExpiredWait` 覆盖等待超时。mutation 新增 `settings-watch-quiescence` 与 `provider-stopped-install`。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`（基于 `74f420b`）：exit 0，lint 0 issues，逐产品文件 100.0% coverage，192 个 mutation 全部 killed。

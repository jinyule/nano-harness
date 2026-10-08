# Subagent 结算通知先于释放唤醒

- Status: implemented
- Date: 2026-10-06

## Context

集成分支 `d7d199d` 上的 `TestService_SendMessageRoundTripAndColdResume` 约 80 次失败 1 次：冷恢复后的 child 结算后，测试等到它的 `done` 再等 root 空闲，偶尔读到的 root 日志只有第一条结算通知。

根因在 `Service.release`：同一 `Service.mu` 临界区内先 `close(current.done)`，再 `registry.Notify` 投递结算通知。测试 helper `h.done` 直接等待 `done`，随后调用 `Agent.WhenIdle`；若它在 `Notify` 入队前拿到 root 的 `agent.mu`，空闲 root 尚无通知与唤醒，`WhenIdle` 立即返回。

这个顺序自 `00803e7` 引入后未变；`1a3568a`（K1）只增加清理失败改写通知与 `delivered` 计数，`27a6e8f`（WP16）只让 job 通知经 `QueueNotice` 持久化，subagent 结算通知仍走内存 `Notify`，两者都没有改变顺序。产品内 `done` 的等待者是并发 `close` 与等待 closing target 的 `deliver`，二者被唤醒后都先重新获取 `Service.mu`，因此只能在整个临界区（含通知）结束后行动，当前没有产品可观察错误。上游 `packages/subagent/subagent/src/continuation-activation.ts` 的 `finishDisposal` 先 `notifySettlement`，再 `releaseOwnership` 与 `observer.settle`，结算观察者被唤醒时 parent 已收到通知；本仓顺序与之不一致，`done` 的语义弱于上游。

## Decision

- `release` 在临界区内依次删除 handle、记录 `closeErr`、投递通知并唤醒 parent 的结算 watcher，最后才 `close(current.done)`。等待者观察到 child 已结束时，通知已投递给 parent；这条规则写入 ADR-0013 的通知段落，不新增 ADR。
- 新增包级观察点 `released`，在 `Service.mu` 下、`done` 关闭后立即调用，默认空函数，与现有 `parked`、`beforePublish` 同类，只供测试固定交错。
- 锁顺序不变：`release` 原本就在持有 `Service.mu` 时调用 `Registry.Notify`（`registry.mu` 读锁，再取 agent 的 `agent.mu`）；关闭 channel 不取锁。`done` 的等待者等待时不持有任何锁，所以后移关闭点不引入新的等待环。
- `scripts/mutation-cases.json` 增加 `subagent-settle-notice-before-done`，把 `done` 关闭移回通知之前，由新测试杀死。
- 不修改测试的同步方式：修复后 `<-done` 与随后的 `WhenIdle` 已有 happens-before 保证，原测试无需改动。

## Consequences

`done` 现在表示释放已完整发布，包括通知与 parent watcher 唤醒，与上游一致；未来直接等待 `done` 而不重取 `Service.mu` 的调用方也不会早于通知观察到 child 结束。代价是一个仅测试使用的包级观察点。改动只涉及 `release` 尾部顺序，不触碰 descriptor 与消息 source，便于并行分支 rebase。

## Verification

- 修复前（只加入观察点）：`go test -count=50 -race -run 'TestService_SettlementNoticePrecedesReleaseWaiters$' ./internal/app/subagent/` 50/50 失败，报错 `parent idle when release waiters woke: <nil>`；该测试在 `released` 中以已取消 context 调用空闲 root 的 `WhenIdle`，root 的通知 turn 以 hold 阻塞，结果不依赖调度。
- 修复后同一命令通过；`go test -count=200 -cpu 1,2,4,8 -race -run 'TestService_SendMessageRoundTripAndColdResume$|TestService_SettlementNoticePrecedesReleaseWaiters$' ./internal/app/subagent/` 通过（每个测试 800 次）。
- `go test -race -count=3 ./internal/app/subagent/ ./internal/adapter/tool/subagent/` 通过；`internal/app/subagent` 语句覆盖率 100%。
- 只含新用例的清单运行 `python3 scripts/mutation-check.py --manifest .cache/mutation/settle-only.json --report .cache/mutation/settle-report.json`：`subagent-settle-notice-before-done: killed`。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，含全部 mutation 用例。

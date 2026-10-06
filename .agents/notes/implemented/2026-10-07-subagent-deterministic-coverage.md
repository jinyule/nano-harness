# Subagent 时序相关分支的确定性覆盖

- Status: implemented
- Date: 2026-10-07

## Context

单独运行 `internal/app/subagent` 测试时曾出现 99.8% 覆盖率，逐文件 100% 门禁随调度随机失败。基线为集成分支 `255745b`，产品代码未改。

调查方法：编译带 `-coverpkg=./internal/app/subagent` 的测试二进制（普通与 `-race` 两份），对 47 个测试逐个运行 30 次（`-test.cpu` 轮换 1/2/8，一半为 race 构建），统计每个语句块有没有至少一个测试在每次运行中都覆盖它。整包运行 50 次时每次都是 100%，并发负载掩盖了问题，因此只用逐测试稳定性判断。结果与分析：

| 语句（`service.go`，基线行号） | 修复前覆盖方式 | 时序依赖 |
|---|---|---|
| 618 `watch` 中 `!ended` 时取默认结局 | 无稳定测试；三个测试分别 15/30、21/30、22/30 | 只有 watcher 在 agent 关闭或服务取消后才读日志时，`Events` 失败才走到这里 |
| 820 `deliver` 等待关闭中的 child | 无稳定测试；`DeliveryWaitsForReleaseThenResumes` 6/10、25/30 | 投递 goroutine 与 `close(gate)` 竞争；原测试的“取消等待”实际在派发前就返回，从未进入等待 |
| 601 `watch` 循环开头发现 child 正在关闭 | 30/30，但依赖调度 | 测试只等 child 空闲，watcher 若在关闭完成后才调用 `WhenIdle`，会从 607 返回 |
| 641 停驻时服务取消 | 30/30，但依赖调度 | watcher 若在 stop 取消后才被调度，`WhenIdle` 的 select 可能先选中取消（607） |

协调者报告的“约第 595 行”位于 `watch` 中，与上表 601、618 对应；不稳定的是 618。上表之外的语句都有在每次运行中稳定覆盖它们的测试。

## Decision

只改测试，不改产品代码；上述分支都是可达的产品路径，不是死代码：618 对应 agent 在服务之外停止后日志不可读，或本次驻留没有提交任何 turn；820 对应投递遇到正在释放的 child。

- 618：新增 `TestService_ResidencyWithoutTurnSettlesAsFinished`。在 engine 上为 `agent-message` 注册拒绝所有 turn 的 admission，冷恢复的 child 接受消息但不提交 turn，本次驻留没有 `turn/end`。测试断言 parent 收到 `finished` 与 `It left no closing message.`，child 日志中没有该消息。
- 820：测试 helper `waitSignal` 在第一次调用 `Done` 时发出信号。投递路径进入等待之前只调用 `Err`，所以这个信号就是“投递已在等待释放”。`DeliveryWaitsForReleaseThenResumes` 先确认等待已开始再取消，断言 `ABORTED` 且保留 `context.Canceled`；再确认第二次投递在等待中，然后打开 release 屏障。
- 601、641：`OneShotParentReleasesItsContinuableTree` 与 `ReleaseEndsChildJobsAndStopClosesDeepestFirst` 改为等待 `parked` 观察点，确认 watcher 已停驻后再触发释放或 stop。前者只可能被关闭唤醒并在循环开头退出，后者只可能因取消退出。`observeParked` 先调用被包装的观察点，所以默认空函数仍被确定性执行；第一版没有这样包装，复查时发现第 46 行变为 16/30，已经修正。

## Consequences

逐文件 100% 门禁不再依赖调度。代价是测试 helper 依赖“等待前不调用 `Done`”这一实现细节；投递路径若在等待前新增 `Done` 调用或派生 context，信号会提前触发，等待分支可能不再执行，此时由逐文件覆盖率门禁暴露。admission 测试依赖 agent 包的 `ErrNotAdmitted` 契约。

## Verification

- 修复前逐测试统计见上表（`.cache/watchcov/pt2`，30 次/测试）。
- 修复后对 48 个测试各运行 30 次（同样轮换 CPU 并混合 race 构建）：每个语句块都有每次运行都覆盖它的测试；618、820、601、641 与第 46 行均为 30/30 或 40/40。
- 整包 coverprofile 110 次（60 次普通构建，`-test.cpu` 轮换 1/4/8；50 次 race 构建）：每次 `service.go` 与 `messages.go` 均为 100%，全部 PASS。
- `go test -count=100 -cpu 1,2,4,8 -race -run 'TestService_(ResidencyWithoutTurnSettlesAsFinished|DeliveryWaitsForReleaseThenResumes|OneShotParentReleasesItsContinuableTree|ReleaseEndsChildJobsAndStopClosesDeepestFirst)$' ./internal/app/subagent/` 通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过。

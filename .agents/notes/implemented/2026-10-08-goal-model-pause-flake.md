# goal driver 模型 pause 测试的偶发失败

- Status: implemented
- Date: 2026-10-08

## Context

main `0e1db11` 的 CI（run 37737034561，ubuntu-latest，Go 1.26.x，`go test -race -count=1 ./...`）报 `TestDriver_ModelPauseDoesNotInterrupt` 失败：`driver_test.go:394: interrupts=1 calls=0`。同一代码在 PR CI 与本地 `make check` 中都通过。

测试原来在 `play` 返回后立即 `stop`。`play` 把轮次结果写进容量为 1 的结果 channel 后返回，不等待 driver 取走。driver 的 `round` 在 `Followup` 返回后才进入 `select { case <-results: … case <-ctx.Done(): controller.Interrupt() … }`。若 driver goroutine 在 `Followup` 返回和进入 `select` 之间没有被调度，测试已经发送结果并经 scope cleanup 取消了 context；进入 `select` 时两个分支同时就绪，Go 随机选择，选中 shutdown 分支就调用一次 `Interrupt`。测试把这次关闭中断误判为模型 pause 的中断。pause 的 watcher 回调在 `Pause` 内同步执行，且只对 `ActorHost` 中断，这一行为没有竞态。

复现证据（本机 Go 1.27.0、darwin/arm64）：

- 原代码 `go test -race -count=500 -cpu 1,2,4,8 -run 'TestDriver_ModelPauseDoesNotInterrupt$' ./internal/app/goal` 一次运行出现 1/2000 失败，错误文本与 CI 相同；随后按单个 CPU 设置各 1,000 次未再失败。
- 八份同时运行的 race 测试二进制，各 `-test.count=2000 -test.cpu=1,4,8`：47,998 pass、2 fail。
- 在私有改动中于 `select` 前临时插入 `time.Sleep(20 * time.Millisecond)`，固定“结果与取消同时就绪”的交错：`-count=100` 下 `-cpu 1` 42 次、`-cpu 4` 48 次失败，与随机二选一一致；在 shutdown 分支临时打印 `len(results)`，57 次失败全部经过该分支且结果已在 channel 中。同样插桩下运行全部 `TestDriver*` 各 20 次，只有本测试失败。插桩未提交。

真实产品中，driver 最后启动、最先关闭；结果已就绪时多出的一次 `Interrupt` 只会取消此刻的活动 turn，而紧随其后的 agent cleanup 本身也会先 `Interrupt` 再关闭，因此不形成用户可观察的差异。这是测试 fixture 的同步缺陷，不是 driver 对模型 pause 的错误中断。

## Decision

只修测试，driver 代码、[ADR-0016](../../../docs/decisions/0016-long-running-goals.md) 与 [ADR-0018](../../../docs/decisions/0018-goal-stop-outcomes.md) 的行为与不变量不变。

`TestDriver_ModelPauseDoesNotInterrupt` 改用 fixture 已有的 `WhenIdle` 屏障（`fakeRoot.idle`）：driver 每次进入 `WhenIdle` 都把一个 release channel 交给测试并阻塞到释放。测试在 `play` 后接收下一个 release，这只可能发生在 driver 已从 `results` 分支取走结果之后，此时 context 尚未取消，不存在同时就绪；释放后，再等到 driver 下一次进入 `WhenIdle`，同时监听 `calls`，证明一次看到 paused 目标的调度没有排队新轮次。此时先断言没有中断，再 `stop`；driver 停在 `WhenIdle`，关闭不经过轮次的 shutdown 分支，关闭后仍断言零中断、零新轮次。屏障是 channel 交接，不丢通知，不依赖 sleep 或重试；create 的 wake 在第二次调度时仍在缓冲中，因而第二次 `WhenIdle` 一定到达。

新增定向 mutation `goal-model-pause-interrupt`：把 watcher 条件改为任何 pause 都中断，由该测试在关闭前的断言拒绝（`model pause interrupts = 1`），证明修复后的测试仍约束目标规则。[测试策略](../../../docs/testing.md)记录这一屏障与 mutation。

## Consequences

测试不再依赖 driver goroutine 与测试 goroutine 的相对调度，失败时能区分模型 pause 中断与关闭中断。没有改变产品代码、配置、持久化或测试并发度。

替代方案是在 driver 的 shutdown 分支先非阻塞检查结果、已结束的轮次不再中断。它增加一个只为测试时序存在的分支，产品上不可观察，因此不采纳；若将来 driver 不再最先关闭，或 agent cleanup 不再先中断活动 turn，再重新评估。同包其他 `TestDriver*` 在同样插桩下不受该窗口影响，未改动。

## Verification

- 修复后：`go test -race -count=2000 -cpu 1,2,4,8 -run 'TestDriver_ModelPauseDoesNotInterrupt$' ./internal/app/goal` 8,000 次通过，约 3 s；`go test -race -count=200 -cpu 1,4,8 -run 'TestDriver|TestNewDriver' ./internal/app/goal` 通过，约 2.5 s。
- 与修复前相同的八份并发二进制负载（各 `-test.count=2000 -test.cpu=1,4,8`）：48,000 pass、0 fail。日志位于忽略的 `.cache/goal-pause-flake/`。
- 保留 20 ms 临时插桩运行修复后的测试，`-cpu 1` 与 `-cpu 4` 各 100 次，0 失败。
- 临时去掉 `change.Actor == ActorHost` 条件，修复后的测试以 `driver_test.go:405: model pause interrupts = 1` 失败。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：race 全量测试、lint 0 issues、架构、submodule、每个产品源文件 100% coverage、242 个定向 mutation 全部 killed（含 `goal-model-pause-interrupt`）与 binary smoke。`AGENT_NOTE_BASE_REF=main make agent-notes` 与 `git diff --check` 通过。
- 未在 Linux CI runner 上做重复负载；CI 的原始失败按上述交错解释，稳定性证据来自本机。

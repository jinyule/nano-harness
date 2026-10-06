# settings writer lock 的取消仲裁与偶发失败

- Status: implemented
- Date: 2026-10-06

## Context

基线 `7ffe646c870a25fd37b52df8ad7fa854199a56db` 的 `TestProviderWatchAndAtomicFailures` 用真实 1 ms 锁期限测试超时，随后用同一期限测试预先取消的 context。全量并行 race 负载下，独占 open 或调度可能耗尽这 1 ms；进入 `select` 时取消与期限都已就绪，Go 随机选择期限，报 `file_test.go:205: cancel=settings writer lock timed out`。这是取消错误被产品的锁等待仲裁掩盖，短期限使缺陷更容易出现。

产品还有同一取消边界的缺口：`withLock` 仅在 open 返回 `os.ErrExist` 后检查 context，锁空闲时预先取消的 `Persist` 仍成功写入；取消发生在取得锁期间时也照常写入并更新 provider 的 hash。真实 `composeApplication` 的 settings update 在旧实现上改写文件，并把 route 改成 Anthropic、revision 从 1 增加到 2。

本机为 Go 1.27.0、darwin/arm64。单个编译好的 race 二进制以 `-test.count=5000 -test.cpu=1,4,8` 运行，并叠加四个其他包的 race 测试，15,000 次未失败；八份二进制并发的完整样本为 27/24,000 次失败（0.1125%），全部是上述取消错误。中断前另有未完成的八份负载样本，仅作原始诊断，不混入完整样本分母。

## Decision

修补 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md#2-provider-neutral-llm-与-provider-owned-wireauth) 的 settings 写入契约：每次 open 前检查 context，成功取得并关闭锁文件后再检查一次；期限分支重新检查 context，取消与期限同时就绪时返回原 context 错误。开始锁内原子写入后完整执行原有提交路径，迟到取消不撤销已经开始的写入。2 s 产品锁等待期限、25 ms 重试间隔和原测试的 1 ms 期限均保留。

永久测试用 channel 暂停真实锁文件的取得，取消后才释放；用 `testing/synctest` 虚拟时间让被屏障阻塞的 open 跨过锁期限，随后取消并释放，每次测试断言 32 个同时就绪交错全部返回取消。另验证纯等待取消与写入开始后的完整提交。测试从真实文件字节、目录条目和 provider hash 观察结果；`cmd` 的真实配置、composition 和 Plugin Runtime 测试再检查磁盘、snapshot revision 与 lock/temp 清理。

`settings` consumer、`settings-file` provider 与 `cmd` composition 的角色保持原有 ownership。此次没有新增运行时 effect；同步 writer lock 由调用结束时删除，watch goroutine 仍由挂载 Scope 取消并等待。三个定向 mutation 分别删除入口取消检查、取得锁后的取消检查和期限仲裁检查，永久测试拒绝这些回归。

与 [核心 Harness Note](2026-08-24-core-agent-harness.md) 部分重叠：它继续拥有组件组合与 provider-neutral 接缝，本 Note 拥有 settings 锁调查和回归证据；两个记录互链，无需归档。其他 settings effort/web 配置 Note 不涉及此锁的取消契约。

## Consequences

调用方可以稳定用 `errors.Is(err, context.Canceled)` 识别锁等待取消，预先取消和取得锁期间取消不再替换设置文件或提交新 revision。原子写入开始后的提交语义由文件证据固定。没有增加配置、接口、等待时长或 sleep，也没有串行化测试或更改持久化格式。

同步 OS open 不能被 context 中断；取消在其返回后被观察。已开始的原子写入仍可能因 I/O 失败返回错误。并发样本只证明本机观察条件下的稳定性，不能代替 Linux/Windows 原生执行；credentials 的独立锁实现不属于此次 settings 修复范围。

## Verification

负载复现先编译：

```bash
mkdir -p .cache/settings-lock-flake
go test -race -c -o .cache/settings-lock-flake/before.test ./internal/adapter/settings/file
.cache/settings-lock-flake/before.test -test.v \
  -test.run='^TestProviderWatchAndAtomicFailures$' \
  -test.count=1000 -test.cpu=1,4,8 -test.timeout=180s
```

并发运行八份上述二进制，各自写独立日志；同时分别循环运行以下四个包的 `go test -race -count=1`，直到全部二进制退出，再等待最后一轮负载结束。每份二进制运行三个 CPU 设置，共 3,000 次。修复前结果为 23,973 pass、27 fail，八份进程退出码为 `1,0,1,1,1,1,1,1`；负载包完成轮数分别为 `15,5,4,2`，全部成功：

```bash
go test -race -count=1 ./internal/app/agent
go test -race -count=1 ./internal/app/subagent
go test -race -count=1 ./internal/adapter/model/provider
go test -race -count=1 ./internal/adapter/tui
```

在私有源码副本中保留永久测试、恢复基线的 `file.go`，使用 `go test -race -c` 编译后执行 `-test.count=10 -test.cpu=1,4,8 -test.run='^(TestProvider_PersistCanceled.*|TestWithLock_CancellationWinsExpiredWait)$'`：三个测试各 30/30 失败，分别报告 `canceled persist = <nil>`、`canceled persist replaced the document` 与 `expired lock wait masked cancellation: settings writer lock timed out`。真实入口修复前执行 `go test -race -count=1 -run '^TestComposition_CanceledSettingsUpdatePreservesFileAndRevision$' ./cmd/nano-harness`，按取消返回值、磁盘字节和 revision 三项断言失败。

修复后 owning package 与上述 composition focused race 测试通过。编译后的 race 二进制运行 `-test.count=100 -test.cpu=1,4,8 -test.run='^(TestProvider_Persist.*|TestWithLock_.*)$'` 通过，涵盖取消、同时就绪与提交边界。

修复后将相同负载命令的二进制换成新编译的 `after.test`，八份二进制仍各运行 `-test.count=1000 -test.cpu=1,4,8`：24,000 pass、0 fail，八个退出码均为 0；四个负载包分别完成 `23,7,6,3` 轮，全部通过。完整前后负载耗时分别为 42.95 s 与 67.95 s；宿主调度与负载轮数不同，样本用于稳定性验证，不作为性能比较。日志与统计位于忽略的 `.cache/settings-lock-flake/`，不进入产品或 commit。

完成检查：

- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括 `go test -race -count=1 ./...`、格式/模块/vet、架构、submodule、Agent Note、skills、workflow helper 反例、lint、coverage、mutation 与真实 binary smoke。所有产品源文件 coverage 为 100%；`file.go` 的原始 profile 有 95 个 block，全部命中。38 个定向 mutation 全部 killed，其中三个 settings 用例的失败来自具名永久测试。
- `make tui-e2e` 通过：真实 binary/PTY 完成 19 个 root tool call，以及图片、后台任务、提问、规划、目标轮次、子代理、审批、文件、打断、恢复与 cleanup 场景。
- `git diff --check`、`make agent-notes`、`make architecture` 与受影响文档文件链接检查通过。审查范围为上述基线到当前 worktree；没有第三方 submodule、归档 Note、依赖或版本变更。未运行远端 provider live 请求和其他 OS 原生矩阵。

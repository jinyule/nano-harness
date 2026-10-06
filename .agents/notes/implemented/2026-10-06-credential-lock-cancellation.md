# credential writer lock 的取消与事务边界

- Status: implemented
- Date: 2026-10-06

## Context

基线 `bbfd8a5f89ace57fd1af047e90e294bcfcd3f3d9` 已包含 settings 锁的修复，credentials 仍有独立实现。`TestLockAndRandomFailure` 把锁期限缩为真实 1 ms 后测试预先取消；open 或调度超过期限时，取消与期限同时就绪，`select` 随机返回 `credential writer lock timed out`。八份 race 二进制并发、叠加四个包的 race 测试时 24,000 次未失败；十六份二进制在相同 CPU/重复设置下失败 62/48,000 次（0.1292%），全部为 `store_test.go:209: lock cancel=credential writer lock timed out`。

取消还有三个遗漏边界：空闲锁直接运行已取消的 modify/delete；取得锁或读取文件期间发生取消后仍进入 mutation；同进程的 `writeMu.Lock` 不受 context 或文件锁期限控制，取消等待者必须等待持有者的 refresh 返回。文件锁已经覆盖同一 read-decide-write 事务，额外 mutex 既不增加互斥保证，也让等待取消失效。

`llm.Runtime.PrepareCall` 在 `Store.Modify` 回调内重新读取当前记录、判断期限并调用 provider refresh；Login 与 Logout 分别沿 Modify 与 Delete。成功刷新可能已在远端轮换 token，因此回调已经开始后不能无条件用迟到取消丢弃新 grant。OpenAI/Anthropic 的网络刷新携带原 context，刷新失败沿回调错误返回，不持久化。

## Decision

credentials 采用与 settings 相同的锁取消仲裁：每次 open 前、成功取得并关闭锁文件后检查 context；锁期限分支重新检查，取消与期限同时就绪时返回原 context 错误。Modify 在读完文件后、进入 mutation 前再检查一次。删除重复 `writeMu`，同进程与跨进程调用都由既有 `O_EXCL` 文件锁串行化，并沿同一可取消等待路径退出。30 s 锁期限、25 ms 重试间隔及原测试的 1 ms 期限保留。

回调开始后沿原提交契约处理：回调错误不写入；合法成功结果完成原子保存，迟到取消不抛弃可能已轮换的新凭据。固定版本 YAML、权限、随机暂存、`fsync` 与 rename 不变。长期契约由现有 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md#2-provider-neutral-llm-与-provider-owned-wireauth) 拥有，安全文档同步单一锁边界，不新增 ADR 或可部署配置。

永久测试用 channel 暂停真实 open/read，虚拟时间固定取消与锁期限同时就绪。不可取消 mutex 的旧交错放在隔离子进程：其 synctest 死锁退出由父测试变成具名失败，父测试拥有子进程与临时目录。另一个独立子进程实际持有文件锁，取消等待者不能删除持有者的锁或修改文件，持有者退出后锁可复用。刷新提交边界分别验证回调取消错误保留原文件、成功结果在迟到取消后仍完整保存。真实 `cmd` 配置/composition/Plugin Runtime 的 Logout 从磁盘字节与无密钥账户列表验证取消不删除记录。

`credentials-file` provider、`llm.CredentialStore` consumer 接口与 `cmd` caller 保持原接缝；没有新增运行时 effect。同步锁和暂存文件由调用返回时回收，测试 goroutine 均有 channel join，子进程均有 Wait。四个定向 mutation 分别移除入口、取得锁后、期限仲裁和 mutation 前的取消检查。

与 [settings 锁 Note](2026-10-06-settings-lock-flake.md) 部分重叠且互链：它保留 settings 修复证据，本 Note 拥有独立 credential 锁、刷新与删除边界；核心 Harness Note 继续拥有组件组合。测试只使用合成凭据，不输出凭据对象、文件内容或 token，不读取用户账户。

## Consequences

取消原因不再被锁超时掩盖，取消的 modify/refresh/delete 不进入尚未开始的 mutation；同进程等待者不再受不可取消 mutex 阻塞。同一文件锁继续保证进程内与跨进程事务互斥，无需引入另一信号量或队列。30 s 文件锁期限现在也约束同进程竞争，等待重试间隔为既有 25 ms。

同步文件系统 open/read 仍只能在返回后观察取消。回调开始后的持久化可能失败；成功回调与原子保存都不因迟到取消额外中断。进程崩溃留下的锁沿既有超时路径处理，不新增自动抢锁或恢复机制。本机验证为 Go 1.27.0、darwin/arm64，不代替其他 OS 的原生矩阵或真实 OAuth 服务验证。

## Verification

修复前先编译 race 二进制，再运行原测试：

```bash
mkdir -p .cache/credential-lock-cancellation
go test -race -c -o .cache/credential-lock-cancellation/before.test ./internal/adapter/credential/file
.cache/credential-lock-cancellation/before.test -test.v \
  -test.run='^TestLockAndRandomFailure$' \
  -test.count=1000 -test.cpu=1,4,8 -test.timeout=180s
```

分别并发运行八份与十六份上述二进制，每份独立日志；同时分别循环 `go test -race -count=1` 测试 `internal/app/agent`、`internal/app/subagent`、`internal/adapter/model/provider`、`internal/adapter/tui`，目标全部退出后停止新负载并等待最后一轮结束。八份样本 24,000/24,000 通过，负载轮数 `10,4,2,2`；十六份样本 47,938 pass、62 fail，负载轮数 `24,7,5,3`，负载包均通过。后者完整耗时 64.95 s，失败率 0.1292%；未改变原测试或产品期限以制造结果。

永久测试在修复前的 race 二进制执行 `-test.count=10 -test.cpu=1,4,8 -test.run='^(TestStore_CanceledBeforeTransaction|TestStore_CanceledDuringAcquisitionAndRead|TestWithLock_CancellationWinsExpiredWait)$'`：三个具名测试各 30/30 失败，四个 modify/delete/acquire/read 子场景也各 30/30 失败；取消变为 nil、mutation 被调用及磁盘改变由独立断言报告。`TestStore_CanceledWaiterReturnsBeforeHolder` 用同样重复设置另跑 30 次，全部具名失败，隔离子进程因旧 mutex 的不可取消等待而退出 2，父进程没有超时。`TestComposition_CanceledCredentialLogoutPreservesFile` 在修复前的 `go test -race -count=1 -run '^TestComposition_CanceledCredentialLogoutPreservesFile$' ./cmd/nano-harness` 中按错误、文件字节与账户列表三项失败。

修复后 owning package 与上述真实 composition focused race 测试通过。所有原始负载与统计保存于忽略的 `.cache/credential-lock-cancellation/`；不提交测试二进制或诊断输出。

修复后的新 race 二进制在同样十六进程、`-test.count=1000 -test.cpu=1,4,8` 和四包负载下，48,000 pass、0 fail，十六个退出码均为 0；四包分别完成 `24,8,5,3` 轮，全部通过，耗时 61.91 s。前后时长仅记录观察条件，不作性能结论。

编译后的二进制运行 `-test.count=100 -test.cpu=1,4,8 -test.run='^(TestStore_CanceledBeforeTransaction|TestStore_CanceledDuringAcquisitionAndRead|TestWithLock_.*|TestStore_RefreshCommitBoundary)$'`：五个具名测试各 300/300 通过。另运行 `-test.count=10 -test.cpu=1,4,8 -test.run='^TestStore_(CanceledWaiterReturnsBeforeHolder|CrossProcessCancellationPreservesHolder)$'`：两项各 30/30 通过，覆盖 waiter 提前取消返回和实际跨进程独占锁的保留、释放与复用。

`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括全仓无缓存 race 测试、格式、模块、vet、架构、submodule、Agent Note、skills、workflow helpers、lint、逐产品源文件 100% statement coverage、53 个定向 mutation 与真实 cmd 构建/version smoke。本次四个 credential mutation 均被目标测试拒绝，`store.go` 没有零计数 coverage block；mutation 在私有副本运行，不修改工作树或参考 submodule。

`make tui-e2e` 通过真实 binary/PTY 与本地模型 fixture 验证，覆盖 19 次根工具调用、附件、计划、提问、goal、子 agent、approval、文件、终端输入/resize/wrap、interrupt、resume 与 cleanup。凭据取消的专属 composition 测试由 focused race 和全仓 race 门禁执行；本次不调用真实 OAuth 服务。

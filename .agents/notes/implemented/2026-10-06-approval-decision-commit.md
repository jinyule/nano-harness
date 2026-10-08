# approval 决定提交与停止状态切换串行化

- Status: implemented
- Date: 2026-10-06

## Context

`approval.Service.Decide` 在 broker 返回后检查一次停止原因，再在锁外以不可取消 context 追加 `approval/decided`。屏障暂停这个追加时，cleanup 能先取消请求；释放追加后，返回值与日志仍是 `allowed-once/operator`。调用方原始 context 仍有效，真实 tool runtime 会据此执行。正常 composition 先停止并等待 agent，遮蔽这个窗口，但 approval 插件自身必须满足关闭契约。

[cleanup Note](2026-10-06-approval-retry-compaction-cleanup.md)继续拥有三个服务的在途登记、取消与 join 决定；本 Note 补齐 approval 的最终提交边界，两者部分重叠并互链，不归档。[ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md#5-工具approval-与-sandbox)拥有授权与持久化契约，具体关闭规则由[架构](../../../docs/architecture.md#工具approval-与调度)维护。

## Decision

`Decide` 在同一服务锁内选择最终结局、追加 decided 并返回该结局，与 cleanup 设置 inactive 串行化。停止先取得锁时，所有已提交问题的决定都变为 `cancelled/cancellation`，不依赖 context 中先被设置的取消原因；决定提交先取得锁时，保留该结局，cleanup 等待提交与调用返回。只在追加之后检查停止并修改返回值会使授权日志与执行结果不一致，因此不采用。

消费者接口仍由 `app/tool` 拥有，`approval.Service` 是 provider 插件，真实 consumer 为 tool runtime；`cmd` 显式注入这条接缝。没有新增 effect，调用仍由 approval Scope 取消并 join。asked 之后的 decided 使用原有不可取消、5 秒提交期限，cleanup 返回前完成配对；追加失败仍通过原错误返回，runtime 失败关闭。既有 ADR 同步补充终态仲裁，不新增 ADR 或持久化字段。

## Consequences

停止不能穿过允许决定的提交窗口，持久化与返回共享同一结局。代价是 decided 追加期间服务锁阻塞其他 approval 操作与停止转换；已开始的提交先完成，不能撤销已经提交的允许。broker 或 journal 忽略取消与期限时仍会延长关闭，这是既有边界。

## Verification

- 修复前，先添加永久 `TestService_CleanupDuringDecisionCommit`，运行 `go test -race -count=1 ./internal/app/approval -run '^TestService_CleanupDuringDecisionCommit$' -v` 退出 1：`commitLocked=false outcome=allowed-once err=<nil>`，落盘为 `allowed-once/operator`，期望为 `cancelled/cancellation`。channel 暂停 decided 追加；追加开始后、cleanup 启动前的 TryLock 区分提交是否已拥有停止锁，未串行化时等待真实 cleanup 取消再释放追加，没有 sleep。
- 修复后同一测试约束提交先于停止时的持久化/返回一致、cleanup join、asked/decided 配对及停止后拒绝新决定；既有 `TestService_CleanupSettlesPendingDecisions` 约束停止先发生时的取消结局。
- `TestComposition_ApprovalCleanupRejectsLateConsent` 通过真实配置、composition、loopback Responses、approval、tool runtime、文件工具与 JSONL，仅关闭 approval Scope，broker 在取消后仍回答允许。原 turn 保持有效并完成下一模型请求；磁盘决定为 `cancelled/cancellation`，工具结果为 `Error: approval cancelled`，真实目标文件不存在。
- 默认 mutation 清单增加 `approval-decision-commit-serializes-stop`，提前释放最终提交锁；原 `approval-shutdown-cancels-decision` 更新定位，移除最终停止仲裁。具名永久测试分别约束这两种回归。
- 最终版测试增加 Scope 关闭入口的 channel，保证释放 decided 追加前 Close 已开始；以 `c7d6012` 的旧 service 为 overlay，`go test -race -count=1 -overlay .cache/approval-before-overlay.json ./internal/app/approval -run '^TestService_CleanupDuringDecisionCommit$' -v` 同样退出 1，失败仍是 `allowed-once/operator` 与目标取消结局不符。源码与日志分别保存为本地 `.cache/approval-service-before.go`、`.cache/approval-before.log`。
- `go test -race -count=1 ./internal/app/approval ./cmd/nano-harness -run '^(TestService.*|TestComposition_ApprovalCleanupRejectsLateConsent)$'` 通过；增加关闭入口屏障后，同一精确回归的 race 测试再次通过。
- `python3 scripts/mutation-check.py --manifest .cache/approval-mutations.json --report .cache/approval-mutation-report.json` 的两个提交/停止回归均 killed。测试枚举选择先后触发 `exhaustive` 与 `staticcheck/QF1003`，改成两个独立条件后，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make lint` 报告 `0 issues`，没有规则豁免。
- 屏障测试在 Append 返回前直接观测请求取消，再核对最终结局；TryLock 只用于构造 cleanup 与提交的顺序，不作为预期授权的来源。同一回归的 race 测试通过，旧源码 overlay 仍以同一授权错误失败。
- `BASE_REF=c7d6012951b80bc8828507d605f343a108372b51 AGENT_NOTE_BASE_REF=c7d6012951b80bc8828507d605f343a108372b51 GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出 0：全仓 race、架构、submodule、Notes/skills/workflow、lint（0 issues）、逐产品文件 100% coverage、144/144 mutation killed 与真实 cmd build/version 通过。
- rebase 到 `feat/upstream-tool-parity` 的 `8dde7766343714c225d0f5c94b73df7bbbdc1c0b`。冲突仅在 mutation 清单尾部：保留集成分支全部 151 项，加上 approval 回归后为 152 项；停止仲裁的原有 site 更新保留。rebase 后的两个 owning package focused race 测试再次通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e BINARY=nano-harness-approval` 退出 0：真实二进制/PTY 的 19 次 root 工具调用、审批、文件、提问、sandbox、规划/目标、spawn/fork、中断、恢复与 cleanup 通过。独立 binary 避免与同时进行的 make check 构建输出共用路径。本机为 Go 1.27.0、darwin/arm64；没有 live provider 或 Linux/Windows 原生执行证据。
- 最终基线运行 `BASE_REF=8dde7766343714c225d0f5c94b73df7bbbdc1c0b AGENT_NOTE_BASE_REF=8dde7766343714c225d0f5c94b73df7bbbdc1c0b GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出 0：全仓 race、架构、submodule、Agent Note 携带（两份）、skills/workflow、lint（0 issues）、逐产品文件 100% coverage、152/152 mutation killed 与真实 cmd build/version 全部通过，报告为本地 `.cache/approval-make-check.log` 与 `.cache/mutation/report.json`。最终补充仅涉及本 Note 的实测结果，Go 源码与测试不变。

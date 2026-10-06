# 终审收尾：context 段落顺序、web cleanup 保护与版本文档

- Status: implemented
- Date: 2026-10-07

## Context

最终联合评审（opus 与 Codex，基线 `357b51e`）没有 Blocker，但留下五个建议项：

1. `docs/architecture.md` 逐项抄写 composition 版本，其中 `goal-tools-v2` 已过时，还漏了 search、question、plan 三个 token；[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 已声明 `cmd/nano-harness/main.go` 的 `compositionID` 是权威来源。
2. 删除 `internal/app/web/web.go` cleanup 中的 `Cancel` 后，`TestService_ShutdownCancelsAndWaitsForInFlightOperations` 在无界的 `<-cancelled` 上挂起，只能靠包级超时失败；plan、approval、retry、compaction 都有同类定向 mutation，web 没有。
3. runtime 快照的段落按插件注册顺序拼接。上游 `packages/core/system-prompt/src/index.ts` 的 `CONTEXT_ORDERS`（`SANDBOX_POLICY: 110`、`SUBAGENT_DELEGATION: 120`）在组装时按 order 排序。当前组合恰好先注册 sandbox，输出一致；一旦调整插件顺序，快照文本和 resume 去重都会改变。
4. `docs/testing.md` 仍写“后注册者能看到前者的贡献”，而 engine 先收集全部贡献、再提交，各 provider 观察同一份日志。
5. `internal/app/subagent` 导出的 `SourceRuntimeContext` 在 K2 之后只剩测试使用；快照的来源由 engine 写入。

## Decision

- `agent.ContextContribution.Sections` 改为 `[]ContextSection{Order, Text}`。agent 包按上游定义 `OrderSandboxPolicy = 110` 与 `OrderSubagentDelegation = 120`，sandbox 与委派 provider 声明各自的 order。engine 收集全部贡献后用 `slices.SortStableFunc` 按 order 排序再合并，同 order 保持注册顺序；独立消息仍按注册顺序跟在快照之后。当前组合下快照文本逐字不变：sandbox 原本就在委派之前注册。
- web 测试的阻塞调用在 release 时也能返回，等待 cleanup 取消的步骤有 10 s 上界和具名失败；测试在 fixture 之后注册释放 cleanup，失败时不会让 scope 关闭挂起。新增 mutation `web-cleanup-cancels-operations`，与其他四个服务的同类项对齐。
- 删除 `subagent.SourceRuntimeContext`。快照的来源种类由 engine 拥有，`internal/app/agent/context.go` 与 TUI 继续使用同一字面量；测试改用测试内常量。
- architecture 只描述各 token 绑定的语义类别，当前版本链接到 `compositionID`。ADR-0016 中已过时的“使用 `goal-tools-v2`”，以及 ADR-0011、ADR-0013 中会过时的现时版本表述，改为“提升历史 + 当前值以 `compositionID` 为准”。其他 ADR 记录的是各自决策时的版本迁移，保留不动。security、testing、README 没有现时版本列表。
- testing.md、architecture.md、ADR-0021 与参考分析中关于快照拼接顺序的描述改为按 order 排序；testing.md 改写为“各 provider 观察同一份日志，engine 收集后提交”。

没有新增插件、goroutine、配置或依赖；模型可见文本、schema、composition token 与 session 格式都不变。

## Consequences

快照段落顺序与上游一致，并且不再受插件注册顺序影响；调整组合顺序不会改变快照文本，也不会让 resume 的去重失效。新的 provider 必须选择 order，同 order 时才由注册顺序决定。web cleanup 回归在约 10 s 内被具名测试拒绝，不再依赖包级超时。版本号只在 `compositionID` 维护，architecture 不会再与代码不一致；ADR 和实施 Note 中的完整列表是带时间点的快照。

`SourceRuntimeContext` 删除后，外部包不能再引用这个常量；目前没有生产代码需要它。

## Verification

- 修复前：`go test -count=1 -run 'TestService_SandboxPolicyPrecedesDelegationRegardlessOfRegistration' ./internal/app/subagent/` 失败，在 sandbox 后注册时快照中委派段落位于 sandbox 段落之前；输出在被忽略的 `.cache/final-polish/order-red.log`。删除 web cleanup 中 `Cancel` 的 overlay 下，原测试以 `-timeout 30s` 超时失败（`.cache/final-polish/web-red.log`）；测试加上界后，同一 overlay 在约 10.6 s 内以 `cleanup did not cancel the in-flight search` 失败，cleanup 正常结束。
- 修复后：`TestEngine_SnapshotSectionsFollowOrderNotRegistration` 固定排序以及同 order 时的注册顺序；subagent 的注册顺序测试，以及真实组合 `TestComposition_SubagentsEndToEnd` 中“sandbox 段之后紧接委派段”的既有断言都通过。`go test -race -count=1 ./internal/app/agent ./internal/app/subagent ./internal/app/web` 通过。
- 两个新 mutation（`web-cleanup-cancels-operations`、`context-sections-follow-order`）用单独清单运行 `scripts/mutation-check.py`，均被具名测试拒绝。
- 基于 `357b51e`：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括全仓 race、逐产品文件 100% coverage、架构、Agent Note、lint 0 issues、全部默认 mutation 与真实 cmd build/smoke；`make tui-e2e` 通过（真实二进制与 PTY 下 19 个 root 工具调用，含 spawn/fork 子代理），终端文本不变。没有 live provider 或其他操作系统的证据。

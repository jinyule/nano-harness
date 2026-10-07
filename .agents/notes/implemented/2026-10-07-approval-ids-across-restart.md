# approval ID 跨进程唯一

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查 B1：`approval.Service.Decide` 用进程内原子计数生成 `approval-<n>`，计数随新的 Service 从 0 开始。`Restore` 只恢复 `approval/policy`，不读取已有 ID。会话恢复后第一次需要审批的工具调用会生成日志中已有的 `approval-1`，JSONL 顺序校验以 `corrupt session: duplicate approval ID "approval-1"` 拒绝追加。工具得到 `Error: approval could not be recorded`，问题到不了用户，较长历史中的重试还会消耗步数。

这是 `main`（`9a75ea1`）已有的缺陷：`git show main:internal/app/approval/service.go` 同样使用 `fmt.Sprintf("approval-%d", service.nextID.Add(1))`，`main` 的 JSONL 同样拒绝重复 ID。把下文的真实 JSONL 测试放进 `main` 的源码快照运行，得到相同失败。

上游 `packages/interaction/user-approval/src/index.ts` 的 `request` 每次用 `randomUUID()` 生成 ID，不依赖进程状态，也不需要从日志恢复。

## Decision

与上游对齐，改用随机 ID：`id := "approval-" + rand.Text()`。`crypto/rand.Text()` 返回 26 个 base32 字符，至少 128 位熵，不会返回错误，并发调用安全，因此删除计数器，无需持锁或从日志恢复水位。保留 `approval-` 前缀，与本仓 `goal-`、`session-` 的随机 ID 风格一致。

没有选择“从日志恢复最大编号”，原因有三：

- 计数器由服务内全部会话共享，恢复水位要在每次 `Restore` 时合并每个打开的日志（root、子代理和 fork 种子），并与进行中的分配同步；
- 正确性会依赖每个日志写入方都先经过 `Restore`；
- 旧日志或手工日志中的 ID 不一定是 `approval-<n>` 格式。

随机 ID 不改变持久化格式和 composition ID：ID 仍是 1–128 字节的 opaque 字符串，旧日志中的 `approval-<n>` 照常读取，不迁移或改写。理论上随机碰撞仍会被日志拒绝并失败关闭。契约写在 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md)，[架构](../../../docs/architecture.md#工具approval-与调度)引用它。

## Consequences

恢复、重启或重建 composition 后，审批能正常到达用户；并发决定互不协调。ID 从短编号变成 35 字节，TUI 不显示 approval ID，模型也看不到它。

与 [approval 决定提交 Note](2026-10-06-approval-decision-commit.md) 和 [cleanup 静止 Note](2026-10-06-approval-retry-compaction-cleanup.md) 相邻但不重叠：它们拥有决定提交与关闭语义，本 Note 拥有 ID 生成的修复证据，均不归档。

## Verification

- 修复前：
  - `go test -race -count=1 -run TestApproval_IDsStayUniqueAcrossRestart ./internal/adapter/session/jsonl/` 失败，报 `question in turn 2 = "", corrupt session: duplicate approval ID "approval-1"`。同一测试放进 `git archive main` 的快照中运行，失败相同。
  - `TestComposition_ApprovalAfterRestartReachesTheOperator` 失败，报 `approvals across restart: 1 asked, 1 decided, failed results ["Error: approval could not be recorded"]`。
  - `TestService_QuestionIDsNeedNoProcessState` 失败。
- 永久测试：
  - `internal/adapter/session/jsonl/approval_test.go`：真实 JSONL 关闭、重开，新建 approval Service 后再次提问，两个问题 ID 不同且都已提交。
  - `cmd/nano-harness/approval_test.go`：真实 composition 两次从配置构建，第二次恢复第一次的 transcript；两次 `write` 都经 operator 批准，两组 asked/decided ID 配对且互不相同，两个文件都真实写入。
  - `internal/app/approval/service_test.go`：两个 Service（模拟重启）各并发决定 8 次，16 个 ID 全部带前缀且不重复。
- 新增 mutation `approval-id-process-independent`：把 ID 改为由 call ID 派生，被 `TestService_QuestionIDsNeedNoProcessState` 杀死；approval 既有 mutation 仍全部 killed。
- 修复后 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`（Go 1.27.0、darwin/arm64，基于 `74f420b`）退出码 0：race 测试、架构、submodule、Agent Note、skills、lint（0 issues）、逐产品文件 100.0% coverage、190 个 mutation 全部 killed 与真实 binary smoke 均通过。`make tui-e2e` 通过，覆盖真实 binary/PTY 下的审批、恢复与退出清理场景。

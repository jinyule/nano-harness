# 修复 spill 会话目录、提问取消与会话 call 校验

- Status: implemented
- Date: 2026-10-06

## Context

基线 `d1f31e3f6cfbefba4df225132f24491669abc389` 的三个边界缺陷由永久测试稳定复现：spill 会话目录只创建而未校验，取消后的合法答案通过提问接缝，todo 快照只检查 call ID 存在。核对 call 关联还发现 `approval/decided` 接受无生产消费者的 `call_id`。测试在修改产品代码前运行，均以目标断言失败。

工作限于 `wp/codex-misc` 专用 worktree，参考 submodule 只读。[spill Note](2026-10-05-tool-output-spill-and-read-before-write.md)、[提问与规划 Note](2026-10-04-ask-user-question-and-plan-mode.md) 和 [todo Note](2026-10-04-todo-write-tool.md) 保留原有能力、生命周期和发布理由；本 Note 部分补充它们的边界校验与取消证据，不归档旧记录。

## Decision

本次修补沿用既有契约，不新增 ADR：spill 目录归属 [ADR-0008](../../../docs/decisions/0008-tool-output-spill-and-observation-policy.md)，提问与退出取消归属 [ADR-0014](../../../docs/decisions/0014-user-questions-and-plan-mode.md)，todo call 类型归属 [ADR-0010](../../../docs/decisions/0010-todo-write-session-record.md)，approval 持久化关联归属 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md)。当前文件、提问和日志边界由[安全规则](../../../docs/security.md)、[架构](../../../docs/architecture.md)和[测试策略](../../../docs/testing.md)拥有。

- spill 的 `createIn` 在 `layout` 锁内复用 `preparePrivate(dir, lstatPath)`；错误通过 `%w` 保留目录准备失败及原原因。现有 `spill-local` 插件仍由真实 `cmd` composition 启动，Scope 撤销 store、等待已打开文件并取消/join sweep；没有新增 effect。
- `question.Service.Ask` 在 broker 返回后优先检查取消。`plan.Service.Exit` 显式接收 context，在状态锁内检查取消后才更新待生效选择；全部调用方同步更新。问题与规划插件的注册和 Scope 清理不变。
- JSONL order validator 要求 todo 的 pending call 名称为 `todo_write`；领域 shape validator 拒绝 `approval/decided` 的额外 call ID。合法固定样本仍逐字比较，错误快照追加不改变磁盘字节。
- `plan/mode`、`goal/change` 和 `subagent/catalog` 没有 call 引用字段，无同类缺口；`approval/asked` 已核对调用名称，`tool/result` 本就允许关联任意工具类型。
- 保留原 workspace 链接 mutation，增加会话目录、取消答案、退出选择取消、todo 工具类型和 approval 多余关联五项定向 mutation。
- `TestService_LimitsLiveJobsPerOwner` 用 channel 固定 producer 已停止接收工作但尚未结算的状态，再断言它仍占名额；cleanup 始终释放屏障并由服务 Scope join producer。[后台 job Note](2026-10-04-background-jobs.md) 的产品决定保留，本 Note 只补充该 fixture 的同步证据。

## Consequences

三个缺陷都在真实执行或读取边界被拒绝，取消审查不会在后续 turn 退出规划模式。session v2 与工具定义不变，非法既有日志整体拒绝且原文件保留；不增加兼容层或迁移。`os.Root` 的评估见 ADR-0008，保留的同一用户 TOCTOU 边界见安全规则。

## Verification

- 修复前：`go test -race -count=1 ./internal/adapter/spill -run '^TestStore_RejectsUnsafeSessionDirectories$'` 失败，启动前和 sweep 后的 symlink、`0755` 会话目录均报 `unsafe session directory accepted`。
- 修复前：`go test -race -count=1 ./internal/app/question -run '^TestService_CancellationWinsOverBrokerAnswers$'` 失败，broker 等待 `ctx.Done()` 后返回合法 `Approve`，接缝仍返回答案与 nil 错误。
- 修复前：`go test -race -count=1 ./cmd/nano-harness -run '^TestComposition_CancelledPlanReviewCannotScheduleExit$'` 失败，真实 composition 写成功审查结果，下一 turn 去掉规划段落并提交 `active:false`。测试以 channel/取消屏障构造顺序，从磁盘 transcript 和 loopback provider 请求验证结果。
- 修复前：`go test -race -count=1 ./internal/adapter/session/jsonl -run '^TestSessionV2Todo_RejectsChangedContract/wrong-tool$'` 失败，固定样本只把 `todo_write` 改成 `read`，decoder 返回 nil。`go test -race -count=1 ./internal/adapter/session/jsonl -run '^TestSessionV2_RejectsChangedContract/decision-call-reference$'` 同样失败，额外 call 引用被接受。
- 修复后：`go test -race -count=1 ./internal/adapter/spill ./internal/app/question ./internal/app/plan ./internal/adapter/tool/plan ./internal/adapter/session/jsonl ./internal/core/session ./internal/app/agent ./cmd/nano-harness` 通过。另有状态锁前取消退出、非法 todo 追加磁盘不变的永久测试。
- 首次 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 在既有 `TestService_LimitsLiveJobsPerOwner` 失败，`stopping job still counts: <nil>`：producer 可以在下一次 Launch 前结算，fixture 没有固定断言的状态。增加结算屏障后 `go test -race -count=1 ./internal/app/job -run '^TestService_LimitsLiveJobsPerOwner$'` 通过；产品 job 逻辑不变。
- ADR 归属调整后重跑 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`，退出码 0：全仓 race、架构、submodule、Agent Note、skills、workflow-tools、lint（0 issues）、逐产品文件 100.0% coverage、29 项 mutation 全部 killed，以及真实 cmd build/version 均通过。环境为 Go 1.27.0、macOS/arm64、golangci-lint 2.12.2；无需 lint 锁重试。
- `make tui-e2e` 退出码 0：真实二进制、PTY 和 macOS sandbox，19 次 root 工具调用、提问答案、规划审查、todo、后台通知、目标轮次、spawn/fork、approval、文件、打断、恢复和 cleanup 均通过。
- `scripts/change-scope.sh d1f31e3f6cfbefba4df225132f24491669abc389` 核对专用 worktree 的完整范围；`git diff --check` 通过，受影响 Markdown 的相对文件链接均指向现有文件，参考 submodule 干净。
- 未执行 live provider、其他 OS 原生验证或 `make ci` 的漏洞/发布矩阵：此次没有改动 provider wire、依赖或发布配置。

# 目标失败停止、输出截断事实与结算授权保护

- Status: implemented
- Date: 2026-10-06

## Context

目标 driver 忽略非准入拒绝的 TurnResult 错误，开场写入失败没有 `turn/end` 且不增加轮次，持续重排可绕过上限。engine 忽略 Completion.Stop，将 Anthropic/OpenRouter 截断当成完成；Responses 把 token 上限 incomplete 当成通用错误。Settle 读取旧结局后释放锁，失败 Pause 和错误分支无条件 disarm，会撤销后来的 clear/create 或 pause/resume。

上游只读参考 `5badb15009ae` 的 `goal-round-driver` 在 agent/error、durability checkpoint 失败和 max-tokens 时解除 armed。约束：engine 的工具 step 后 drain/notices 与另一项取消修复共享文件，改动仅位于 assistant message 提交后的局部分支。此 Note 补充[长期目标初始实现](2026-10-05-long-running-goals.md)，不取代其工具、权限、提示与生命周期证据。

## Decision

[compaction 摘要截断修补](2026-10-06-compaction-truncated-summary.md)拥有摘要消费方的停止检查；本 Note 继续拥有 provider 归一、engine 和 goal 停止结局的实施证据。

长期契约见 [ADR-0018](../../../docs/decisions/0018-goal-stop-outcomes.md)，[ADR-0016](../../../docs/decisions/0016-long-running-goals.md) 同步其 driver 规则。三个 provider 将输出上限归一为 StopMaxTokens，engine 持久化 max_tokens 结局，不执行截断工具提案或消费待投递通知。JSONL 严格校验形状与闭合 assistant step，固定 goal 样本包含该结局与未知停止枚举反例。

armed 表保存 GoalRef；成功提交后更新，Settle 和失败的旧轮次只按确切 ID/revision 解除，目标轮次使用开场归属（见[轮次停止归属修复](2026-10-06-goal-round-stop-ownership.md)），接管与关闭仍无条件解除。开场失败无需补写日志也能停止推进。composition 以 goal-tools-v2 绑定停止语义，旧组合恢复被拒且原始数据保持不变。插件及 Scope 回收顺序沿原 composition；未增加 goroutine、注册或持久化文件。

## Consequences

持续存储失败不会重排同一轮；输出截断从权威日志重建并阻止继续调用；后来的人类授权得以保留。截断工具提案不执行，用户需显式 resume；旧 composition 保留原文件但不能直接恢复。逐文件 coverage、真实 composition 与定向 mutation 分别验证语句、链路和断言有效性。

## Verification

- 修复前：`go test -count=1 ./internal/app/goal -run 'TestDriver_FailedOpeningDoesNotRequeueTheRound|TestService_SettlePreservesLaterHumanAuthorization'` 失败；开场失败仍 Armed:true，pause-resume 的 revision 3 与 clear-create 的新 ID 均 Armed:false。交错由 Journal 查询 channel barrier 控制，不依赖 sleep。
- 修复前：`go test -count=1 ./internal/adapter/model/provider ./internal/app/agent -run 'TestProvider_OutputLimitMapsToMaxTokens|TestEngine_OutputLimitStopsBeforeToolsAndNotices'` 失败；Responses 返回 invalid_request、OpenRouter 保留 length，engine 收到截断后发出第二次模型请求。Anthropic 的 max_tokens 原始映射已经正确，问题在 engine。
- 修复前：`go test -count=1 ./internal/app/goal -run 'TestService_SettlePausesCancelledRoundsAndDisarmsOtherStops/output_limit'` 失败，截断结局留下 Armed:true。
- `go test -race -count=1 ./internal/app/goal ./internal/app/agent ./internal/adapter/model/provider ./internal/adapter/session/jsonl ./internal/core/session ./cmd/nano-harness` 通过；真实 composeApplication 对三个 provider 验证每次截断只一次调用、active/disarmed、人类 resume 后下一轮及磁盘 max_tokens。
- `make coverage` 通过：原始 profile 无未执行语句，每个产品源文件与函数 100.0%。
- 首次完整 `make check` 在既有 `TestService_LimitsLiveJobsPerOwner` 失败（stopping job still counts: <nil>）：Kill 只请求取消，producer 可以在下一次 Launch 前结算，测试缺少 stopping 状态观察屏障。rebase 到 `e0cd284` 后保留 `f64c2dd` 已合入的 producer 结算屏障，删除本变更重复的 gate 扩展；断言名额仍占用后才释放，失败 cleanup 同样释放，不修改 job 产品代码。
- rebase 后 `make tui-e2e` 通过：真实 binary/PTY、19 次 root 工具调用、目标轮次完成、interrupt/resume、子代理、approval 与 cleanup。
- 新 outcome 的子代理 consumer 同步：前台工具保留 `max_tokens` 原因与 partial output，后台通知与 job 报告异常结算；`TestDelegationTools_ReportFailuresAndUnfinishedRuns` 在补齐前台 outcome switch 前失败（错误只有 partial output，没有停止原因）。
- `go test -race -count=20 ./internal/app/job -run '^TestService_LimitsLiveJobsPerOwner$'` 通过，producer 的取消后结算由屏障控制。
- rebase 到 `e0cd284` 后 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：race tests、架构、submodule、Agent Notes、skills、工作流反例、lint（0 issues）、逐文件 100% coverage、33 个 mutation 全部 killed，以及真实 cmd 构建与 version smoke。合并后的 mutation ID 无重复且每个替换位置唯一；四个 goal 用例与集成分支新增的五个用例均保留。
- ADR 使用独立编号 0018，标题与所有 goal 停止语义引用同步；0017 保留给图片附件存储，旧 goal ADR 文件名无残留引用。`AGENT_NOTE_BASE_REF=e0cd284 make agent-notes` 通过，范围相对该集成提交只有一个修复 commit；job fixture 与集成分支完全一致。
- `go test -race -count=10 ./internal/app/goal -run 'TestDriver_FailedOpeningDoesNotRequeueTheRound|TestDriver_FailedOldRoundPreservesNewAuthorization|TestService_SettlePreservesLaterHumanAuthorization'` 通过；最终范围审计确认 provider、engine、session、goal 和子代理 consumer 同步，engine.go 仅新增 9 行局部处理。`git diff --check` 通过，参考 submodule 无改动。
- 未使用真实远端账户、未跑跨平台 CI 或发布矩阵；全部 provider 证据来自 loopback 真实协议。

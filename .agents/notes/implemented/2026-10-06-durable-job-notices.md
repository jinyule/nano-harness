# 后台任务完成通知持久化

- Status: implemented
- Date: 2026-10-06

## Context

这是[工具对齐计划](2026-10-04-upstream-tool-parity.md)的 WP16。[ADR-0009](../../../docs/decisions/0009-background-jobs.md) 原先让待投递的完成通知只存在内存里，进程退出就丢弃。上游能力对齐审计（`_coord/reports/codex-audit3-report.md`“已记录的有意偏离”）指出理由不成立：已经完成、只是还没送达的通知仍是有效的完成事实。

调查了参考提交 `5badb15009ae` 的 `packages/core/agent-loop/src/inbox.ts`、`agent.ts`、`packages/core/agent/src/consumed-work.ts` 和 `packages/jobs/tool-jobs`：上游 inbox 把每次入队、认领和丢弃写成 `agent/inbox/spliced`，投影从这些事件折叠；认领后的消息作为 step 的 `user/message` 提交；恢复后待投递项仍在 inbox 中，只有下一次唤醒才开启 turn。`tool-jobs` 对空闲 owner `followup`、对忙碌 owner `inject`，被 wait 收走、模型自己 kill 和 teardown 的 settle 不入队。

本仓的 followup、steer、agent 消息、子代理结算和目标收尾仍是内存 `Notify`，范围只限 job 完成通知。非目标：可恢复的 job 执行、其他通知来源的持久化、通知的 UI 管理。

## Decision

长期契约在 [ADR-0023](../../../docs/decisions/0023-durable-job-notices.md)，ADR-0009 中被取代的条目已改写并在状态行注明；当前事实归[架构](../../../docs/architecture.md#事件持久化与-replay)、[安全](../../../docs/security.md#session-与恢复)和[测试](../../../docs/testing.md#持久化固定样本)文档。本次实施：

- `core/session` 新增记录 `notice/queued`（turn 与 step 为 0，只有 `message`）和来源字段 `notice_id`。`notice.go` 提供形状校验、`NoticeID`（按整个日志已入队数编号，fork 继承的编号之后继续）和 `PendingNotices`（入队而无同 ID 投递的消息，按入队顺序）。只有用户消息可以携带 `notice_id`。surface 与 TUI 把 `notice/queued` 当作非可见元数据，投递时 TUI 照旧显示 `job> `。
- JSONL order validator 记录已入队的 ID 和内容：重复入队、投递未入队或已投递的 ID、投递内容与入队不同都按损坏会话拒绝；`notice/queued` 可以出现在任意位置。
- agent 新增 `Agent.QueueNotice` 与 `Registry.QueueNotice`：先在 agent 锁内确认能接收（活动中，one-shot 只在其唯一 turn 运行时），再在通知锁内读日志、分配 ID、提交 `notice/queued`，最后入队；提交失败不入队也不消耗 ID。投递复用现有队列、边界和取消语义，engine 追加的那条带 `notice_id` 的 `user/message` 就是送达事实。`validUserMessage` 拒绝自带 `notice_id` 的直接输入、steer 和内存通知。
- `Registry.Create` 恢复会话时把 `session.PendingNotices(session.OwnEvents(events))` 放回队列，不唤醒：与上游一致，由下一个 turn 投递；fork 子代理因此不欠父会话的通知。
- job 服务的 `Notifier` 改为 `QueueNotice(ctx, sessionID, message)`，settle 以不继承取消的 context 调用；何时通知的规则不变。
- composition token `job-tools-v1` 升为 `job-tools-v2`，session 格式号保持 v2。

## Consequences

重启或崩溃不再丢失已完成但未送达的 job 结果，模型在下一个 turn 得到它，且只得到一次；日志可以独立回答哪些通知已送达、哪些还欠着。

代价与风险：

- 每个完成通知多一条记录、一次日志读取和一次 `fsync`。
- 恢复后的通知描述一个已不存在的 job，`job_output` 对旧 ID 返回 `unknown job`，或在新进程复用编号后指向新 job；通知文本本身仍准确。
- 新记录类型扩大了格式面；旧会话按 composition mismatch 拒绝恢复（尚无发布 tag）。
- 只有 job 通知持久化，其他通知通道仍在内存中；若以后统一为 durable inbox，需要新 ADR。

## Verification

- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0，lint 0 issues，逐文件 100.0% coverage，mutation 清单全部 killed，build 通过。
- `make tui-e2e`：真实二进制和 PTY 通过（19 次根工具调用，含后台 job 通知、interrupt 与 resume）。
- `TestComposition_OwedJobNoticeSurvivesRestart`：真实 composition、真实 JSONL 和真实 host 进程。job 在第二个 step 运行时完成，通知提交后打断该 turn 并关闭进程；用同一会话重启，恢复本身不发模型请求，下一个 turn 把通知交给 provider；再重启一次，磁盘 transcript 中 `notice-1` 只入队一次、投递一次，不再欠着。去掉 `Registry.Create` 的恢复后，该测试以 “the provider did not receive the owed notice” 失败。
- `TestSessionV2Notice_FrozenContract` 与 `TestSessionV2Notice_RejectsChangedContract` 冻结 `testdata/session-v2-notice.jsonl`（step 内入队、同 turn 投递、turn 之后仍欠着）并拒绝变体；关闭新的 order 规则后，重复 ID、投递未欠的 ID 和内容不同三个负例以及 order、resume 用例都失败。`TestLog_OwedNoticeSurvivesInterruptedRepair` 证明修复中断尾部后通知仍欠着。
- agent 包：`TestAgent_QueueNoticeCommitsBeforeDelivering`（提交时队列为空、投递携带同一 ID）、`TestAgent_QueueNoticeFailuresCommitNothing`（空闲 one-shot、未运行、读日志和提交失败都不入队，下一条仍是 `notice-1`）、`TestAgent_OwedNoticeIsDeliveredOnceAfterRestart`（打断后关闭、恢复不开 turn、下一个 turn 投递一次、再恢复不重复）、`TestAgent_DurableNoticeAtLastStepWakesTheNextTurn` 和 `TestAgent_ForkOwesNoneOfItsParentsNotices`（子代理不欠、编号从 `notice-2` 继续）。
- 未验证：Linux 上的真实运行；没有 live provider 调用。

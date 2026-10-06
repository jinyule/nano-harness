# ADR-0023：后台任务完成通知持久化

- 状态：Accepted
- 日期：2026-10-06
- 决策者：nano-harness maintainers

## 背景

[ADR-0009](0009-background-jobs.md) 原先规定，待投递的完成通知和 followup、steer 一样只在内存中，进程退出就丢弃，理由是“恢复后 job 已不存在”。上游能力对齐审计指出这个理由不成立：job 已经完成、只是还没送达的通知仍然是有效的完成事实，丢失它会让模型永远不知道一个它启动的命令已经结束、结果如何。

参考提交 `5badb15009ae` 的上游 agent 用 durable inbox 承载全部待投递输入（`packages/core/agent-loop/src/inbox.ts`）：

- 每次入队、认领和丢弃都写一条 `agent/inbox/spliced`；入队记录完整的 `UserMessage`，`MessageId` 是唯一身份。
- step 边界的认领是不带 `canceled` 的删除 splice，随后被认领的消息作为该 step 的 `user/message` 提交；取消产生带 `outcome: canceled` 的丢弃 splice。
- inbox 是从这些事件折叠出的投影，恢复后待投递项仍在 inbox 中，等下一次唤醒（followup/steer）开启的 turn 认领；恢复本身不开启 turn。
- `tool-jobs` 对空闲 owner 用 `followup` 入队并唤醒，对忙碌 owner 用 `inject` 入队；wait 收走、模型自己 kill 和 teardown 的 settle 不入队。

本仓的 followup、steer、agent 消息和子代理结算通知仍是内存队列，本 ADR 只处理后台任务完成通知。非目标：持久化其他通知来源、可恢复的 job 执行、通知的 UI 管理。

## 决策

### 记录

新增 session 记录 `notice/queued`，并给消息来源增加 `notice_id`：

```json
{"type":"notice/queued","message":{"role":"user","content":[{"type":"text","text":"background job bash-1 (bash: make) finished [status: completed, exit code: 0]. Read its output with job_output."}],"source":{"kind":"tool-jobs","notice_id":"notice-1"}}}
```

- `notice/queued` 是会话级事实：`turn` 与 `step` 为 0，只有 `message` 字段，消息必须是 role 为 user、带非空 `notice_id` 的完整用户消息。它可以出现在日志任意位置，包括活动 step 内部和两个 turn 之间，不进入模型 surface。
- 投递事实是 `user/message`，内容与入队消息完全相同（包括 `notice_id`）。只有用户消息可以携带 `notice_id`；直接输入、steer 和非持久化通知不能自带它。
- `notice_id` 在整个日志内唯一，格式为 `notice-<n>`，`n` 是该日志已有 `notice/queued` 数加一。fork 子代理继承父日志的闭合前缀，因此从继承的数量之后继续编号，不会与继承的 ID 冲突。
- 严格校验：未知字段、`notice/queued` 带 turn/step、缺少 ID、重复入队同一 ID、投递从未入队或已投递的 ID、投递内容与入队内容不同，都按损坏会话拒绝。

session 格式号保持 v2。composition ID 中的 `job-tools-v1` 升为 `job-tools-v2`，没有这类记录的旧会话按 composition mismatch 拒绝恢复；本仓尚无发布 tag，没有已发布的用户会话需要迁移。

### 写入与投递

- job settle 时，若通知规则（[ADR-0009](0009-background-jobs.md#完成通知)：没有 wait 收走、不是 kill、不是 teardown）要求通知，job 服务调用 `Notifier.QueueNotice`。agent `Registry` 实现它：找到 owner 的 live agent，调用 `Agent.QueueNotice`。
- `Agent.QueueNotice` 先确认 agent 能接收通知（活动中；one-shot agent 只在它唯一的 turn 运行时接收），再在 agent 的通知锁内读取日志、分配下一个 ID、提交 `notice/queued`，最后才放入内存队列。提交失败时不入队、不消耗 ID。先持久化事实、再更新投影。
- 投递沿用现有队列语义：忙碌 agent 在下一个边界追加，空闲 agent 被唤醒开启 turn；最后一个允许的 step、`max_tokens` 截断和被取消的 turn 都不取通知。投递就是 engine 追加的那条带 `notice_id` 的 `user/message`，已取出的通知以不继承取消的 context 提交。
- job 服务以不继承取消的 context 提交通知，所以 settle 与 shutdown 竞态时通知仍然写入。job 服务渲染的通知总在单个文本块上限内（截断规则见 [ADR-0009](0009-background-jobs.md#完成通知)），`QueueNotice` 剩下的失败都来自 owner：owner 不再 live、one-shot owner 已不在唯一的 turn 内，或者通知无法写入日志。前两种情况与 teardown 相同，通知没有 reader。job 服务不区分这些情况，失败一律以 `completion notice not delivered: <错误>` 并入该 job 的 detail，`job_output` 与 `job_kill` 的状态行会显示它；仍能读取的 owner 因此可以发现日志写入失败，而不是静默丢失完成事实。该诊断不写入日志，也不重试。

### 恢复

- `Registry.Create` 恢复会话时，从 session 自己的事件（`session.OwnEvents`，不含 fork 继承的前缀）折叠出仍欠着的通知：有 `notice/queued` 而没有对应投递的消息，按入队顺序放入 agent 的内存队列。
- 与上游 durable inbox 相同，恢复本身不开启 turn；下一个 turn（用户输入或新通知唤醒）在开始后投递它们。被中断的 turn 留下的通知因此在重启后仍然欠着。
- 去重：同一 ID 只能投递一次，内存队列取出即离开，日志中的投递事实使恢复后不再折叠出它。进程在提交投递之前退出时，通知仍欠着，恢复后再投递一次；投递已提交则不会重复。
- resume 修复（补写中断的 approval、tool result、step/turn 结尾）不触碰 `notice/queued`，也不为欠着的通知补写任何事实。

### fork、compaction 与 subagent

- fork 子代理继承父日志到最后一个 turn/end 的前缀，其中可能有父会话欠着的通知。子代理只从自己的事件折叠欠着的通知，因此不继承它们，与 [ADR-0013](0013-background-continuable-subagents.md) 的 owner 规则一致：job 和通知属于启动它的 session。
- compaction 只替换 surface，不删除原始事件；欠着与否由原始事件决定，已投递的通知像普通用户消息一样参与摘要。
- 只有 job 完成通知持久化。agent 之间的消息、子代理结算通知和目标收尾指令仍走内存 `Notify`。

## 后果

重启或崩溃不再丢失已经完成但未送达的 job 结果，模型在下一个 turn 得到它，且只得到一次。日志可以独立回答“哪些通知已送达、哪些还欠着”。

代价与风险：

- 每个完成通知多一条记录、一次日志读取和一次 `fsync`。
- 恢复后的通知描述一个已不存在的 job：`job_output` 对旧 ID 返回 `unknown job`，或在新进程复用同一编号后指向新 job（与上游重置编号的共同限制）；通知文本本身仍然准确记录了完成状态。
- 新的记录类型扩大了格式面，所有 provider、validator 和工具需要同步维护。

## 被否决方案

- 保持内存队列：已完成的结果在重启时静默丢失，审计认定理由不足。
- 照搬上游通用 inbox（持久化所有入队、认领和丢弃）：需要同时改变 followup、steer、agent 消息和子代理结算的语义，并为认领和取消增加记录；本仓这些通道的语义已由 [ADR-0013](0013-background-continuable-subagents.md) 等确定，超出本次范围。只持久化 job 完成事实，以投递的 `user/message` 作为送达事实，已能回答“欠着什么”。
- 恢复时立即开启 turn 投递：会在用户没有输入时发起模型调用；上游 durable inbox 也只在下一次唤醒时投递。
- 用 job ID 作为通知 ID：job 编号随进程重置，同一会话内会重复。

## 复审触发条件

- 决定把 followup、steer 或 agent 消息也做成 durable inbox。
- 需要可恢复的 job 执行，或恢复后旧 job ID 需要有明确含义。
- 需要撤销或过期未送达的通知。
- 参考指针更新改变了上游 inbox 或 tool-jobs 的投递语义。

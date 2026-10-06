# 唤醒 turn 开场的取消与通知归还

- Status: implemented
- Date: 2026-10-06

## Context

第三轮增量审查（`_coord/reports/opus-review3-agent.md`）在 agent 唤醒 turn 的开场阶段发现三个缺陷和一个待确认项，与 [ADR-0023](../../../docs/decisions/0023-durable-job-notices.md) 的持久化通知直接相关：

- A1：engine 的开场回调用可取消的 ctx 依次追加 `turn/start` 和开场 `user/message`，关闭 turn 的 defer 在开场返回之后才注册。打断落在两条记录之间时，日志停在没有 `turn/end` 的 `turn/start`，之后每个 turn 的 `turn/start N+1` 都被 JSONL order validator 拒绝，直到重启修复。
- A2：`claimWake` 先把最早的通知移出内存队列作为开场消息。开场在提交 `turn/start` 之前被取消时什么都没有写入，持久化通知在本进程内不再投递，内存通知（agent 消息、子代理结算、目标收尾）永久丢失，违反“被取消的 turn 不取通知”和“下一个 turn 得到它”。
- A3：`QueueNotice` 先在 agent 锁内判定、释放锁后提交 `notice/queued`、入队时再判定一次；one-shot 的唯一 turn 若恰在提交期间结束，日志会留下一条永远不会投递的通知，调用方收到 `ErrInvalidConfig`。
- R3（待确认）：worker 在 `claimWake` 与 `select` 之间时，空闲 `Notify` 置位 woken，而 `select` 选中了同时到达的 turn 请求，woken 在整个 turn 中保持为真；该 turn 被打断时，中断前排队的通知被当作“中断后唤醒”立即开启新 turn。

上游 agent-loop 在 `turn/start` 之后用 try/finally 保证 `turn/end`，inbox 输入在 turn 内的 pre-step 才被认领，认领前中止则输入仍留在 inbox。

## Decision

- A1：关闭 turn 的 defer 移到开场之前注册，每个已提交的 `turn/start` 都有 `turn/end`，包括开场自身 I/O 失败的情况（违反契约、开场后才报 stale 的 admission 也被闭合）。开场只在提交 `turn/start` 之前检查一次取消；一旦开始，`turn/start` 与开场 `user/message` 用不继承取消的 context 一起提交，取消由紧随其后的边界观察，turn 以 `canceled` 结束。
- A2：`TurnResult` 增加未导出字段 `opened`，表示 `turn/start` 已提交。唤醒 turn 以 `canceled` 结束且未开场时，worker 把开场通知放回队首；`finishTurn` 不会因此立即重开（被取消的 turn 只重放取消之后的唤醒），通知由下一个 turn 投递一次。admission 拒绝和其他失败不归还，避免持续失败时反复开 turn。
- R3：能用测试接缝确定性复现。`turn()` 标记 busy 时同时清除 woken：turn 开头会取走全部通知，此前的唤醒请求已被满足；因取消没能取走的通知按契约等下一个 turn。新增包级测试接缝 `beforeAgentWait`，在 worker 进入 `select` 之前调用，生产实现为空函数。
- step 开场：step context provider（例如 K2 的 runtime-context）在已开场的 turn 内、`step/start` 之前提交消息，已由关闭 turn 的 defer 覆盖，消息也已用不继承取消的 context 提交；但打断若落在消息提交之后，`step/start` 以已取消的 ctx 追加失败，turn 被记为 `error`。现在 step context 之后先检查取消，`step/start` 的追加失败也按 `outcomeFor` 分类，turn 以 `canceled` 结束。
- A3：选择如实记录而不改代码。即使让判定在提交期间保持不变，在最后一个边界之后入队的通知对 one-shot 同样永远不会投递；one-shot 不开启新 turn，这些欠账没有 reader，root 和 continuable 会话在这个窗口里唯一的拒绝原因是 agent 已停止，由恢复重放兜底。ADR-0023 与 `QueueNotice` 文档写明这一点。
- 同步 ADR-0023 的“写入与投递”和 architecture.md 中 agent loop 与 `Notify` 的表述，不新建 ADR。没有修改 subagent 包。

## Consequences

被打断的唤醒 turn 不再卡死会话，开场通知在本进程内不会丢失或重复：开场前取消时等下一个 turn，开场后取消时已随开场提交、模型在下一个 turn 的 surface 中看到它。

代价与风险：

- 开场一旦开始就不再响应取消，最多多提交两条记录、一次 `fsync`。
- 开场后取消的 turn 中，开场通知算作已投递，但该 turn 没有回应它；模型在下一个 turn 才会看到它。
- one-shot 仍可能留下不会投递的欠账，只在文档中说明。
- `beforeAgentWait` 是生产代码中的测试接缝（与已有的 `beforeAgentDrain` 同类）。

## Verification

- 修复前失败（同一套永久测试，在未修改的 `agent.go`/`engine.go` 上运行）：
  - `TestAgent_InterruptWhileOpeningCommitsTheWholeOpening`：`turns left open: [1]`。
  - `TestAgent_WokenTurnCancelledBeforeOpeningKeepsItsNotice` 的 durable 与 in-memory 两个子测试：下一个 turn 的用户消息只有 `["1:next"]`，通知丢失。
  - `TestAgent_StaleWakeDoesNotReopenAfterInterrupt`：`["1:block" "1:idle notice" "2:before interrupt"]`，中断前的通知开启了第 2 个 turn。
  - `TestAgent_InterruptAtStepContextEndsTheTurnCanceled`：在 step context 消息提交后打断，turn 结局为 `Outcome:error`。
- 修复后四项通过；`TestEngine_RejectedAdmissionCommitsNothing` 的 “stale after open” 期望从 2 条记录改为 3 条（turn 被闭合）。
- 审查者在 `/tmp/nh-review3` 的真实 JSONL 复现（临时复制到本 worktree 运行后删除）：A1、A2、R3 三项通过；A3 用例仍按记录的窗口失败。
- `go test -race -count=3 ./internal/app/agent/` 通过，agent 包 coverage 100.0%；`go test -race -count=1 ./...` 通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0，lint 0 issues，逐文件 100.0% coverage，mutation 清单全部 killed，build 通过。
- `make tui-e2e`：真实二进制和 PTY 通过（19 次根工具调用，含后台 job 通知、interrupt 与 resume）。
- 未验证：subagent watcher 在开场被取消时的驻留行为（审查报告的延伸项）没有单独复现；没有 live provider 调用。

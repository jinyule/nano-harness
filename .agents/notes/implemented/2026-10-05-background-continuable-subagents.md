# 子代理工具族对齐上游：后台可继续子代理与双向消息

- Status: implemented
- Date: 2026-10-05

## Context

这是[工具对齐计划](2026-10-04-upstream-tool-parity.md)的 WP7。本仓原有五个自有工具 `spawn_subagent`、`subagent_followup`、`subagent_interrupt`、`subagent_report`、`list_subagents`：spawn 总是等待 child 首轮报告，followup 同步等待，fork 是包含进行中 turn 的 128 KiB 文本快照。上游 Base 组合（参考提交 `5badb15009ae`）提供 `subagent`（spawn、`backgroundMode: continuable`）、`subagent_fork`（fork、`backgroundMode: one-shot`）、`send_message`、`interrupt_agent` 与 `list_agents`。

调查了上游 `packages/subagent/{subagent,subagent-spawn-in-process,subagent-fork-in-process,subagent-in-process-driver,tool-subagent,tool-subagent-control}`、Base 组合、`docs/tool-catalog.md`，以及 fork 保持 one-shot 与按调用选择模型的两份上游 Note。要点：continuable child 驻留期间接收消息，空闲、收件箱为空且没有 continuable 子代理时结算并释放 activation，parent 收到 runtime 拥有的结算通知；消息只跨越直接父子边；`interrupt_agent` 可指向任一后代且不等待；列表读取 parent 自己的目录记录；fork 以 parent 最后一个 `turn/end` 为止的会话为种子；上游默认深度 1、池上限 8。

WP3 的 [ADR-0009](../../../docs/decisions/0009-background-jobs.md) 要求引入可在运行期间关闭的 owner 时同时实现 job 的 owner 释放。

非目标：子代理写文件或请求审批、按调用选择模型、persona/tool filter 参数、外部进程子代理、跨进程 mailbox。

后续正确性修补见 [subagent 正确性 Note](2026-10-06-subagent-correctness.md)：它拥有池名额转交、取消截止线、中断后的新唤醒、清理错误优先级、job 启动准入、closing output 与原样参数保存；本 Note 继续拥有工具迁移、目录、fork 与 owner 释放的实施证据。

重叠审计：[核心 harness Note](2026-08-24-core-agent-harness.md) 中对 subagent 工具与 fork 快照的描述被本 Note 部分取代，其余部分仍有效，两者保留并互相链接；[上游工具定义 Note](2026-10-04-upstream-tool-definitions.md) 记录的 `subagent-tools-v2` 迁移已被 v3 取代，该 Note 其余内容不受影响。

## Decision

长期契约见 [ADR-0013](../../../docs/decisions/0013-background-continuable-subagents.md)，当前事实归[架构](../../../docs/architecture.md#subagent)、[安全](../../../docs/security.md#subagent-与生命周期)与[测试](../../../docs/testing.md#agent-与工具证据)文档。本次实施：

- `internal/adapter/tool/subagent`（插件 `subagent-tools`）注册与上游 Base 逐字节一致的五个工具，旧工具移除。`subagent`、`subagent_fork` 并发安全，三个控制工具 exclusive；`subagent` 以 `appTool.OrderSubagent = 2800` 贡献上游 `tool:subagent` guidance。
- `internal/app/subagent.Service` 重写。新 API（WP10 可复用）：

  ```go
  func New(registry *agent.Registry, jobs *job.Service, repository transcript.Repository) (*Service, error)
  func (*Service) StartContinuable(ctx, StartRequest) (string, error)   // 后台 continuable，立即返回 child id
  func (*Service) Run(ctx, StartRequest) (Report, error)                // 前台 one-shot，返回 Outcome 与最终回答
  func (*Service) StartBackground(ctx, StartRequest) (string, error)    // one-shot 作为 kind "subagent" 的 job，返回 job id
  func (*Service) SendMessage(ctx, senderID, targetID, text string) error
  func (*Service) Interrupt(callerID, targetID string) error
  func (*Service) ListChildren(ctx, parentID string) ([]Entry, error)
  func (*Service) ListDescendants(ctx, rootID string) ([]Entry, error)
  func (*Service) List(parentID string) ([]Info, error)                 // TUI /agents 的 live 快照
  type StartRequest struct { ParentID string; Journal Journal; Turn, Step uint64; Description, Prompt string; Fork bool }
  ```

  失败为 `*subagent.Error{Code, Message}`，`Code` 取 `INVALID_REQUEST`、`DEPTH_LIMIT`、`ACTIVATION_LIMIT_REACHED`、`UNAUTHORIZED`、`NOT_RESUMABLE`、`PARENT_UNAVAILABLE`，`Message` 直接作为模型可见文本。服务生命周期错误为 `ErrNotRunning`。
- 驻留与结算：每个 continuable child 有一个结算 watcher（服务 `WaitGroup` 所有）。watcher 在 child 空闲后于服务锁内比较投递代数并检查 continuable 子代理：期间有新投递则重来，有子代理则等待 wake，否则标记 closing、读取本次驻留的最后 outcome 与最终回答、释放 child，并在移除句柄的同一临界区通知 parent。冷恢复用一个 closing 占位句柄持有池名额并让并发投递等待。effect/cleanup：`Start` 登记 `stop`，后者拒绝新工作、取消 watcher 并等待，再从最深处释放全部 child。
- 释放顺序：中断 child、深度优先释放 live 子代理、`registry.Close`、`job.Service.Release(ctx, child)`。
- job 清理与通知语义：新增 `job.Service.Release(ctx, owner)`，取消 owner 的 live job、等待全部 settle、删除该 owner 的全部记录，这些 settle 不发通知；服务已停止时返回 nil，ctx 先结束时返回其错误，剩余 job settle 后仍列出直到服务停止。后台 one-shot job 的通知仍由 `app/job` 按 ADR-0009 发给 parent（`background job subagent-N (subagent: <label>) finished ...`），值结果是 child 的最终回答。
- 消息与通知经现有 `Agent.Notify`：`agent-message`（`Agent <id> sent a message: ` + 正文）与 `subagent-settled`（上游结算文案）两种 source kind；continuable 首条任务追加上游的返回指引。TUI 把两者显示为 `agent> `。
- 持久化：descriptor v2（`spawn`/`fork` provider、`inherited` 前缀长度，紧跟继承前缀且不在 turn 内）、parent 在创建 step 内写的 `subagent/catalog`、fork 种子经 `transcript.OpenOptions.Seed` 与 header 一次写入并整体校验。`core/session` 新增 `OwnEvents`、`Children`、`FinalAssistantText`、`LastOutcome` 纯投影。`agent.CreateRequest` 增加 `Provider` 与 `Seed`，root 不得携带。固定样本 `internal/adapter/session/jsonl/testdata/session-v2-subagent.jsonl`。
- composition：构造顺序为 jobs 先于 subagents（subagents 依赖 jobs 与 session manager），插件启动顺序不变；token 升为 `subagent-tools-v3`（合并后与 `fs-tools-v2`、`search-tools-v3`、`shell-tools-v3`、`spill-v1` 等并列）。两份工具目录 fixture 更新：完整目录仍为 20 个工具（旧五个换成新五个），upstream parity 数量从 15 增至 20。
- mutation 新增 `subagent-direct-parent` 与 `subagent-descendant-interrupt`；`validateSeed` 的序号比较改写，避免与既有 `session-sequence` 变异点重名。
- `scripts/tui-e2e.py` 的子会话场景改为前台 `subagent`（child 用 `read`）、前台 `subagent_fork`、`list_agents`、写给目录外 id 的 `send_message`（预期错误）与 `interrupt_agent`。

fork 种子复制 parent 的全部事件，包括 WP8 的 `plan/mode` 与 WP6 的 skill 上下文消息，因此 fork child 继承 parent 当时的规划模式；上游 plan 投影同样折叠 fork 继承的日志（“resume and fork restore the state”）。以后新增的会话状态投影若不应被 fork 继承，应只读 `session.OwnEvents`。

child 固定继承 route、委派说明改为 runtime context 与消息发送者身份由 [K2 Note](2026-10-06-subagent-route-context-sender.md) 记录。

与上游的差异：深度上限保持本仓既有的固定 4（上游设置默认 1），池上限固定 8；`interrupt_agent` 对 live one-shot 后代同样有效；目录读取失败只报告 `unavailable`；结算 watcher 不等待 child 的后台 job，结算时由 owner 释放结束它们，与上游一致。

## Consequences

模型看到与上游相同的委派工具，可以并行启动后台 child 并在结算时收到通知，parent 与 child 可中途交换信息，fork 获得完整的已完成会话。代价：fork 复制 parent 日志前缀的磁盘空间；结算后的消息需要冷恢复；旧会话按 composition mismatch 拒绝恢复（本仓尚无发布 tag）。

已知风险：只有中断前排队且没有后续唤醒的消息会继续占用驻留名额；后台任务通知与 child 结算并发时可能丢失；驻留状态与未提交消息只在内存中。当前取消与唤醒规则由 ADR-0013 和后续正确性 Note 维护。

与其他 WP 的冲突热点：`internal/app/agent/{registry,types}.go`（descriptor v2、`Provider`/`Seed`）、`internal/app/job/service.go`（`Release` 与 WP2 的 `Output.Advertise`/`Read.Spills` 并存）、`internal/app/tool/define.go`（`OrderSubagent`）、`internal/adapter/session/jsonl/jsonl.go`（种子写入）、`cmd/nano-harness/{application,main}.go` 与两份 fixture、`docs/testing.md` 的 mutation 数量、`scripts/tui-e2e.py`。WP2 的 `Spec.Check` 签名变化不影响本包（subagent 工具没有 `Check`）。

复杂度观察（`make quality BASE_REF=3b29e7a`，阈值 10，仅观察）：新代码中 `(*Service).deliver` 14、`create` 12、`watch`/`release`/`resume` 各 11，分支来自互斥的授权与驻留状态，已把冷恢复、占位与替换拆成独立函数；既有函数 `validateOrder` 升到 79（descriptor 与目录两条规则）、`(*Registry).Create` 28、`(*model).applyEvent` 28、`(*Manager).Open` 16。没有新增跨包重复候选。

## Verification

- `go test -race -count=15 ./internal/app/subagent/`：通过；`go test -race -count=10 -run TestComposition_SubagentsEndToEnd ./cmd/nano-harness/`：通过。
- `go test -race -count=1 ./...`：通过。
- `python3 scripts/mutation-check.py --manifest <新增两项>`：两项 killed；`session-sequence` 改写后 killed。
- rebase 到集成分支 `67c9f83`（WP8、WP6、WP2 已合入）后：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，逐产品文件 coverage 100.0%，17 个 mutation 全部 killed。
- `make tui-e2e`：PASS（rebase 后 16 次 root 工具调用，含 spawn/fork 子会话、`never` 策略与目录记录）。
- `make quality BASE_REF=3b29e7a`：生成报告，见上文观察。
- 偶发失败修复（2026-10-06，WP5 报告）：`TestComposition_SubagentsEndToEnd` 让模型在同一 step 并发调用 `subagent` 与 `subagent_fork`，两者都是并发安全工具，两条 `subagent/catalog` 的提交顺序取决于哪个 child 先创建完成；测试却假定第一条是 continuable 的 `worker`。复现：在 `05e4012` 上 `go test -race -c` 后以 `-test.count 100 -test.cpu 1,4,8` 运行，300 次中 39 次失败，全部位于该目录顺序断言。产品消费方不依赖顺序：冷恢复与授权按 id 查目录，`list_agents` 按目录提交顺序列出，这一顺序由日志确定，同一 step 内并发委派按完成顺序提交，已写入 `ListChildren` 的文档。修复只把测试改为按标签查找目录项并用 fork 的 id 读取其 transcript。审计：subagent 包测试的 child 都顺序创建；`scripts/tui-e2e.py` 的 `subagent` 与 `subagent_fork` 位于不同 step，目录顺序确定。修复后同一测试二进制以 `-test.count 1000` 分别在 `-test.cpu 1`、`4`、`8` 下运行，3000 次全部通过。
- 整体审查 S2/S5 修复（2026-10-06）：
  - S2：`send_message` 已向模型返回 `message delivered`，随后的 `interrupt_agent` 让 turn 以取消结束，消息留在 agent 内存；watcher 看到空闲即结算并关闭 child，消息丢失。修复只改 `app/subagent`：每个驻留 child 记录本次驻留由服务投递的消息与结算通知数（`delivered`），watcher 在最后一个 turn 以取消结束且日志中已提交的 `agent-message`/`subagent-settled` 少于该数时保持驻留，等待下一次投递开启的 turn 一并提交；其他结局下的差额意味着没有可等待的消息，不会卡住。`TestService_InterruptedChildKeepsDeliveredMessages` 在模型请求运行中投递再中断（取消发生在任何 step 边界之前，与引擎是否修复 B1 无关），用 `parked` 观察点与 `closeAgent` 钩子确定性地区分驻留与错误结算；在修复前的 `service.go` 上该测试稳定失败（`settled with an accepted message still queued`），修复后 `-race -count=30` 通过。取消发生在工具执行中时，消息能否保留取决于 WP3 的 B1（取消的 turn 不再取走通知）；B1 合入后同样适用本规则。
  - S5：one-shot child 的后台 job 若在其唯一 turn 越过最后一个通知边界之后完成，`Agent.Notify` 会把通知排队，`finishTurn` 随即开启第二个 turn，日志多出一个 turn，报告可能取到它的文本。修复在 `agent.go`（不涉及 `engine.go`）：one-shot agent 只在 turn 运行时接受通知，`finishTurn` 不为 one-shot 置 `woken`。`TestAgent_OneShotNeverOpensASecondTurnForLateNotices` 以 `maxSteps: 1` 与模型 barrier 构造迟到通知；修复前稳定失败（`one-shot ran 2 turns`），修复后 `-race -count=20` 通过。
- 未覆盖：真实 provider 的 live 子代理运行；跨进程重启后的冷恢复只由 registry 与服务测试中的关闭后重开证明，没有 PTY 重启场景。

# ADR-0013：后台可继续子代理、相邻消息与子代理目录

- 状态：Accepted
- 日期：2026-10-05
- 决策者：nano-harness maintainers

## 背景

这是[工具对齐计划](../../.agents/notes/implemented/2026-10-04-upstream-tool-parity.md)的 WP7。本仓原有 `spawn_subagent`、`subagent_followup`、`subagent_interrupt`、`subagent_report` 和 `list_subagents` 五个自有工具：`spawn_subagent` 总是等待 child 首轮报告，continuable child 也一样；followup 同样同步等待；fork 把 parent 当前 surface 压成一条最多 128 KiB 的文本消息，连同进行中的 turn 一起交给 child。

上游 Base 组合（参考提交 `5badb15009ae`，`packages/bundle/base/cordis.patch.yml`）提供 `subagent`（spawn provider、`backgroundMode: continuable`）、`subagent_fork`（fork provider、`backgroundMode: one-shot`）、`send_message`、`interrupt_agent` 和 `list_agents`：continuable child 在后台运行，立即返回 id，结束时通知 parent；parent 与 resident child 之间可以双向发消息；fork child 以 parent 已完成 turn 的会话前缀为种子；列表读取 parent 自己的子代理目录记录，不打开 child 日志。

这些能力改变模型可见工具、核心循环的通知来源和持久化格式（descriptor 版本、新记录类型、以 parent 事件为种子的 child 日志），按根规则需要 ADR。WP3 的后台任务服务（[ADR-0009](0009-background-jobs.md)）要求引入可在运行期间关闭的 owner 时同时实现 owner 释放，本 ADR 一并决定。

非目标：子代理写文件或请求审批（delegated 策略仍固定为 `never`）、按调用选择模型（`list_subagent_models` 与 `provider`/`model`/`reasoning_effort` 参数）、persona 或 tool filter 参数、外部进程子代理、Agent Teams、跨进程共享 child 的 mailbox。

## 决策

### 1. 模型可见定义

五个工具的名称、描述、参数 schema、必填项、枚举与属性顺序和上游 Base 组合逐字节一致，由 `cmd/nano-harness/testdata/upstream-base-tools.json` 约束：

- `subagent`：spawn 措辞加 continuable 后台句（`It runs in the background by default and returns a subagent id you can continue with \`send_message\`; you are notified when the run settles.`），参数 `description`、`prompt`、`run_in_background`（`Defaults to true. ...`）。贡献 `tool:subagent` guidance：`Start independent subagent delegations together in one assistant message and continue useful work while they run.`，order 2800（上游 `TOOL_SUBAGENT`）。
- `subagent_fork`：继承会话措辞加 `This call waits for the result by default.`，`run_in_background` 使用 job 文案，没有 guidance。
- `send_message`、`interrupt_agent`、`list_agents`（`scope` 枚举 `children`/`descendants`）。

`subagent` 与 `subagent_fork` 声明并发安全，与上游一致；三个控制工具是 exclusive。结果文案沿用上游 render：`started subagent <id>`、`started background subagent job <id>`、前台返回 child 的最终文本；`message delivered to agent <id>`；`interrupt requested for agent <id>`；`list_agents` 每行 `<id> [running|inactive][ parent=<id> depth=<n>] — <label>`，读不到目录的条目为 `<id> [diagnostic: unavailable]...`，空列表为 `(no subagents)`。

description 原样进入 descriptor、catalog、列表与 job label，不去空白或截断；空字符串和空白字符串在前台及 continuable 委派中合法，prompt 同样允许为空或只有空白。后台 one-shot 的空 label 由 job 准入拒绝。description 最多 128 KiB，独立于完整工具参数对象的预算，为 job 通知的附加文案在 256 KiB 文本块内保留空间；prompt 最多 256 KiB，与单文本块上限一致。服务在创建 child 前明确拒绝超限值；持久化 decoder 同样拒绝超限 label。这一规则不增加记录字段；旧二进制仍可能拒绝含超过 128 字节或空 label 的新记录。

前台 run 未完成时返回错误：`subagent run was cancelled`（取消或中断恢复）、`subagent run failed`（错误）或 `subagent run ended abnormally (step_limit)`，有部分回答时追加 `\nPartial output before the run ended:\n<text>`。

> 后续停止契约：[ADR-0018](0018-goal-stop-outcomes.md) 将 `max_tokens` 纳入前台异常结束错误、后台结算通知和后台 job 的失败结局；此处保留原结果描述。

### 2. 生命周期

`internal/app/subagent.Service`（插件 `subagents`）拥有全部 child 句柄：

- **前台 one-shot**（`subagent` 且 `run_in_background: false`，或 `subagent_fork` 默认）：创建 child、提交任务、等待唯一一个 turn、读取最终回答、释放 child。调用被取消时释放 child 并返回取消错误。
- **后台 one-shot**（`subagent_fork` 且 `run_in_background: true`）：先准入 kind `subagent`、owner 为 parent 的 job，再在 job 拥有的取消信号下创建 child、复制 fork 种子并写 parent catalog。池满或 job 服务停止时没有 child 或 catalog 副作用。调用只等待启动阶段结束，使 catalog 在调用方活动工具 step 内提交；启动失败仍返回 job id，由 `job_output` 显示 `failed` 与错误 detail。job 值结果是 child 的最终回答；完成为 `completed`，取消为 `killed`，其他结局为 `failed`、detail 为 outcome 名称。清理失败优先于取消，记为 `failed` 且不提供成功值结果。`job_output`、`job_kill` 按 ADR-0009 读取与终止它。
- **后台 continuable**（`subagent` 默认）：创建 child、提交任务后立即返回 id。child 驻留（resident）期间接收消息；当它空闲、等待期间没有新投递、没有 continuable 子代理，且服务投递给它的消息都已写入日志时**结算**：服务关闭 child agent（status 变为 `inactive`），然后通知 parent。之后 parent 的 `send_message` 从 transcript 冷恢复它。

清理失败归类为 `ACTIVATION_TEARDOWN_FAILED`，保留底层原因且不阻止其他 cleanup。continuable 结算必须在清理之后决定通知：失败时通知 parent `failed before it finished.` 与 `It left no closing message.`，不附 child 的成功或部分输出；并发释放调用方得到同一清理错误。

释放 child 时先中断它，按深度优先释放它的 live 子代理，关闭 agent 与 transcript，再释放它拥有的 job。服务关闭与 one-shot parent 被回收时也按此顺序释放整棵子树，但这些拆除不是结算，不发通知。

每个 continuable 池最多 8 个驻留 child：根或 one-shot agent 的 continuable child 自成一池，continuable child 的 continuable 子代理共用其池。创建与冷恢复在重建 agent 前占位；创建的 reservation 与发布后的 resident handle 在同一临界区转交，整个创建过程只占一个名额，失败释放占位；超限立即返回 `subagent limit reached (active child limit: 8); wait for an existing child to finish or complete this work with the current agents`，不排队。one-shot child 不占池。绝对 delegation depth 上限仍为 4，超限返回 `subagent depth <n> exceeds maxDepth 4`。上游 Base 默认深度为 1、池上限可由设置修改；本仓保留既有的固定深度 4 和上游默认池上限 8，两者都不作为部署配置。

### 3. 消息与通知

`send_message` 只跨越一条直接父子边：

- 任何 agent 可以写给自己目录中的 continuable 直接 child；child 不驻留时冷恢复。目录中的 one-shot child、不在目录中的 id 和他人的 child 分别返回 `has no supported continuation state ...`、`is unavailable` 和 `belongs to another parent session`。
- 驻留的 continuable child 可以写给直接 parent；parent 不再 live 时返回 `direct parent is not live; the message was not delivered`。one-shot child 或非驻留 child 写给 parent 返回 `is not a resident continuable child and cannot send to parent ...`。

`send_message` 在调度之前和收件箱接受消息的锁内检查调用取消。前者返回 `ABORTED_BEFORE_DISPATCH`（`tool call aborted before dispatch`），后者返回 `ABORTED`（`tool call aborted`）；工具结果使用 `Error: ` 信封。被截止线拒绝的消息不入队、不写 recipient 日志，也不释放原有 resident child。

消息以 `user/message`（source kind `agent-message`，`sender_session_id` 为发送方会话）投递，内容为 `Agent <sender> sent a message: ` 加正文两个文本块，经 `Agent.NotifyContext`：接收方忙时在下一个 step 边界追加，空闲时开启新 turn。中断已生效、旧 turn 尚未退出时接受的新消息属于下一 turn，并保留唤醒请求；旧 turn 退出后自动处理，旧 turn 不消费这些待投递消息。中断前已排队的消息不单独唤醒，但可随该新 turn 一起提交。one-shot 仍不因此开启第二个 turn。continuable child 的首条任务（source kind `delegation`）在正文后追加上游的返回指引，告诉它 parent id 并要求用 `send_message` 报告结果；one-shot 任务只有正文。

结算通知是 source kind `subagent-settled`、`sender_session_id` 为结算 child 的 `user/message`，以 `Background subagent <id> finished and will do no further work unless you send it more.`（取消或中断为 `was stopped before it finished.`，错误为 `failed before it finished.`，step 上限为 `ended abnormally (step_limit) before it finished.`）开头，后接 `Its closing message:` 与本次驻留的最终回答，没有回答时为 `It left no closing message.`。结局取本次驻留中最后一个 `turn/end`，回答先选择最后一条含内容块的 assistant message，再提取文本；本仓分开记录的 `tool/call` 同样属于该 assistant 消息的内容。最后消息只有工具提案时没有 closing text，不退回较早的进度文字。空 content 的 usage-only 消息不替换候选；没有任何带内容的 assistant 消息时才拼接流式文本。child 的移除和通知在同一临界区完成，continuable parent 不会在两者之间结算；等待该 child 释放的调用方最后才被唤醒，看到 child 已结束时通知已投递给 parent。

`interrupt_agent` 取消调用方任一 live 后代当前 turn 而不等待，不级联到目标的子代理；目标不 live 是被接受的空操作，自身或非后代返回 `UNAUTHORIZED` 类错误。上游只中断 continuable activation；本仓对 live one-shot 后代同样有效。

### 4. 持久化

session format 仍为 v2，变化都在记录层：

1. **`subagent/descriptor` v3**：`{"version":3,"provider":"spawn"|"fork","mode":"one-shot"|"continuable","label":...,"route":{"provider":...,"model":...,"effort":...},"inherited":N}`，`persona`/`tools` 字段保留。`route` 必填，`effort` 省略表示不发送 effort；未知字段、非法 effort 和空 provider/model 被拒绝。v1、v2 与 `in-process` provider 被拒绝。descriptor 是 child 自己写的第一条记录，必须位于 `inherited + 1` 号序列且不在 turn 内；spawn 的 `inherited` 必须为 0（省略）。
2. **fork 种子**：fork child 创建时复制 parent 最后一个 `turn/end` 为止的事件（不含调用方进行中的 turn），序号保持不变，因此 compaction 的 shadowed 序号仍然有效。`transcript.OpenOptions.Seed` 让 JSONL 管理器把 header 与种子一次写入、一次 `fsync`，先整体校验连续序号、记录 schema 和闭合因果顺序。之后追加 descriptor 与 `never` 策略。child 的模型 surface 因此直接包含 parent 已完成的会话，并按第 7 节固定使用 parent 委派时的 route。
3. **`subagent/catalog`**：parent 在创建 child 的工具 step 内写 `{"type":"subagent/catalog","turn":T,"step":S,"catalog":{"session_id":...,"mode":...,"label":...}}`。记录必须位于活动 step，同一日志内 `session_id` 唯一。它不进入 surface。
4. **自有事件**：`session.OwnEvents` 以最后一个 descriptor 的 `inherited` 为界区分继承前缀；`session.Children` 只读取自有事件中的目录，fork 继承的 parent 目录不属于 child。会话自有的状态同样只从自有事件折叠：规划模式（`session.ProjectPlan`，见 [ADR-0014](0014-user-questions-and-plan-mode.md)）与目标（见 [ADR-0016](0016-long-running-goals.md)）。sandbox 与此不同：spawn/fork 在委派时捕获父显式 override，child 自有 delegation 模式覆盖种子中较旧策略，cold resume 保留；approval 始终为 `never`，见 [ADR-0021](0021-session-sandbox-modes.md)。种子中的这些记录仍按顺序规则原位校验，只作为 parent 的历史存在；种子带入 child 模型 surface 的只有消息、工具调用与结果和 compaction 摘要。
5. **消息来源**：`agent-message` 与 `subagent-settled` 的 source 必须带 `sender_session_id`（发送消息的会话，或结算的 child），其他 source kind 与 assistant 消息不得带它；decoder 同时拒绝缺失和多余的发送者。上游 source 的 `form` 与结算 `summary` 只服务界面展示，本仓不持久化：消息正文已含发送者与结算摘要，TUI 按 kind 区分显示。
6. **runtime context**：delegated child 的委派说明是完整 `runtime-context` 快照的一段，快照以 `user/message` 持久化，见第 7 节。

旧二进制遇到新记录或 descriptor v3 时按未知记录或非法字段拒绝；composition token 截至 ADR-0019 为 `subagent-tools-v5`，当前值以 `cmd/nano-harness/main.go` 的 `compositionID` 为准（v3 引入本 ADR 的记录，v4 引入继承 route、发送者身份与 runtime context，v5 让这些工具的 tool/result 按 [ADR-0019](0019-structured-tool-results.md) 持久化 `SubagentError` 分类），旧组合创建的会话按 composition mismatch 拒绝恢复，不迁移。本仓尚无发布 tag，没有需要迁移的会话。新记录与其他事实同存于 `0600`、写后 `fsync` 的只追加日志，受单 record 6 MiB、单 session 64 MiB 限制；fork 种子计入 child 的 64 MiB，parent 接近上限时 fork 失败。

### 5. 冷恢复语义

驻留状态只在进程内。进程重启后，恢复 root 会话即可通过它的目录看到全部 child（`inactive`）；`send_message` 校验目录后以 `registry.Create` 重新打开 child 日志，按常规修复中断尾部，再从 descriptor 与 header 恢复 parent、depth、mode、label 和继承的 route，并核对 header 中的 parent 与目录一致、mode 为 continuable。transcript 被其他 writer 持有或无法读取时返回 `subagent "<id>" is unavailable`。结算报告只看本次驻留新增的事件；冷恢复为上次驻留补写的结束事实在边界之前，不计入。

### 6. job 的 owner 释放

`job.Service.Release(ctx, owner)` 取消 owner 的 live job，等待全部 settle，然后删除该 owner 的全部记录；这些 settle 不发通知。subagent 服务在关闭每个 child agent 后调用它，因此 child 启动的后台 one-shot（及其子树）随 child 结算或拆除一起结束。root 的 job 仍由 `jobs` 插件关闭时回收。one-shot agent 只在它唯一的 turn 运行期间接受通知，turn 结束后既不接受新通知，也不为来不及投递的通知开启第二个 turn；因此 one-shot child 的后台 job 在其 turn 之后完成时，通知被拒绝并随释放丢弃，报告只来自那一个 turn。

### 7. 继承 route 与委派 runtime context

**继承 route。** 创建 child 时，服务读取 parent 最新的 `request/header`，把其中的 provider、model 和 effort 写入 descriptor 的 `route`；parent 还没有发出请求时委派失败。child 的每个 step 与冷恢复都使用这一 route：request header、system prompt 中的 route 行、`PrepareCall`、retry 策略键和工具执行 route 都取自它，`llm.Request.Effort` 携带 header 冻结的 effort（含省略），provider 以它代替目录中当时的 effort。context window 仍按该 provider/model 查当前设置目录。root 与 parent 自身继续跟随热切换的设置 route（[ADR-0002](0002-provider-neutral-agent-harness.md)）。route 所指的模型从设置中移除后，child 的请求以 `PrepareCall` 的未知模型错误失败，不改用其他模型。child 的 compaction 同样使用继承的 route：压力阈值和保留量按该 provider/model 在当前设置目录中的 context window 计算，摘要调用在该 route 上发出并携带继承的 effort，`compaction/summary` 记录这一 provider、model 和 effort；设置热切换与冷恢复都不改变它。上游 `compaction-basic` 在没有单独配置摘要模型时同样取会话最新请求的 route（child 即继承的 route），但不显式传 effort、由模型默认值决定；本仓传继承的 effort，使摘要请求与 child 其他请求一致。目录中已不再列出该模型时窗口未知，压力 compaction 不触发，context-window 错误与手动请求仍强制执行。

**委派 runtime context。** child 与 parent 使用同一 system prompt 组装，不再有 child 专属段落，因此同 route 的 fork 请求在继承历史之前与 parent 前缀一致。subagent 服务注册的 step context provider 从权威 descriptor 判断委派身份，每个 step 为 delegated 会话贡献以下 section：

```text
You are a delegated subagent: your permission scope was fixed when you were started and cannot be widened from inside this session — operations that require approval are rejected automatically. When the task needs access beyond that scope, do not retry the denied operation; state the limitation in your reply so the delegating agent can handle it.
```

文本与上游 `SUBAGENT_DELEGATION_CONTEXT` 一致，包含审批自动拒绝、不要重试被拒操作、向委派方说明限制三项指引。engine 将它与 sandbox 等当前 section 聚合为一份完整快照，再添加一次取代旧快照的声明、比较并提交；所有 provider 观察同一份日志，独立的 skill 消息在快照之后提交。快照位于任务消息之后、`step/start` 之前，与上游先聚合 section 再包装的顺序一致。恢复后同一完整快照仍可见，不重复提交；compaction 隐藏它后，下一个 step 重建全部 section。fork 保留继承前缀，在 child 自有任务之后追加含 sandbox 与委派范围的完整快照。root 不贡献委派 section，但仍有 sandbox section。来源与版本识别由 [ADR-0021](0021-session-sandbox-modes.md#模型策略上下文) 拥有。

## 后果

模型在本仓与上游看到同样的委派工具，可以并行启动后台 child、继续工作并在 child 结束时收到通知；parent 与 child 可以在任务中途交换信息。fork child 获得完整的已完成会话而非截断文本，代价是复制 parent 日志前缀的磁盘空间与一次写入。

结算即释放 child agent，长期闲置的 continuable child 不占 goroutine 与 writer lock；代价是之后的消息需要冷恢复。continuable child 结算时会终止它仍在运行的后台 job，这与上游的 owner 释放一致。

已知限制：

- 只有中断前已接受但尚未提交的消息，且中断后没有新的唤醒输入时，child 才继续等待下一次投递。服务按本次驻留投递的消息与结算通知计数，与日志中已提交的 `agent-message`/`subagent-settled` 比较；仍有差额、且 agent 报告自己仍持有已接受的通知（`Status().Queued > 0`）时，child 保持驻留（`list_agents` 显示 `inactive`）。这一判定与最后一个 turn 的结局无关：以取消结束的 turn，以及失败后在追加 `turn/end` 期间被中断、结局保留 `error` 的 turn（[ADR-0024](0024-provider-stream-idle-timeout.md)），都会留下通知而不开启新 turn。agent 在判定前已停止、内存中的通知已清空时，`Queued` 为 0，差额不再阻止结算，照常结算；驻留期间才停止的 child 不会触发重新判定，由下一次投递（发现 agent 已停止并释放句柄）或服务关闭回收。产品关闭时 registry 先于 subagent 服务关闭，服务停止时取消 watcher，不会泄漏。中断后新接受的消息在旧 turn 退出后自动唤醒下一 turn，一并提交此前排队的消息。parent 不再发送新输入时，中断前的待投递消息与池名额保留到服务关闭。
- 后台任务通知不经过 subagent 服务；它与 child 结算并发到达时可能落在正在关闭的 agent 上而丢失，被释放的 job 本身已经结束。
- 待投递消息与驻留状态只在内存中；进程崩溃会丢失已接受但尚未写入 child 日志的消息。
- 目录的读不到状态只报告 `unavailable`，不区分上游的 `corrupt`。

## 被否决方案

- **保留同步 spawn/followup 工具**：与上游 Base 定义和后台通知能力不一致。
- **fork 继续使用有界文本快照**：描述中“seeded with all completed turns”不成立，且混入进行中的 turn。
- **逐条 Append 复制 fork 种子**：每条记录一次 `fsync` 和一次全量顺序校验，长会话 fork 的成本为 O(N²)。
- **从会话 header 扫描推导子代理列表**：需要读取全部 child 日志才能得到 mode 与 label，fork child 的日志还包含整个 parent 前缀。
- **结算后保留 child agent 驻留**：长期占用 goroutine 与 writer lock，`inactive` 也无法与上游语义对应。
- **把后台任务通知改经 subagent 服务转发**：会在 job 与 subagent 服务之间形成构造环，只为缩小一个已接受的竞态窗口。

## 复审触发条件

上游改变这些工具的定义、通知文案或授权规则；产品需要子代理写文件、按调用选择模型或跨进程共享 child；需要承诺跨版本恢复旧会话；被中断后保留驻留的 child 长期占用池名额，或通知竞态在实际使用中造成问题。

# ADR-0016：长期目标、goal/change 会话记录与自动轮次

- 状态：Accepted
- 日期：2026-10-05
- 决策者：nano-harness maintainers

## 背景

参考提交 `5badb15009ae`（`dsh-v0.2.1-alpha.1`）的 Base 组合挂载 goal 组的四个包：`dsh-goal`（每个 session 一个持久目标）、`dsh-tool-goal`（`get_goal`、`create_goal`、`update_goal`，默认 `blockedAfterConsecutiveRounds: 3`）、`dsh-goal-round-driver`（空闲时自动续跑）和 `dsh-command-goal`（`/goal`）。上游要点：

- 状态只存于 session 日志：每次变更写 `goal/change`（version 1）完整快照，clear 写 revision 加一的 tombstone；准入轮次是带 `{goalId, revision, round}` 来源的 `user/message`。严格 fold 拒绝畸形形状、revision 不连续、非法迁移、时间倒退和不连续的轮次。
- 阶段 `active`/`paused`/`blocked`/`complete`，另有进程内的 `armed`/`disarmed`：create、resume 置 armed，pause、complete、block、clear 解除，edit 保持；任何 session 开启边沿都解除。变更携带精确 `{id, revision}` 做 compare-and-set。
- 工具权限在执行点判定：create、edit、pause、resume 需要运行时 root agent 当前 turn 中的 `{kind: 'user'}` 消息；complete 与 blocked 另接受当前目标的确切当前轮次，自主 blocked 至少 3 轮。模型不能 resume durable paused 目标。自主轮次成功 complete/blocked 后延迟注入 `<goal_complete>`/`<goal_blocked>` 收尾指令。
- driver 在整个 agent 空闲时预约 `roundsStarted + 1` 并 followup 一条 `<goal_round>` 提示；pre-step 栅栏拒绝陈旧预约，只有进入历史的消息消耗轮次。到上限以 `round-limit` 阻塞；被取消的轮次在下一个空闲点暂停，无关取消只解除 armed；人类 pause 中止当前 turn，模型自己的 pause 正常结束；`max-tokens` 与 agent 错误解除 armed；卸载时解除并取消在途轮次。

本仓的会话格式、agent worker 与命令面和上游不同。新增持久化记录、改变核心循环与模型输入都须由 ADR 说明（根 AGENTS.md）。总体范围见[工具对齐计划](../../.agents/notes/proposed/2026-10-04-upstream-tool-parity.md)，定义权威规则沿用 [ADR-0007](0007-upstream-base-tool-definitions.md)，复用 [ADR-0009](0009-background-jobs.md) 的 `Notify`。

非目标：独立评估器、token/费用/时间预算、并行多目标、`/goal` 附件、每命令轮次上限参数、Ralph 式新 agent 迭代、异常失败的自动重试。

## 决策

> 后续决定：[ADR-0018](0018-goal-stop-outcomes.md) 拥有输出截断事实、开场持久化失败停止推进及按确切 ID/revision 结算的契约；本 ADR 拥有目标状态、权限、轮次驱动和恢复规则。下文的停止语义已同步到该后续决定。

### 领域与记录

`internal/core/session` 定义 `GoalSnapshot`、`GoalChange` 与严格折叠 `GoalState.Apply`/`ProjectGoal`。新记录：

```json
{"type":"goal/change","goal":{"operation":"block","snapshot":{"id":"goal-…","revision":5,"objective":"…","phase":"blocked","blocked_reason":{"code":"model-reported","message":"…"},"max_goal_rounds":3},"rounds_started":2,"created_at_unix_ms":1000,"updated_at_unix_ms":5000}}
{"type":"goal/change","goal":{"operation":"clear","cleared":{"id":"goal-…","revision":7},"cleared_at_unix_ms":7000}}
```

- `turn` 与 `step` 必须缺省；记录可出现在任何位置，因为人类命令可在 turn 进行中提交。字段使用本仓 snake_case，不带上游的 `kind`/`version`，版本由会话格式与 composition 识别（见下文）。
- 形状规则：操作为 create/edit/pause/resume/complete/block/clear；快照 ID 为 1–128 字节无换行标识；revision ≥ 1；objective 按 ECMAScript `trim()` 去除首尾空白后非空且不超过 16 KiB；上限为 1 到 2^53−1；`blocked_reason` 恰在 blocked 时出现，code 为 lower-kebab-case（≤ 64 字节），message 按同一空白集去除首尾空白后非空且不超过 16 KiB；创建时间为正且更新时间不早于它；clear 只有 tombstone 与清除时间。工具入口与 durable decoder 的文本、goal ID、clear ID 和轮次来源 ID 统一采用 ECMAScript 空白集（含 U+FEFF，不含 U+0085），不能写入 decoder 会拒绝的归一化文本。
- 折叠规则与上游一致：create 要求 revision 1、active、零轮次、没有未完成的当前目标且 ID 未用过；其余操作要求同一 ID、revision 加一、保持创建时间与轮次计数、更新时间不倒退；只有 edit 可改 objective 与上限且不得改阶段与阻塞原因；pause 只从 active，resume 从 active/paused/blocked 且轮次未满，complete 从任何未完成阶段，block 只从 active；clear 必须 tombstone 下一 revision 且时间不早于最近更新。
- 准入轮次是 `source.kind = "goal"` 的 `user/message`，`source` 增加 `goal_id`、`goal_revision`、`goal_round`。三者只在 goal 来源出现且必须齐全，助手消息不得使用；折叠要求它正是当前 active 目标当前 revision 的下一轮且不超过上限。
- JSONL 的 order validator 对每个事件执行同一折叠；追加时非法事实被拒绝且文件不变，读取时整份日志被拒绝。fork 子代理的种子前缀里的父目标事实同样在原位校验并接受。
- 一个 session 的目标只从它自己提交的事件折叠（`session.OwnEvents`，与 `subagent/catalog`、规划模式的投影一致）：fork 继承的父目标、轮次和 turn 结局属于父会话，不成为子会话的目标，也不参与子会话的准入、权限与结算。父目标描述总体任务，fork 子代理执行委派的局部任务；复制目标会让 child 的目标状态与分工不符，并让 child 的 complete/blocked 看似能结算父任务，实际上又无法更新父日志。本仓因此保留上下文快照而隔离目标所有权。

### 服务、权限与工具

`internal/app/goal.Service`（插件 `goals`）是唯一写 `goal/change` 的组件。它经 `Registry.Journal` 取得 live agent 的日志，每个操作在服务锁内折叠、校验、追加并更新进程内 armed 表，提交后通知 watcher。错误码与文本沿用上游 `GoalError`（如 `stale goal ref …`、`cannot pause goal … from phase …`），本仓新增 objective 超过 16 KiB 的同码错误。时间戳取墙钟并钳位到不早于最近更新；目标 ID 为 `goal-` 加 16 字节随机数的十六进制。

“直接来自人类的根权限”在执行点判定：`Service.Authority(sessionID, turn, delegated)` 读取调用方 turn 的已提交 `user/message`。若有 `source.kind = "user"` 且调用方不是 delegated，即人类权限；若有当前目标当前 revision 当前轮次的 goal 来源消息，即轮次权限。本仓的 `user` 来源只由前端在人类输入时使用（TUI 提交、steer、`/plan TEXT`、附图），后台通知（`tool-jobs`）、规划提示（`plan-mode`）、skill 目录与注入（`skill-catalog`、`skill-invocation`）、委派任务与 agent 消息（`delegation`、`agent-message`、`subagent-settled`）、目标轮次（`goal`）和收尾指令（`tool-goal`）各有来源，所以它们开启的 turn 不具人类权限；人类在这样的 turn 中 steer 后即具备。这一不变量由守卫测试 `TestHumanSource_OnlyFrontendsAttributeHumanInput`（`internal/app/goal`）执行：它用 `go/parser` 解析 `cmd/` 与 `internal/` 下全部非测试产品源码（不含仓库工具 `internal/tools` 与 `testdata`），找出把 `Kind` 设为 `"user"` 或 `HumanSource` 的复合字面量键与赋值，要求它们只出现在 `internal/adapter/tui` 与 `internal/adapter/media/image`（`/attach`）。新的生产者若借用 `user` 来源，测试失败；新增人类输入前端必须同时修改允许列表并经评审。delegated 判断使用持久化的 delegation（`Invocation.Delegated`）：child 永远以 child 身份恢复，与上游“运行时拥有关系”在本仓等价，这与 [ADR-0014](0014-user-questions-and-plan-mode.md) 的判断相同。

`internal/adapter/tool/goal`（插件 `goal-tools`）注册三个工具，名称、描述与参数 schema 与上游 Base 逐字节一致。全部 exclusive，不需要 approval。`update_goal` 携带上游 `tool:goal` 段落（order 2400，阈值 3）。执行顺序与文本沿用上游：先校验 `goal_id` 非空且去空白、revision 为正安全整数；edit/pause/resume 先要求人类权限；空字符串与 0 视为严格 schema 的占位；paused 目标的 resume 返回 `the model cannot resume a paused goal; the user must resume it`；complete/blocked 先判定权限，再拒绝不属于该动作的参数，自主 blocked 在不足 3 轮时返回 `blocked requires at least 3 consecutive goal rounds; current round is N`；blocked 原因以 code `model-reported` 保存。结果是上游紧凑 JSON，字符串按 `JSON.stringify` 规则引用。错误 code 只在进程内错误对象上保留，tool result 与持久化日志当前只保存错误正文。get_goal 对 delegated agent 可用，返回其自身 session 的目标；fork 子代理在自己创建目标之前读到 `{"goal":null}`，而子代理不具人类权限，所以实际上总是如此。

自主轮次中成功的 complete/blocked 经 `Registry.Notify` 投递收尾指令（上游原文，`source.kind = "tool-goal"`）。上游把它作为本次工具结果之后的延迟上下文；本仓在该工具 step 的 `step/end` 之后作为 `user/message` 追加，turn 因此再走一步回复用户，模型可见内容相同。已在最后一步时，通知按 ADR-0009 留待下一个 turn。投递失败不撤销已提交的变更。

### 核心循环变化

engine 增加按 source kind 注册的 `Admission`（`Engine.RegisterAdmission`，scope 所有）。worker 取出 turn 后，若开场消息的 kind 已注册，admission 在排除并发状态变化的同时调用 `open` 提交 `turn/start` 与开场 `user/message`，或以 `agent.ErrNotAdmitted` 丢弃。被丢弃的 turn 不写任何记录，`TurnResult` 没有 turn 与 outcome，也不覆盖 `Status().Last`。目标服务为 `goal` 注册 admission：持有与变更相同的锁，只接纳当前 active、armed revision 的下一轮，且该 revision 没有被日志中的归属停止（取消、失败或输出截断）撤销。停止归属由 ADR-0018 定义，旧轮次的结局即使晚于新 create/resume，也不撤销新授权。这取代上游的 pre-step 栅栏：陈旧轮次从不进入日志。

`internal/app/goal.Driver`（插件 `goal-driver`）驱动 root agent，不另建 turn 启动路径：

1. 启动时解除该 session 的 armed，并从当前日志末尾开始观察。
2. 循环等待 root 的 `WhenIdle`（不等待其驻留的 continuable 子代理），然后 `Settle` 自上次以来的事件：被取消的目标轮次在其 revision 仍为当前、active、armed 时暂停（暂停失败则解除），error 与 `max_tokens` 目标轮次只解除其开场消息所属 ID/revision 的 armed，非目标轮次的取消、error 或 `max_tokens` 解除结束时当前 revision；之后的 create/resume 抵消前面的停止。同一结算窗口内的结局按顺序累积而不互相覆盖：轮次被取消后再有取消的 turn 仍暂停该轮次 revision；之后的 error 或 `max_tokens` 先解除同一 revision，与上游 `agent/error` 先解除再判断暂停的顺序一致，因此不再暂停；只有 create/resume 清空此前的暂停与解除。新授权即使先于旧轮次结束，也不受旧结局影响。step limit 不影响继续。
3. 目标 active 且 armed 时：达到上限以 `round-limit` 阻塞；否则用 `Followup` 排入上游原文的 `<goal_round>` 提示并等待其结果。排队失败以 `queue-failed` 阻塞；admission 拒绝后若下一次计算出的轮次来源不变（既无新 revision 也未被撤销），以 `prompt-rejected` 阻塞。非准入拒绝的轮次错误（含没有 `turn/end` 的开场追加/fsync 失败）解除该轮次 revision 的 armed，不重排；否则等待目标变更通知。
4. watcher 收到人类（`ActorHost`）的 pause 时中断当前 turn；模型与 driver 的 pause 不中断。
5. cleanup 先解除 armed（排队中的轮次因此被 admission 拒绝），再取消循环；在途轮次被中断并等待结果，受 shutdown 期限约束。driver 最后启动，因此在目标服务撤回 admission 之前停止。

“整个 agent 空闲”与上游一致，指 root agent 自身没有活动、排队或被通知唤醒的 turn。上游 driver 检查的是该 agent 的 `status === 'idle'`，`whenIdle()` 文档写明它等待“当前 whole-agent activity”，即该 agent 的 driver 与维护任务，不包括后代 agent。因此 root 有驻留的 continuable 子代理在后台工作时，driver 仍会排下一轮；子代理结算或发来消息时，`Notify` 会唤醒 root，driver 随之等待那次 turn。等待整棵树空闲会让后台子代理阻塞目标推进，并与上游行为不同，所以不采用。

规划模式、approval 等待和用户打断都不需要专门分支：轮次是普通 turn，服从当前规划段落、approval policy 与等待；人类 `/interrupt` 使轮次以 canceled 结束，driver 随后暂停该目标。

输出停止事实、三个 provider 映射、截断工具提案与通知处理、v2 枚举及兼容规则由 [ADR-0018](0018-goal-stop-outcomes.md) 定义。

### 模型输入变化

- 工具目录新增 `create_goal`、`get_goal`、`update_goal`，system prompt 在工具段落中加入 `tool:goal` 原文，二者随 `request/header` 冻结。原样 guidance 中“fork 后 active goal 为 disarmed”描述上游复制目标的行为；在本仓它只表达 fork 不会自动续跑父目标，child 实际没有当前目标，`get_goal` 返回 `{"goal":null}`。恢复本会话时则保留它自己的 active 目标并解除 armed，二者不同。
- 自动轮次以 `<goal_round>` 用户消息进入 surface，包含 JSON 引用的 objective 与 `Round: N/M`；自主终结后的 `<goal_complete>`/`<goal_blocked>` 收尾指令同样是用户消息。`goal/change` 本身不进入 surface。

### `/goal`

TUI 的 `/goal` 实现上游语法：空参数显示状态（阶段、阻塞原因、objective、轮次、activation 与可用命令），`clear`/`pause`/`resume`/`edit` 仅在占满输入（edit 后跟 ECMAScript 空白与文本，按完整 UTF-8 rune 解码分隔符）时是控制词，其余非空文本创建目标；未完成的目标不能被直接替换，complete 后 `edit` 创建新目标。领域拒绝显示为上游固定文本 `The goal command is not valid for the current state. Run /goal to view available commands.`，意外错误原样显示。命令以 `ActorHost` 经 `app/goal` 用例操作，输出不进入模型请求。状态栏从日志折叠显示 `goal=<阶段> <轮次>/<上限>`。附件暂不支持：附件 turn 与 driver 的第一轮之间没有可证明的顺序，待发送图片时 `/goal` 拒绝并保留图片。

### 版本识别、拒绝旧格式与恢复

- session format 保持 v2。`goal/change` 与消息来源的三个字段是加法格式，与 [ADR-0004](0004-provider-neutral-effort.md)、ADR-0014 一致：不含它们的 v2 日志仍可解码；较旧的二进制遇到 `goal/change` 或未知来源字段按未知记录/字段拒绝整份日志。
- composition ID 使用 `goal-tools-v2` 绑定目标停止语义（见 [ADR-0018](0018-goal-stop-outcomes.md)）。由不含目标工具的组合创建的会话恢复时因 composition mismatch 被拒绝，不迁移，也不静默接受。本仓尚无发布 tag，没有需要迁移的已发布会话。
- 严格 decoder 拒绝未知字段、未知操作与阶段、缺失负载、带 turn/step 的记录和任何违反折叠的事实；非法行使整份日志被拒绝，文件不被截断或改写，维护者仍可离线检查原始数据。ECMAScript 空白修补保持 format v2 和 composition ID；原先误接受的首尾 BOM 文本与 ID 现在拒绝，含 NEL 的合法文本保持原文，不自动迁移旧记录。
- `goal/change` 与其他事实保存在同一个只追加、`0600`、写后 `fsync` 的 JSONL 中，受单 record 6 MiB 与单 session 64 MiB 限制，compaction 不删除它，保留期与会话文件相同。resume 修复中断尾部不追加、不改动目标记录；恢复后目标、阶段、revision 与轮次计数不变，自动继续一律 disarmed，需人类 `/goal resume` 或在人类 turn 中由模型 resume。

## 后果

目标的模型可见定义、提示、结果与错误文本和上游一致，所有目标事实都可从同一份日志重建。陈旧轮次在 admission 处丢弃，不会形成需要回滚的半个 turn。

代价与风险：

- engine 多一个按 kind 查找的 admission，目标服务每次操作与每次准入都折叠整份事件快照，与既有的 surface 折叠同量级。
- 进程内持有 session 写权限的组件仍可伪造 `goal/change`；折叠只检测畸形或不一致的事实。
- 轮次上限只限制轮次数，一个从不完成的目标默认最多运行 256 轮模型调用。
- 人类 pause 中断任何正在运行的 turn，包括人类自己的 turn，与上游相同。
- 收尾指令在 `step/end` 之后追加，位置与上游不同；在 turn 最后一步完成时，收尾指令开启下一个 turn。
- `/goal` 不支持附件。
- 上游 fixture 的三个工具定义与 `tool:goal` 段落需在参考指针更新时重新推导。

## 被否决方案

- **沿用上游 pre-step 栅栏，在 step 前丢弃陈旧轮次**：本仓 turn 的开场消息在第一个 step 之前就已提交，陈旧轮次会先写入 `turn/start` 和非法的 `user/message`；在开场处准入更早也更简单。
- **放宽折叠，让陈旧轮次不计数**：模型会看到旧 objective 的轮次提示，被暂停的目标还会继续跑完一整轮。
- **工具通过 `Invocation.Journal` 读取事件**：需要扩大所有工具共享的 `tool.Journal` 接口；只有目标需要读取，改由目标服务经 registry 解析日志。
- **driver 订阅 durable 事件流判断 turn 结局**：订阅在背压下可丢弃；改在空闲时从日志结算，顺序准确且不会漏掉取消。
- **收尾指令改为结束 turn 的硬停止**：上游已用收尾指令取代硬停止，让模型回复用户一次。
- **提升到 format v3 或在记录内携带版本**：与 ADR-0010、ADR-0014 相同，加法记录没有歧义，恢复边界已由 composition ID 控制。
- **`/goal` 附件以普通用户消息提交**：无法保证它先于 driver 的第一轮进入历史，与上游“附件先于下一轮”的承诺不符。

## 复审触发条件

上游改变三个工具的定义、`tool:goal` 段落、轮次提示或收尾指令、权限规则或阈值；需要独立评估器、资源预算或多目标；需要 `/goal` 附件或其他前端的目标命令；出现第二个需要 admission 的 source kind；首次发布需要承诺旧会话迁移。

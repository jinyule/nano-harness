# ADR-0014：用户提问接缝、规划模式与 plan/mode 会话记录

- 状态：Accepted
- 日期：2026-10-05
- 决策者：nano-harness maintainers

## 背景

参考提交 `5badb15009ae`（`dsh-v0.2.1-alpha.1`）的上游组合提供两项本仓缺少的交互能力：

- `ask_user_question` 来自 Web preset：`packages/bundle/web-app/presets/standard.patch.yml` 挂载 `@deepseek-ai/dsh-tool-ask-user` 且不带配置，选用默认的阻塞（legacy）定义。它通过 `ctx.userQuestions` 接缝提问，答案以紧凑 JSON 返回。上游 `ask()` 只拒绝空问题和不自洽的 intent；只有可选的 timed 变体检查问题 id 唯一。运行时被其他 agent 拥有的子 agent 以 `DELEGATED_CALLER` 拒绝。阻塞模式不写额外会话记录：问题在 `tool/call` 参数中，答案在 `tool/result` 中。
- 规划模式来自 Base 组合的 `@deepseek-ai/dsh-plan-mode`。状态是只写日志的整值事件 `plan/mode {active}`，最后一条生效。用户选择在没有打开的 turn 时立即追加；turn 进行中则保留在进程内，到下一个被接受的 step 前置点追加。激活时 system prompt 在 `PLAN_POLICY`（500）位置加入 Base 配置的 `section` 原文。`exit_plan_mode` 始终在工具目录中：规划模式外执行失败；规划模式内通过提问接缝提交计划（`plan-review` intent，`Approve`/`Keep planning`），获批后在下一个 step 前置点静默记录退出。用户切换且上一次请求描述的是另一种模式时，追加一条用户切换提示。上游明确只靠提示词约束，“every tool remains available”，强制限制交给 sandbox 与 approval。

本仓的会话格式、approval 和 subagent 模型与上游不同，新增持久化记录前必须说明版本识别、旧格式拒绝和恢复路径（根 AGENTS.md）。总体范围见[工具对齐计划](../../.agents/notes/proposed/2026-10-04-upstream-tool-parity.md)，定义权威规则沿用 [ADR-0007](0007-upstream-base-tool-definitions.md)。

非目标：timed 提问、pending 问题与迟到回答（`user-question-reply`）、Web 的 plan-review 专用卡片、创建 agent 时指定规划模式、通用协作模式注册表。

## 决策

### 用户提问接缝

`internal/app/question.Service`（插件 `user-questions`）是消费方接缝。前端用 `RegisterBroker(Broker, *plugin.Scope)` 发布唯一的回答面，scope 关闭时撤回。调用方使用：

```go
func (*Service) Ask(ctx context.Context, request question.Request) ([]question.Answer, error)
```

`Request` 携带 session、tool call ID、`Delegated` 和问题列表；`Question` 有 id、问题、可选标题、可选详情（`Detail`）、选项、多选和可选 `Intent`（目前只有 `plan-review`，`Approve` 指明批准选项）。`Answer` 的 `Selected` 是选项标签，永不为 nil；`Custom` 是自由回答，单选时替代选择，多选时补充；两者都空表示跳过该题。

`Ask` 依次拒绝：已取消的 context（`ask_user_question was aborted before the user answered`）、空列表（`ask_user_question requires at least one question`）、delegated 调用方（上游 `DELEGATED_CALLER` 原文），以及本仓的请求上限和 intent 校验。没有 broker 返回 `no user-questions answerer accepted the request`。broker 返回后先检查 context，等待中取消时返回 `question.ErrAborted` 和空答案，无论答案是否合法、错误是否为空；context 仍有效时，broker 返回 `question.ErrCancelled` 得到 `the user cancelled ask_user_question`，其他错误一律视为不可用。broker 的答案必须对每题恰好一条、只选该题提供的标签且不重复、单选至多一个且有 `Custom` 时不选标签、`Custom` 是不超过 16 KiB 的合法 UTF-8；否则以 `the user-questions answerer returned an invalid answer batch` 失败关闭。答案按请求顺序返回，切片与 broker 解耦。

本仓在上游之外增加边界限制，错误文本对模型可见：每次最多 16 题、每题最多 32 个选项；id 为去除首尾空白、无换行的 1–128 字节且在本次调用内唯一（沿用上游 timed 变体的唯一性文案）；问题文本和选项标签不能为空白；同题标签唯一，因为答案用标签回指选项。文本长度受[工具参数预算](0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败)约束。

delegated 判断使用持久化的 delegation（`Invocation.Delegated`）。本仓的 child session 总由 subagent 服务以 child 身份恢复，不能以 root 身份恢复，因此与上游“运行时拥有关系”判断在本仓等价；上游允许以 root 恢复带血缘的会话后提问，本仓没有这条路径。

### ask_user_question

`internal/adapter/tool/question`（插件 `question-tools`）注册阻塞定义，名称、描述和参数 schema 与上游逐字节一致，两层嵌套对象声明 `additionalProperties: true`：嵌套未知成员被接受并丢弃，根对象仍按 ADR-0007 严格拒绝。工具不贡献 guidance，没有并发声明，因此是独占调用。结果是 `{"answers":[{"id":"…","selected":[…],"custom":"…"}]}`，按问题顺序，`selected` 总是存在，`custom` 为空时省略，与 `JSON.stringify` 一样不转义 `<`、`>`、`&`；Go 仍会转义 U+2028/U+2029，这是已知的字节差异，JSON 语义相同。失败成为 `Error: <message>` 结果。

问题与答案不新增会话记录：问题是已提交的 `tool/call` 参数，答案、取消或不可用是唯一的 `tool/result`。等待期间进程退出时，resume 按既有规则补写 `Error: interrupted before a result was committed`；不提供迟到回答。

TUI 是唯一 broker，与 approval 共用同一交互锁逐题提问：输入全为数字列表时按选项编号选择（单选只能一个），其他非空文本作为自由回答，空输入跳过；带 `(Recommended)` 的第一个选项预先填入输入框，用户可见并可清除；`Detail` 逐行显示在问题下方；Ctrl+C 取消整批；终端停止或界面消失时不可用。

### 规划模式状态与记录

`internal/app/plan.Service`（插件 `plan-mode`）拥有进程内的待生效选择和 step 边界提交。持久化事实是新记录：

```json
{"type":"plan/mode","turn":2,"plan":{"active":false}}
```

- `step` 必须缺省。`turn` 为 0 表示在 turn 之间记录；否则必须等于当前打开的 turn，且该 turn 没有打开的 step。记录必须改变当前模式，所以第一条只能是 `active:true`。它可以出现在 compaction 事务之间，不生成 surface 节点。
- `session.ProjectPlan(events)` 是唯一折叠规则：只看会话自己提交的事件（`session.OwnEvents`），其中最后一条 `plan/mode` 决定当前模式，没有记录即非规划模式；同时给出其中最近一个 `request/header` 写入时的模式。
- 规划模式属于选择它的会话。fork 子代理的种子复制了 parent 已完成 turn 中的 `plan/mode` 与 `request/header`，这些记录在 child 日志中仍按顺序规则原位校验，但不进入 child 的折叠，所以 child 总是从非规划模式开始，也不会因继承的请求头收到切换提示。上游的 fork 会继承规划状态；本仓不继承，因为 child 既不能选择模式（`SetPlanMode` 只接受 root），也不能通过审查（提问接缝拒绝 delegated 调用方），继承的规划模式永远无法离开；而且种子截止到 parent 最后一个 `turn/end`，本 turn 刚获批、尚未在边界记录的退出不在种子中，"批准后 fork 去实施"的 child 会收到“不要修改文件”的规划段落。parent 的模式与折叠不受影响，种子带入的消息和工具结果照常进入 child 的模型 surface。

读写方式：

- `Registry.SetPlanMode(ctx, sessionID, active) (plan.Change, error)` 是用户选择入口，只接受 live root agent。它在 agent worker 的状态锁内调用 `Service.Select`；worker 在追加 `turn/start` 前于同一把锁内标记忙碌，因此立即提交永远不会与 turn 开始交错。没有打开的 turn 时立即追加 `turn:0` 记录（`Committed`）；turn 进行中保留到下一个 step 边界（`Queued`）；撤回尚未生效的相反选择返回 `Cancelled`，重复选择返回 `Unchanged`。追加失败返回错误，状态不变。
- engine 在每个 step 的主动 compaction 之后、`step/start` 之前调用 `Service.Step(ctx, journal, turn)`：提交待生效选择，在需要时追加用户切换提示，并返回本 step 请求使用的规划段落。任一追加失败使 turn 以 error 结束，选择保留到后续边界重试。
- `Service.Active(sessionID)` 报告最近一次边界或提交后的模式，供本 step 内的工具使用；`Service.Exit(ctx, sessionID)` 在该会话的状态锁内检查 context；已取消时用 `%w` 保留取消原因且不改变待生效选择，否则记录一次获批退出。接受退出选择之后的取消不撤销已经接受的选择。
- 服务按会话加锁：每个会话的选择、边界和退出（包括其中的日志读取与 `fsync` 追加）由该会话自己的锁串行化，服务锁只保护生命周期和会话表，因此并发子代理的边界互不等待。锁顺序为 agent worker 状态锁 → 会话锁。会话表项在第一次选择或边界时创建，保留到服务停止，每项只有一把锁和几个布尔值；不在运行中删除表项，避免持锁调用方与新表项各自持有不同的锁。

用户切换提示是 `source.kind = "plan-mode"` 的 `user/message`，文本沿用上游：`The user switched this session to plan mode.` 或 `The user switched this session back to the default mode.`。只有用户选择会请求提示，而且只在最近一次 `request/header` 描述的是另一种模式时追加；首个请求之前或往返切换后净变化为零时不追加。它位于 turn 的用户输入之后、`step/start` 之前；上游把它放在同一 step 的消息里，位置不同但模型同样在下一请求看到它。

待生效选择只在进程内，与上游相同：在 turn 最后一个边界之后选择且进程在下一个 turn 前退出，选择丢失，界面需要重新选择。获批退出同样如此：如果进程在 `exit_plan_mode` 结果提交之后、下一个边界之前退出，恢复后日志中有获批结果但仍处于规划模式，模型会继续看到规划段落，用户可用 `/plan off` 离开。

### 模型输入变化

- 规划模式在边界提交后的状态为激活时，system prompt 在角色段落之后、工具列表与 guidance 之前加入 Base `section` 的 YAML 解析值，原文逐字保留，包括 block scalar 的结尾换行；该段落随 system prompt 冻结进 `request/header`，模型可见内容仍可从日志重建。
- 工具目录新增 `ask_user_question` 和 `exit_plan_mode`，两者在所有模式下都存在，进入或离开规划模式只改变 system prompt。
- 用户切换提示进入模型 surface；问题答案与审查结果以普通 tool result 进入。

### exit_plan_mode

`internal/adapter/tool/plan`（插件 `plan-tools`）的定义与上游 Base 逐字节一致。执行依次检查：会话在本 step 处于规划模式，否则 `exit_plan_mode is only available in plan mode`；去除首尾空白后以单个 `#`、空白和可见文本开头，否则 `exit_plan_mode requires a non-empty markdown plan starting with a # heading`。随后通过提问接缝发出上游同款审查问题（id `plan-review`，标题 `Plan review`，`Detail` 为计划原文，选项 `Approve` 与 `Keep planning`）。

- 恰好选择 `Approve` 且没有自由回答，并且退出选择检查时 context 仍有效：记录获批退出，返回 `Plan approved — plan mode exited; carry out the plan starting with your next step.`；本批次剩余调用仍在规划模式下执行，下一个边界追加 `plan/mode {active:false}`，不追加提示。
- 其他答案（`Keep planning`、跳过或反馈）：返回错误结果 `The user chose to keep planning; revise the plan and present it again.`，有反馈时为 `The user chose to keep planning; their feedback: <text>`，模式不变。
- 用户取消审查：返回上游的 `The user dismissed the plan review to speak instead; stay in plan mode, stop here, and wait for their message.`。
- 回答面不可用、取消或服务停止：返回接缝的失败文本，保持规划模式；`/plan off` 始终是手动出口。

子 agent 从不处于规划模式，调用时得到“只在规划模式可用”的错误。

TUI 的 `/plan` 进入、`/plan off` 离开、`/plan TEXT` 进入后把文本与待发送图片作为下一条用户输入（turn 进行中 steer，否则提交新 turn）；`/plan off` 带图片时在改变模式前拒绝。状态栏在规划模式下显示 `mode=plan`，转录用 `mode>` 行显示模式变化与切换提示，与任务清单面板的 `plan>` 区分。

### 执行点约束评估

上游只靠提示词约束规划模式。本仓评估后同样不在执行点阻止写类工具：

- 规划模式是协作模式，不是授权边界。root 会话中 `write`、`edit`、`bash` 每次都在执行点请求一次性 approval，`never` 策略直接拒绝，delegated agent 永不提权；这些规则在两种模式下都不变，并且 approval 前用户可以从状态栏看到规划模式。
- 规划需要运行非修改性检查（测试、静态分析、`git status`），这些都通过 `bash`；按工具名阻止会让规划无法完成，按命令内容判断“只读”又无法可靠实现。
- 只阻止 `write`/`edit` 会改变上游模型可见行为（同一工具在两种模式下结果不同），却不能阻止 `bash` 写文件，不带来新的安全保证。

因此规划模式不读取也不改变 approval、sandbox 或 tool allowlist；需要强制只读时使用 `/permission never`。

### 版本识别、拒绝旧格式与恢复

- session format 保持 v2。`plan/mode` 是加法记录，与 [ADR-0004](0004-provider-neutral-effort.md) 的加法字段一致：不含它的 v2 日志仍可解码。较旧的二进制遇到 `plan/mode` 按未知记录拒绝整份日志。
- composition ID 加入 `question-tools-v1` 和 `plan-tools-v1`。由不含这两个工具的组合创建的会话恢复时因 composition mismatch 被拒绝，不迁移，也不静默接受。本仓尚无发布 tag，没有需要迁移的已发布会话。
- 严格 decoder 拒绝 `plan` 负载中的未知字段、缺失负载、step 内的记录、错误 turn 和重复当前模式的记录；任一非法行使整份日志被拒绝，文件不被截断或改写，维护者仍可离线检查原始数据。
- `plan/mode` 与其他事实保存在同一个只追加、`0600`、写后 `fsync` 的 JSONL 中，受单 record 6 MiB 与单 session 64 MiB 限制，compaction 不删除它，保留期与会话文件相同。resume 修复中断尾部时不追加、不改动 `plan/mode`，已提交的模式在恢复后继续生效。

## 后果

提问和规划模式与上游同名同义，模型在两个实现间看到同样的工具定义、规划段落、审查问题和结果文本；问题、答案、模式变化和用户切换都从同一份日志重建，不需要额外存储。提问接缝是通用的：后续的长期目标等功能可以复用 `question.Service` 和 `plan.Service`，而不必依赖 TUI。

代价与风险：

- 规划模式不强制只读，模型仍可能在规划中请求写入，依靠 approval 由用户拒绝。
- 待生效选择与获批退出在进程内，存在上面记录的崩溃窗口。
- 本仓的提问上限和答案校验比上游严格；超过上限的调用得到明确错误。
- engine 每个 step 多读一次事件快照；与既有的 surface 折叠同量级。
- 两份目录 fixture 和 `upstream-base-tools.json` 的 `prompt_sections` 需要在参考指针更新时重新推导。

## 被否决方案

- **只在 broker 返回错误时检查取消**：合法答案与 nil 错误仍可越过取消，产生成功审查和待生效退出选择。
- **timed 提问与迟到回答**：需要 pending 结果、`user-question-reply` 消息来源和单独投影，没有现有前端或组合需要它；上游默认组合也不启用。
- **仿照 approval 另写 question asked/answered 记录**：与 `tool/call`、`tool/result` 重复同一事实，并引入第二套必须保持一致的顺序规则。
- **在任意位置立即记录用户选择**：记录位置不再对应生效边界，`request/header` 中的段落与折叠出的模式可能不一致。
- **在执行点按工具名阻止写类工具**：理由见上文评估。
- **规划状态保存在日志之外**：resume 和 fork 需要第二个恢复来源，违背日志是唯一事实来源的规则。
- **提升到 format v3**：会拒绝全部 v2 日志，而加法记录没有歧义，恢复边界已由 composition ID 控制。
- **通用协作模式注册表**：目前只有规划模式一个实例，上游也因同样理由否决。

## 复审触发条件

上游改变这两个工具的定义、Base `section` 原文、审查问题或结果文本；出现需要 timed 提问或迟到回答的前端；产品需要强制只读的规划模式；需要在进程重启后保留待生效选择；第二种协作模式出现；首次发布需要承诺旧会话迁移。

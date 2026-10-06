# 用户提问接缝、ask_user_question 与规划模式

- Status: implemented
- Date: 2026-10-05

## Context

这是[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP8。产品没有让模型向用户提问的能力，也没有规划模式。上游参考提交 `5badb15009ae` 中，`ask_user_question` 来自 Web preset（`tool-ask-user` 不带配置，即阻塞定义），`exit_plan_mode` 与规划指引来自 Base 的 `plan-mode`。调查结论：

- 上游提问接缝 `ctx.userQuestions.ask()` 只拒绝空问题与不自洽的 intent；只有 timed 变体检查 id 唯一；运行时子 agent 以 `DELEGATED_CALLER` 拒绝；阻塞模式不写额外记录，问题与答案分别在 tool call 与 tool result 中；结果是紧凑 JSON。
- 规划模式的持久状态是整值事件 `plan/mode {active}`。没有打开的 turn 时用户选择立即追加，否则保留到下一个 step 前置点；获批退出同样延迟且静默；用户切换在上一次请求描述另一模式时追加切换提示；system prompt 在 `PLAN_POLICY` 位置加入 Base `section`。上游只靠提示词约束，强制限制交给 sandbox 与 approval。
- 本仓已有最接近的参照：approval service 的 broker 注册、失败关闭和 TUI 交互锁。

非目标：timed 提问与迟到回答、plan-review 专用界面、创建 agent 时指定规划模式、通用协作模式注册表。

## Decision

边界校验与取消语义的补充实施见[修复 Note](2026-10-06-spill-question-and-call-validation.md)；本 Note 保留能力建立、生命周期和原始验证证据。

长期契约记录在 [ADR-0014](../../../docs/decisions/0014-user-questions-and-plan-mode.md)，当前事实归[架构](../../../docs/architecture.md#用户提问与规划模式)、[安全](../../../docs/security.md#用户提问与规划模式)和[测试](../../../docs/testing.md)文档。本次实施：

- `internal/app/question`（插件 `user-questions`）：`Service.Ask(ctx, Request) ([]Answer, error)` 与 `RegisterBroker(Broker, *plugin.Scope)`。请求在 broker 之前校验（上游文案加本仓上限），delegated 调用方被拒绝，答案逐题校验后按请求顺序返回并与 broker 切片解耦；取消、broker 缺失或失败、非法答案都失败关闭。broker 注册用指针 token 标识身份，因为函数类型的 broker 值不可比较，直接比较会在 cleanup 时 panic。
- `internal/app/plan`（插件 `plan-mode`）：`Select`（用户选择）、`Step`（engine 边界提交、切换提示和规划段落）、`Active` 与 `Exit`（供 `exit_plan_mode` 使用）。`Section` 是 Base YAML 的解析值，含结尾换行。服务按会话加锁：会话表项在第一次选择或边界时创建并保留到服务停止，每个会话的日志读取与追加只持有自己的锁。
- `internal/core/session`：新增 `plan/mode` 记录、`PlanMode`、`ProjectPlan` 折叠和形状校验；`jsonl/order.go` 只允许它在 step 之外、turn 0 或当前 turn，且必须改变模式。
- `internal/app/agent`：`NewEngine` 显式注入 `*plan.Service`；engine 在每个 step 的主动 compaction 之后、`step/start` 之前调用 `Step`，把返回的段落交给 prompt assembler（位于角色段落之后、工具段落之前）。`Registry.SetPlanMode` 只接受 live root，并通过 `Agent.selectPlan` 在 worker 状态锁内调用 `Select`，使立即提交不会与 `turn/start` 交错。
- `internal/adapter/tool/question`（`question-tools`）与 `internal/adapter/tool/plan`（`plan-tools`）：定义与上游逐字节一致；`exit_plan_mode` 通过提问接缝发出 `plan-review` 问题，结果文本沿用上游。上游的句子带结尾标点，用 `reviewError` 类型原样承载，避免 Go 错误字符串风格检查改写模型可见文本。
- TUI：独立的 `questionBroker`（`App.Ask` 已用于 approval）与 approval 共享交互锁；逐题显示、数字列表选择、自由文本、空输入跳过、推荐项预填、Ctrl+C 取消；`/plan`、`/plan off`、`/plan TEXT`；状态栏 `mode=plan` 标记与 `mode>` 转录行。新文件命名为 `question.go`、前缀用 `mode`，避开 WP4 任务清单面板的 `plan.go` 与 `plan>`。
- composition：插件顺序加入 user questions（approval 之后）、plan mode（prompt 之后）和两个工具 provider；composition ID 加入 `question-tools-v1`、`plan-tools-v1`；`tool-catalog.json` 与 `upstream-base-tools.json` 增加两个工具，后者新增 `prompt_sections` 记录 Base 规划段落原文；`TestComposition_MatchesUpstreamBaseTools` 的数量随之增加两项（合计 14）。
- mutation 新增 `question-delegated` 与 `plan-mode-boundary`。PTY 脚本在根任务中加入两题提问，并加入 `/plan` → `exit_plan_mode` → TUI 批准的流程；fixture 在切换提示跟在用户消息之后时仍按用户任务路由。

规划模式不在执行点阻止写类工具；评估和理由在 ADR-0014。

整体审查后的修复：

- fork 子代理曾继承 parent 的规划模式。WP7 的 fork 种子复制 parent 最后一个 `turn/end` 之前的全部记录，`ProjectPlan` 又折叠整份日志，所以种子里的 `plan/mode {active:true}` 让 child 每一步都带规划段落；child 不能选择模式，审查也以 delegated 被拒绝，于是无法离开。最常见的触发是本 turn 刚批准计划就 fork 去实施：退出要到下一个边界才记录，不在种子中。现在 `ProjectPlan` 只折叠 `session.OwnEvents`，与 `session.Children` 和 WP10 的目标投影一致；种子中的记录仍按顺序规则原位校验，消息与工具结果照常进入 child 的 surface，parent 不受影响。上游 fork 继承规划状态，这一差异写入 ADR-0014，ADR-0013 的“自有事件”一节补充了会话自有状态的规则。
- 服务原先在全局锁内执行每个会话的日志读取和 `fsync` 追加，并发子代理的每个边界都要排队。现在按会话加锁，服务锁只保护生命周期与会话表；锁顺序为 agent worker 状态锁 → 会话锁，没有反向获取。`Exit(ctx, sessionID)` 的取消检查随之移到该会话的锁内，`TestService_CancelledExitKeepsPlanMode` 改为持有会话锁来构造等待点。

空白、Unicode 边界与交互补充的实施证据见[交互与会话状态对齐](2026-10-06-interaction-state-upstream-alignment.md)；本 Note 保留各能力的初始组装、生命周期和持久化决定。

## Consequences

模型看到的两个工具、规划段落、审查问题和结果文本与上游一致；提问接缝与规划状态都是通用的 app 服务，WP10 可以直接复用。问题、答案、模式与切换提示都能从日志重建。

代价与风险：

- 待生效选择与获批退出只在进程内；获批结果提交后、下一个边界前崩溃时，恢复后仍处于规划模式。
- 本仓提问上限与答案校验比上游严格；Go 对 U+2028/U+2029 的转义与 `JSON.stringify` 不同。
- 问题等待期间 turn 被打断时，TUI 仍显示该问题直到用户回答或取消；迟到答案被丢弃，行为与 approval 相同。
- engine 每个 step 多读一次事件快照。
- 后续工作包仍会在这些位置追加内容：`session` 记录类型和 validate 的穷举列表、`order.go`、`application.go`/`main.go`/`main_test.go`、两份 fixture、`tui-e2e.py`、`mutation-cases.json`、`docs/testing.md` 的 mutation 数量与 composition ID 字符串。

重新评估条件见 ADR-0014。

## Verification

- `go test -race -count=1 ./...`：通过。
- `scripts/coverage.sh`（`make coverage`）：每个产品源文件 100.0%。
- `go run ./internal/tools/archcheck`：依赖方向通过。
- `golangci-lint run ./...`（v2.12.2，私有 `GOLANGCI_LINT_CACHE`）：0 issues。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0，依次覆盖 fmt、mod、vet、race test、architecture、submodule（`5badb15009ae…`，干净）、agent-notes、skills、workflow-tools、lint、coverage、mutation 和 build；`git diff --check` 干净。
- `make mutation`：十三个用例全部 killed，包括本次新增的 `question-delegated` 与 `plan-mode-boundary`。
- `make tui-e2e`：真实二进制、PTY 与 macOS `sandbox-exec` 通过。根会话 16 次工具调用（含其他工作包的 `todo_write`、后台 `bash` 与 `job_output`）全部成功；`ask_user_question` 的结果为 `{"answers":[{"id":"mode","selected":["Fast (Recommended)"]},{"id":"note","selected":[],"custom":"PTY_ANSWER"}]}`（Enter 接受预填推荐项，第二题键入文字）；`/plan` 后 `exit_plan_mode` 经 TUI 输入 `1` 批准；日志中的 `plan/mode` 为 `(true, turn 0)`、`(false, turn 3)`，存在 `plan-mode` 切换提示；fixture 记录到批准前的请求带 Base 规划段落、批准后的请求不带。
- assembled 测试（`cmd/nano-harness/plan_test.go`）复用 `composeApplication`，只把终端 broker 换成脚本前端：提问答案出现在磁盘 transcript 和下一次 provider 请求中；规划模式依次验证带反馈继续规划、批准、规划模式外拒绝，以及请求段落与 fixture 原文逐字一致和 `plan/mode` 的位置。
- 持久化固定样本 `session-v2-plan.jsonl`：读取、投影、只读恢复不改字节，独立构造的 writer 输出逐字节相同；六个篡改反例全部被拒绝；中断尾部修复不改动已提交模式。
- 门禁反例：把 `upstream-base-tools.json` 中 `ask_user_question` 描述改一个词，`TestComposition_MatchesUpstreamBaseTools` 失败；把 fixture 规划段落改一个词，规划模式 assembled 测试失败；恢复后通过。
- Base 段落由一次性程序用 `gopkg.in/yaml.v3` 解析 submodule 的 `packages/bundle/base/cordis.patch.yml` 得到，未参考 Go 实现。
- 审查修复的证据：`TestService_ForkChildStartsOutsidePlanMode`（`internal/app/subagent`）走真实 engine、registry、JSONL 和 fork 种子：root 进入规划模式并完成一个 turn，在第二个 turn 中 fork；修复前 child 的请求 system 含 Base 规划段落而失败，修复后不含，同时断言 child surface 仍含 parent 的消息、种子仍携带 parent 的 `plan/mode`，以及 parent 的请求与折叠保持规划模式。`TestProjectPlan_SkipsRecordsAForkInherited` 在 core 层覆盖继承前缀、child 自己的请求头和 parent 视图。`TestService_SessionsDoNotWaitForEachOther` 让一个会话的日志读取停住，另一个会话的选择与边界必须完成；换回全局锁实现时它在 10 s 后失败。修复基于集成分支 `7ffe646`，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出码 0（lint 0 issues，逐文件 100.0%，36 个 mutation 全部 killed）。
- 未验证：没有 live provider 调用；Linux `bwrap` 下的 PTY 流程未在本机运行。

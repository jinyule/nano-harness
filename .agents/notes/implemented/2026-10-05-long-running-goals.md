# 长期目标、goal/change 与自动轮次 driver

- Status: implemented
- Date: 2026-10-05

## Context

这是[工具对齐计划](2026-10-04-upstream-tool-parity.md)的 WP10。产品没有长期目标，也没有在空闲时自主续跑的能力。上游参考提交 `5badb15009ae` 的 Base 组合挂载 `dsh-goal`、`dsh-tool-goal`、`dsh-goal-round-driver` 与 `dsh-command-goal`。调查结论（细节见 ADR-0016 背景）：

- 状态只存于日志：`goal/change` 完整快照与 clear tombstone，准入轮次是带 `{goalId, revision, round}` 的 goal 来源 `user/message`，严格 fold 校验迁移与连续性；armed/disarmed 只在进程内，session 开启边沿一律解除。
- 工具权限在执行点判定：create/edit/pause/resume 需要运行时 root 当前 turn 中的 `{kind:'user'}` 消息；complete/blocked 另接受确切当前轮次，自主 blocked 至少 3 轮；自主终结后延迟注入收尾指令。
- driver 在整个 agent 空闲时预约并 followup `<goal_round>`，以 pre-step 栅栏拒绝陈旧预约；`round-limit` 阻塞；被取消的轮次暂停，无关取消只解除；人类 pause 中止 turn。
- 本仓差异：turn 的开场消息在第一个 step 前提交，没有 pre-step 钩子；工具的 `Invocation.Journal` 只能追加；agent 订阅在背压下可丢弃。

非目标：独立评估器、资源预算、多目标、`/goal` 附件、每命令上限参数、异常自动重试。

## Decision

长期契约在 [ADR-0016](../../../docs/decisions/0016-long-running-goals.md)，当前事实归[架构](../../../docs/architecture.md#长期目标)、[安全](../../../docs/security.md#长期目标)和[测试](../../../docs/testing.md)文档。本次实施：

- `internal/core/session`：`goal/change` 记录、`GoalSnapshot`/`GoalChange`、`MessageSource` 的 `goal_id`/`goal_revision`/`goal_round`，形状校验与严格折叠 `GoalState.Apply`/`ProjectGoal`（拒绝时状态不变）；`CloneEvent` 深拷贝目标负载。`jsonl/order.go` 对每个事件执行同一折叠，`goal/change` 位置不受 turn/step 约束。
- `internal/app/agent`：`Journal` 接口与 `Registry.Journal`；`Engine.RegisterAdmission(kind, Admission, scope)`，`runTurn` 通过 `openTurn` 提交开场记录；被拒绝的 turn 返回 `ErrNotAdmitted`、不写记录、不覆盖 `Status().Last`。新代码放在 `admission.go`，`engine.go` 与 `agent.go` 只做局部替换，减少与 WP6/WP7 的冲突。
- `internal/app/goal`：`Service`（插件 `goals`）持有一把锁串行化变更与轮次 admission，`Settle` 按读取时的 revision 条件应用结果；上游错误码与文本；`Authority(sessionID, turn, delegated)` 读取调用方 turn 的已提交消息判定人类或轮次权限；`Watch` 由 scope 撤回且撤回后不再回调。`Driver`（插件 `goal-driver`）按 ADR 的五步循环运行。`prompt.go` 持有上游原文的轮次提示与收尾指令，`Quote` 按 `JSON.stringify` 规则引用（不转义 `<>&` 与 U+2028/U+2029）。
- `internal/adapter/tool/goal`（`goal-tools`）：三个工具与上游逐字节一致，`update_goal` 携带 `tool:goal` 段落（新增 `appTool.OrderGoal = 2400`）；自主 complete/blocked 经 `Registry.Notify` 投递收尾指令。
- TUI：`/goal` 上游语法与渲染、`Config.Goals`、状态栏 `goal=<阶段> <轮次>/<上限>`、`goal>` 转录行；待发送图片时拒绝 `/goal`。
- composition：`goals` 在 subagents 之后、`goal-tools` 在 plan tools 之后、`goal-driver` 最后；composition ID 使用 `goal-tools-v2`（停止语义见[后续修复](2026-10-06-goal-stop-outcomes.md)）；两份 fixture 增加三个工具，`upstream-base-tools.json` 增加 `tool:goal` 段落；上游工具数量断言为 23（含 WP6/WP7 的工具）；原 `upstreamPlanSection` 改为按名称查找的 `upstreamSection`。
- mutation 新增 `goal-direct-human` 与 `goal-round-revision`。守卫测试 `TestHumanSource_OnlyFrontendsAttributeHumanInput`（`internal/app/goal`）用 `go/parser` 扫描全部非测试产品源码，只允许 `internal/adapter/tui` 与 `internal/adapter/media/image` 把来源 `Kind` 设为 `"user"` 或 `HumanSource`。PTY 脚本加入 `/goal` 创建 → driver 第 1 轮 `get_goal` → `update_goal complete` → 收尾指令回复的流程。

关键取舍：

- 用开场 admission 取代上游 pre-step 栅栏，陈旧轮次从不进入日志；拒绝原因无法由新 revision 或撤销解释时 driver 以 `prompt-rejected` 阻塞，避免空转。
- 人类权限依赖 `source.kind = "user"` 只由前端使用这一不变量；本仓所有非人类生产者都已有独立来源（`tool-jobs`、`plan-mode`、`skill-catalog`、`skill-invocation`、`delegation`、`agent-message`、`subagent-settled`、`goal`、`tool-goal`），工具测试覆盖后台通知与规划提示开启的 turn 被拒绝，守卫测试阻止新生产者借用 `user`。
- 目标只从 session 自己的事件（`session.OwnEvents`）折叠：整体审查发现 fork 子代理的 `get_goal` 会读到种子前缀中的父目标（S3）。修复后 `Get`、变更、准入、权限与结算都只看自有事件，JSONL 仍在原位校验并接受种子中的目标事实；永久测试 `TestService_ForkedChildOwnsOnlyItsOwnGoal` 在修复前失败，`TestOpen_ForkSeedKeepsTheParentGoalInPlace` 证明种子仍被接受。
- driver 只等待 root 自身空闲（审查项 S4）：上游检查该 agent 的 `status`，`whenIdle()` 的 whole-agent 不含后代；驻留子代理的结算或消息经 `Notify` 唤醒 root。理由写在 ADR-0016。
- driver 在空闲时从日志 `Settle`，而不是订阅事件流：订阅可丢弃，日志顺序还能准确处理“取消之后又 resume”。
- 收尾指令走既有 `Notify`，位置在 `step/end` 之后，模型可见内容与上游相同。
- `/goal` 附件暂缓，因为无法保证附件消息先于 driver 的第一轮。

停止原因与结算交错的实施证据由[目标停止修复](2026-10-06-goal-stop-outcomes.md)补充；本 Note 仍记录工具、权限、轮次与持久化目标的初始实现。

空白、Unicode 边界与交互补充的实施证据见[交互与会话状态对齐](2026-10-06-interaction-state-upstream-alignment.md)；本 Note 保留各能力的初始组装、生命周期和持久化决定。

模型 pause 不中断的 driver 单元测试的偶发失败调查与屏障修复见[模型 pause 测试偶发失败](2026-10-08-goal-model-pause-flake.md)；driver 行为与本 Note 的决定不变。

## Consequences

模型看到的工具定义、`tool:goal` 段落、轮次提示、收尾指令、结果 JSON 和错误文本与上游一致；目标事实可从日志重建，恢复后目标保留且 disarmed。轮次是普通 turn，规划模式、approval 与打断不需要专门分支。

代价与风险：

- 目标服务每次操作与准入都折叠整份事件快照；engine 多一次 admission 查找。
- 人类 pause 会中断任何正在运行的 turn；轮次上限默认 256，不计量 token。
- 进程内组件仍可伪造 `goal/change`；折叠只做完整性检测。
- 与其他工作包的冲突热点：`session` 记录穷举列表与 `validate.go`、`order.go`、`engine.go` 的 `runTurn` 开头与 `Engine` 字段、`define.go` 的 Order 常量、`application.go`/`main.go`/`main_test.go`/`plan_test.go`、两份 fixture、TUI 的 `Config`/`actions.go`/`transcript.go`、`tui-e2e.py`、`mutation-cases.json` 与 `docs/testing.md` 的 mutation 数量。已 rebase 到集成分支 `00803e7`（WP6、WP2、WP7 之后）：cmd 测试补上 `spillRoot` 与 skill 目录，composition 与 WP2/WP6/WP7 的 token 合并，TUI 转录同时保留 `skill>`、`agent>` 与 `goal>`。

重新评估条件见 ADR-0016。

## Verification

- 审查修复（S3）：`go test -race -count=1 -run ForkedChild ./internal/app/goal/` 在撤回 `ownEvents` 时失败、修复后通过；`make check` 再次通过。
- `go test -race -count=1 ./...`：通过；`go test -race -count=10 ./internal/app/goal/` 与 `-count=8` 的目标 assembled 测试稳定通过。
- `make coverage`：每个产品源文件 100.0%。
- `golangci-lint run ./...`（私有 `GOLANGCI_LINT_CACHE`）：0 issues。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0（fmt、mod、vet、race test、architecture、submodule、agent-notes、skills、workflow-tools、lint、coverage、mutation、build）。
- `make mutation`：十九个用例全部 killed，包括新增的 `goal-direct-human` 与 `goal-round-revision`。
- `make tui-e2e`：真实二进制与 PTY 通过。根会话 18 次工具调用（除 WP7 场景中预期失败的 `send_message`）成功；`/goal PTY_GOAL ship it` 显示 `goal> Goal created`，driver 开启 `goal> round 1`，模型 `get_goal` 后 `update_goal complete`，终端显示 `goal> <goal_complete>` 与模型回复，`/goal` 显示 `Status: complete`，状态栏出现 `goal=complete 1/256`；日志中 `goal/change` 为 create 与 complete（均为 turn 0），恰有一条第 1 轮 goal 来源消息和一条 `tool-goal` 收尾消息，fixture 记录的 system prompt 含 `tool:goal` 段落。
- assembled 测试（`cmd/nano-harness/goal_test.go`）复用 `composeApplication`：模型在人类 turn 中创建目标后 driver 自动运行第 1 轮并由轮次 complete，下一请求含收尾指令且 driver 停止，每个请求含 `tool:goal` 原文；轮次中的 pause 与过早 blocked 返回上游错误文本，driver 以 `round-limit` 阻塞，人类 turn 提高上限并 resume 后继续到新上限；人类 pause 中断在途轮次（turn canceled、目标保持 paused 不被重复暂停），resume 后继续，关闭中断在途轮次，新 composition 恢复同一会话时目标 active、disarmed、轮次 3，编辑并 resume 后继续。断言来自磁盘 transcript 与 provider 请求。
- 持久化固定样本 `session-v2-goal.jsonl`：读取、多前缀投影、只读恢复不改字节，独立构造的 writer 输出逐字节相同；二十个篡改反例全部被拒绝；陈旧轮次的追加被拒绝且文件不变；中断尾部修复不改动目标。
- 守卫反例：分别在 `internal/app/job` 加 `session.MessageSource{Kind: "user"}`、在 `internal/app/subagent` 加 `message.Source.Kind = "user"`、在 `internal/adapter/tool/goal` 加 `Kind: appGoal.HumanSource`，守卫测试各自失败并列出越界位置；还原后通过。
- 门禁反例：把 `upstream-base-tools.json` 中 `update_goal` 描述改一个词、把 `tool:goal` 段落删去一个逗号，`TestComposition_MatchesUpstreamBaseTools` 与目标 assembled 测试失败；恢复后通过。
- 三个工具定义取自 submodule 的 `docs/tool-catalog.md` 生成版，轮次提示与收尾指令用脚本从上游源码拼接后与 Go 常量比对一致。
- 未验证：没有 live provider 调用；Linux `bwrap` 下的 PTY 流程未在本机运行。

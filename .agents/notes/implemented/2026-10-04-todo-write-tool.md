# 增加 todo_write 工具、todo/write 记录与 TUI 计划面板

- Status: implemented
- Date: 2026-10-04

## Context

内置工具对齐上游 Base 工具集的总体计划（`proposed/2026-10-04-upstream-tool-parity.md`）把 `todo_write` 列为 WP4。产品此前没有任务列表能力，模型无法记录多步计划，界面也无法显示进度。

上游参考提交 `5badb15009ae` 的 `packages/todo/tool-todo` 与生成工具目录给出行为：模型每次提交完整列表并替换旧列表；列表写入调用方 session 的 `todo/write` 快照，不进入模型 surface；Base 组合设置 `allowParallelInProgress: true`；结果文案固定；空内容、重复内容和非 agent 调用者失败；界面显示最新快照，下一次 `turn/start` 清除，`turn/end` 保留。上游 compaction 与 subagent 不对清单做特殊处理，每个 agent session 各有一份列表。

工具定义抽象和执行上下文由 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md) 与 [上游工具定义 Note](2026-10-04-upstream-tool-definitions.md) 拥有：`tool.Spec[A]` 声明 schema 与类型化参数，runtime 统一校验参数并以 `Error: <message>` 返回失败，`tool.Invocation` 携带调用方 journal、turn、step 和 call ID。session v2 decoder 严格拒绝未知记录，新增记录类型需要按根规则写 ADR。

非目标：单一 `in_progress` 策略变体、跨 agent 共享列表、读回工具，以及在 compaction 或系统提示中注入清单。

重叠审计：上游工具定义 Note 继续拥有定义抽象、schema 校验和固定样本机制，本记录只向其固定样本加入 `todo_write`；[核心 harness Note](2026-08-24-core-agent-harness.md) 继续拥有 session v2、工具调度与 composition 的原始设计；[TUI v2 Note](2026-10-03-tui-v2-frontend-plugins.md) 继续拥有前端插件与生命周期；[工程证据 Note](2026-10-04-engineering-evidence-gates.md) 拥有固定 v2 样本与 mutation 规则。本记录只拥有 todo 能力，不归档上述记录。

## Decision

边界校验与取消语义的补充实施见[修复 Note](2026-10-06-spill-question-and-call-validation.md)；本 Note 保留能力建立、生命周期和原始验证证据。

长期决定见 [ADR-0010](../../../docs/decisions/0010-todo-write-session-record.md)，本节记录实施位置。

- **执行上下文**：工具使用 `tool.Invocation` 的 `Journal`、`Turn`、`Step` 和 `CallID`。engine 为每个 agent 传入自己的 journal，所以 root 与 subagent 的写入落到各自 session；engine 测试断言这四个值来自调用方。
- **领域与持久化**：`internal/core/session/todo.go` 定义 `TodoStatus`、`TodoItem`、`TodoWrite`、上限 256 项与 2048 字节、共享校验 `ValidateTodoItems` 和投影 `StandingTodos`。`Record` 增加 `Todo *TodoWrite`（JSON `todo`）。`validate.go`、`surface.go` 接入新类型，`CloneEvent` 深拷贝条目。JSONL `order.go` 要求记录位于活动 step、引用 pending call、每个 call 最多一条，且 result 之后不能写入。format 保持 v2。
- **工具插件**：`internal/adapter/tool/todo` 的 `Provider`（ID `todo-tools`）在 `Start` 中把 `tool.Define` 编译的 `todo_write` 注册到 tool runtime，唯一 effect 是 runtime 通过 `Scope.Defer` 登记的注销。schema 用 `Array`/`Object`/`String` 构造器表达，参数违规由 runtime 统一报告；不声明 `Concurrent` 与 `Approval`，因此调度为 exclusive 且不需要 approval。执行顺序与上游相同：去空白并校验列表，再检查 journal，然后在调用方 context 下追加 `todo/write`，最后返回计数文案；列表错误沿用上游文案。上游该工具没有 prompt guidance，因此不增加 guidance order。
- **组合**：`cmd/nano-harness` 在 subagent 工具之后组装 `todo-tools`，composition ID 增加 `todo-tools-v1`。`todo_write` 加入 `testdata/upstream-base-tools.json`（上游同名工具由 6 个变为 7 个）和组合工具目录 `testdata/tool-catalog.json`。
- **TUI**：`plan.go` 用 `StandingTodos` 折叠初始 replay 与实时事件，计划面板固定在输入区上方。面板最多占 transcript 剩余行数的一半，并保留至少一行 transcript；溢出时从第一个未完成项开始显示，最后一行给出隐藏数量，标题保留各状态计数。窗口变化和计划变化都通过同一 `layout` 重新分配行数。

空白、Unicode 边界与交互补充的实施证据见[交互与会话状态对齐](2026-10-06-interaction-state-upstream-alignment.md)；本 Note 保留各能力的初始组装、生命周期和持久化决定。

## Consequences

模型获得与上游一致的计划工具，计划随会话持久化，恢复后可见，且 root 与每个 subagent 相互隔离。界面只从已提交事件渲染，不另设状态源。

composition ID 加入 `todo-tools-v1` 之前创建的会话不能恢复；当前没有发布 tag，这一点由 ADR-0010 记录。nano 的条目数和长度上限是上游没有的模型可见限制。与本仓其他工具一样，根对象的未声明成员被拒绝，而上游会忽略它们；模型可见 schema 不变。

`scripts/tui-e2e.py` 的根工具序列以 `todo_write` 开始，后续结果断言的下标相应后移。没有新增 mutation case：todo 顺序规则的关键判断已用临时变异验证（见下文），且 `session-causality` 已覆盖 order validator 的定向变异机制。

重新评估条件：上游改变 `todo_write` 定义或结果文案、产品需要单一 `in_progress` 策略或共享列表、第二个前端需要同一投影。

## Verification

- `go test -race -count=1 ./internal/core/session/ ./internal/adapter/session/jsonl/ ./internal/adapter/tool/todo/ ./internal/app/tool/ ./internal/app/agent/ ./internal/adapter/tui/ ./cmd/...` 通过。覆盖内容：有效/非法快照与上限、模型可见错误文案、`StandingTodos` 的替换/保留/清除与非别名、`todo/write` 不进入 surface；固定样本 `testdata/session-v2-todo.jsonl` 的读取、投影、resume 不改字节与 writer 字节比较，以及十种篡改被拒绝；order 规则的七种非法位置与 call ID 复用；中断修复后计划仍可投影、下一 turn 清除。
- `TestComposition_ToolCatalogGolden` 与 `TestComposition_MatchesUpstreamBaseTools` 从真实 composition 的 request header 和 provider wire 比较 `todo_write`；加入固定样本的描述和参数先用 Python 按属性顺序与上游 `docs/tool-catalog.md` 比较，结果为 `True`。工具测试经真实 runtime 覆盖 9 种参数与列表错误（含根对象未知成员）、无 journal、追加失败、取消，以及同一 batch 内按 call 顺序写入。
- `cmd/nano-harness/todo_test.go` 经真实 composition 与 loopback Responses SSE 调用 `todo_write`，从磁盘断言 request header schema、`todo/write` 与结果文案，再以新 composition 恢复并从 replay 得到同一计划；另一测试证明 composition ID 不等于同一组合去掉 `todo-tools-v1` 后的值；临时删除该 token 时此测试失败。
- TUI 测试覆盖初始 replay、实时替换、空列表、下一 turn 清除，以及 18×8、40×12、80×24、30×6、30×5 窗口中的宽高、transcript 保留行与活动项可见性。
- `make tui-e2e` 在 macOS/arm64 通过：真实 binary/PTY 显示 `plan> 1 in progress · 1 pending` 与 `[>] inspect workspace`，根会话 12 种工具调用（`todo_write` 在 WP1 的 11 种之前）、结果文案和 `todo/write` 快照正确，重启 replay 不显示已被后续 turn 清除的计划。在 rebase 到工具定义抽象之前，把 `StandingTodos` 临时改为在 `turn/start` 保留计划后，同一命令以 `a later turn/start must clear the replayed plan` 失败；恢复后重新构建。
- 临时删除 `order.go` 中 pending call 检查时，`TestSessionV2Todo_RejectsChangedContract`、`TestValidateOrder_BindsTodoWriteToOnePendingCall` 和 `TestLog_TodoWriteSurvivesAppendResumeAndInterruptedRepair` 失败；临时删除同一 call 的重复写入检查时，`TestValidateOrder_BindsTodoWriteToOnePendingCall` 失败。两处均已恢复。
- 基于集成分支 `4eaa093`（含 ripgrep 版 glob/grep）运行 `make check` 与 `make tui-e2e` 均通过。`make check` 覆盖：格式、tidy、vet、全仓 race tests、架构、submodule（`5badb15009ae`，干净）、Agent Note 格式、skills、workflow tools、lint 0 issues、每个产品文件 100.0% coverage、八个 mutation 全部 killed、binary build/version。多个 worktree 共用默认 golangci-lint 缓存时，lint 曾报告另一 worktree 中的文件位置；设置私有 `GOLANGCI_LINT_CACHE` 后同一命令为 0 issues，门禁本身未改动。
- 未执行：live provider 验证（工具不改变 provider wire）、其他 OS 的原生 PTY 验证，以及 `make ci` 的漏洞与发布检查（依赖和发布配置未变）。

# WP12：结构化工具结果实施计划

- Status: proposed
- Date: 2026-10-07

## Context

维护者已确定：先补数据，TUI 卡片暂缓；模型可见文本不变；上游的错误分类 `{name, code}` 和结果 meta 持久化到 tool/result。协调者在 2026-10-07 按“对齐上游”确认了 [ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 的取舍：只存 `{name, code}`；普通错误不带 `error`，也不新造码；接受 read 不存 `lang`、write 的 diff 至多一个 hunk 这两处差异，等 UI 卡片工作开始时再复审；按批次合入。持久化契约、错误映射表、24 个工具的 meta 取舍、上限和版本策略由 ADR-0019 拥有；本 Note 只拥有分批、冲突面、验收证据和工作量。调查基线是 `feat/upstream-tool-parity` 的 `52d3715`，只读上游是 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`。

直接证据：`internal/app/tool` 把类型化错误渲染成文本后丢掉了类别；`session.ToolResult` 只有正文、`is_error` 和图片引用。web、goal、subagent 已有 Code；文件、搜索和提问的错误只有文案。上游只有 read、read_image、write、edit、glob、grep、web_search、web_fetch 有 presentationMeta，其他工具的 canonical output 不持久化。

### 其他分支对设计的影响

分支标签（K1、K2、审计 3、B1–B5、WP13、WP14）的定义见[上游工具总计划](2026-10-04-upstream-tool-parity.md)。

| 分支 | 状态 | 对 WP12 的影响 |
|---|---|---|
| K1 subagent 正确性 `1a3568a` | 已合入 | runtime 有三个取消检查点。Execute 返回后发现取消时，本仓连错误结果也替换为 `tool call aborted`；上游两处替换都以 `!isError` 为前提，错误保留原文本和分类。这一偏差由 WP12 之前的批次 R 修复。修复后文件的 `FS_ABORTED`、搜索和 web 的调用方取消、`ASK_ABORTED`、bash 的 `tool call aborted` 都能到达工具结果，ADR 已为它们写入映射。K1 给 `app/subagent` 增加的 `ABORTED`、`ABORTED_BEFORE_DISPATCH`、`ACTIVATION_TEARDOWN_FAILED` 经 `SubagentError` 原样持久化。 |
| 审计 3 shell 修复 `cd6912b`、B5 `2d519a0`、shell 文案对齐 `7667d02` | 已合入 | runner 缺失或失败返回 `process.ErrSandboxUnavailable`，文本已是上游 `SandboxUnavailableError` 原文。WP12 在 shell adapter 将其分类为 `SandboxUnavailableError/SANDBOX_UNAVAILABLE`；platform 不依赖领域层，所以分类不放在 runner。后台 runner 失败只是 failed 状态。B5 的交接前取消返回 bash 自己的 `tool call aborted`，按上游 tool-bash 分类为 `AbortError/ABORTED`。 |
| K2 subagent 设计项 `576547a` | 已合入 | K2 没有修改 ToolResult。消息 source 的 `sender_session_id` 与 tool/result 无关，不复制进 error。新的 `subagent delegation requires a parent request to inherit its route from` 使用已有的 `INVALID_REQUEST`，由 `SubagentError` 自动覆盖。subagent token 已是 v4。 |
| WP13 + B3 `57b56a9` | 已合入 | `compaction/prune` 的 `applyPrune` 只替换 surface 节点的 `Result.Output`，error/meta 自然保留；基础批用测试固定这一点。token 增加了 `tool-result-prune-v1`。 |
| B1 `27ad50b`、plan cleanup `005a3a8`、cleanup 静止 `f0b343e` | 已合入 | goal 批和 question 批不再有进行中的文本冲突。 |
| WP14 会话 sandbox 模式 | 进行中（`wp14-sandbox`） | 修改文件四工具、glob/grep、bash、job provider、workspace sandbox、`jsonl.go`/`order.go`；提升 fs 与 shell 并新增 `sandbox-policy-v1`。`FS_SANDBOX_DENIED` 和 shell 拒绝的具体条件要在它合入后重新核对。 |
| A1/A2 `4f4c193` | 已合入 | 修改 agent 包的开场与通知。基础批不改 `app/agent`，只依赖 engine 原样追加结果的路径。 |

重叠审计：[上游工具总计划](2026-10-04-upstream-tool-parity.md)继续拥有工作包安排；[runtime 对齐记录](../implemented/2026-10-06-tool-runtime-upstream-alignment.md)拥有参数预算和并发调度；[subagent 正确性记录](../implemented/2026-10-06-subagent-correctness.md)拥有取消检查点；[spill/问题校验记录](../implemented/2026-10-06-spill-question-and-call-validation.md)拥有既有修复；[搜索/spill 对齐记录](../implemented/2026-10-06-search-spill-query-parity.md)拥有正文、query 和历史读回；[图片存储记录](../implemented/2026-10-06-content-addressed-image-attachments.md)拥有附件路径。WP12 不取代这些已实施契约，也不归档旧 Note。本提交只新增 ADR-0019 和本 Note；architecture、security、testing 和既有 ADR 继续描述本分支已有的行为，由各批在实施时同步。

## Decision

### 分批与合入顺序

先做取消对齐 R，再做基础批 F，然后并行做 producer 批，最后做收尾批 K。协调者把 opus 并发限制为 3，本 Note 的作者串行完成 R 和 F。每批是独立的分支和提交，从合入时的集成分支 HEAD 开始，按协调者的合入协议 rebase、跑门禁并 ff 合入。每批提升自己负责的 composition token，中间状态因此都自洽；不能把本设计基线的 token 串覆盖到合入后的 cmd。jobs、todo、skill 自身没有新分类，也没有 meta，不单独成批。

| 批次 | 范围 | 开始条件 | 与其他分支的冲突面 | token | 估算 |
|---|---|---|---|---|---|
| R 取消对齐 `wp/wp12-runtime-error-cancel` | `app/tool/runtime.go`：Execute 返回错误时保留错误，只有成功结果在取消后替换为 `tool call aborted`；修复前失败的测试、mutation；architecture 与所属 ADR；受影响的工具测试与 tui-e2e | 现在 | 无 | 不变：只改变被取消调用的结果文本，与 K1 引入该行为时相同 | 2–4 h |
| F 基础 `wp/wp12-base` | `core/session`：`ToolError`、`ToolMeta` 与 8 种 DTO、`Validate`、裁剪、深复制；`jsonl/order.go` 的工具名关联；`jsonl.go` resume 修复写 `TOOL_OUTCOME_UNKNOWN`；`app/tool`：`Failure`、`Result.Meta`、runtime 自有分类（`UNKNOWN_TOOL`、`INVALID_ARGS`、`ABORTED_BEFORE_DISPATCH`、`ABORTED`、`INVALID_TOOL_OUTPUT`）与归一出口；手写样本；architecture/testing 的 tool/result 段落 | R 合入；不等 WP14 | WP14（`jsonl.go`、`order.go`）；token 行 | `tool-runtime` +1 | 12–16 h |
| E web | `app/web.Error.ToolError`；web_search/web_fetch meta；`formatFetch` 一次返回文本与实际截断 | F 合入 | 无 | web +1 | 3–4 h |
| H goal | `app/goal.Error`、goal 工具 `toolError` 的 `ToolError` | F 合入 | 无 | goal +1 | 1–2 h |
| I question | `app/question` 哨兵改为带分类的值，`RequestError` 中 `EMPTY_QUESTIONS`/`BAD_INTENT` 两类加分类；plan 不改代码，只提升 token | F 合入 | 无 | question +1、plan +1 | 2–3 h |
| G subagent | `app/subagent.Error.ToolError` | F 合入 | 无 | subagent +1 | 1–2 h |
| C 文件 | `adapter/tool/file`（含 workspace 错误归类）的 `FsError` 包装；read、read_image、write、edit meta；write 在锁内同一次读取中保留小于 10 MiB 的旧内容；按 ADR 规则生成 hunk | F 与 WP14 合入 | WP14（文件四工具、workspace sandbox） | fs +1 | 10–14 h |
| D 搜索 | `adapter/tool/search` 的 `SearchError` 包装；glob/grep meta | F 与 WP14 合入 | WP14（glob.go、grep.go） | search +1 | 4–6 h |
| S shell | `adapter/tool/shell` 的 `SANDBOX_UNAVAILABLE` 与 `AbortError/ABORTED` 分类 | F 与 WP14 合入 | WP14（bash.go） | shell +1 | 1–2 h |
| K 收尾 | 跨 provider 的 wire、token 估算和 compaction 摘要输入排除 error/meta 的测试；跨批 assembled 场景；security/reference 文档；ADR-0019 与实际 token 同步；本 Note 移至 implemented | 以上全部合入 | 只有文档与测试 | 无 | 5–7 h |

实施进度：R 已实施，见[取消只替换成功结果](../implemented/2026-10-07-tool-cancel-keeps-body-failure.md)；F 已实施，见[基础批记录](../implemented/2026-10-07-structured-tool-results-base.md)，`tool-runtime` 为 v3。

F 合入后，E、H、I、G 可以立即开始；C、D、S 等 WP14。估算按一名熟悉本仓的工程师在干净基线上工作计算，包含永久测试和逐文件 coverage，不包含等待合并、TUI 卡片和数据迁移。合计 **41–60 工时，约 5–8 个工作日**。主要不确定性是 write 的旧内容读取与 hunk 生成、WP14 合入后的文件和 sandbox 错误路径，以及严格校验对既有样本的影响。

### 每批的验收证据

每批的 owning package 测试先写独立预期：在没有结构化字段的代码上，磁盘结果的分类或 meta 断言必须失败。预期值不从正文反解析。审批、取消、发布和 cleanup 的交错用 channel/barrier 固定，不用 sleep。持久化可见的变化在同批提供真实 cmd 或 golden 证据。

| 批次 | 必须证明的结果 |
|---|---|
| R | 修复前失败的测试：Execute 返回错误后调用才被取消（用 barrier 固定），结果保留该错误文本；Execute 成功后取消仍替换为 `tool call aborted`；两个调度前检查点不变。受影响的工具测试改为断言领域错误文本，例如 todo 追加被取消、job_output 等待被取消。mutation 恢复旧的检查顺序后被具名测试拒绝。`make tui-e2e` 通过。 |
| F | 手写样本包含 typed error、8 种 meta 和带图片引用的成功结果，原样 reopen；负例覆盖未知成员、多余成员、error 出现在成功结果上、meta 出现在错误结果上、恰好一个成员、行号与计数越界、上限及上限加一、工具名与 call 不符、`arguments_omitted` 带 meta；失败的 append 不改文件，也不通知订阅者。runtime 覆盖每条自有分类，包括调度前两个检查点、成功后取消丢弃 meta/图片、错误后取消保留领域分类、prepare 失败不受取消影响、`INVALID_TOOL_OUTPUT` 的固定文本；spill、spill 失败降级、无 store、KeepInline 和图片结果下 error/meta 不变；修改调用方持有的 meta 不影响日志、Surface 和订阅者；resume 修复写 `TOOL_OUTCOME_UNKNOWN` 且没有 meta。 |
| E | 现有每个 `WEB_*` 都带 `WebError`；sources 的去重与轮转顺序和正文一致；fetch 的 truncated 与正文截断标记一致，HTML 只转换一次；非 2xx 是成功结果；审计失败时没有 dispatch。 |
| H、G、I、S | 每个已有 Code 或映射的哨兵都写出 ADR 表中的 name/code；`errors.Is/As` 保留原因；没有映射的失败没有 `error`；exit_plan_mode 传播提问分类，dismiss、keep 和 feedback 没有分类且文本不变；前台 runner 失败为 `SANDBOX_UNAVAILABLE`，bash 前台等待或交接时取消为 `AbortError/ABORTED`，非零退出、信号和超时仍是成功文本。 |
| C | 未观察、陈旧、缺失、目录、二进制、超限、越界和 I/O 各自得到 ADR 中的码；审批等待期间外部修改文件，结果是 stale，没有写入，也没有 meta；发布前取消得到 `FS_ABORTED` 且目标不变；link/rename 提交后取消得到 `ABORTED`，文件保持已发布。从外部读取实际文件，独立核对 diff：BOM、CRLF、末尾换行、插入、删除、`replace_all`、create、内容相同的覆盖写、达到 10 MiB 和二进制旧文件。read 窗口、截行和空文件；read_image 只有 path 且图片仍只引用已提交的对象。旧文件的内存上限和取消点分别验证。 |
| D | 用真实 rg 验证 glob/grep 的顺序、0 结果、100/250 边界和 total；非法正则或 glob、信号、退出码、启动失败、输出畸形、原始输出超限和超时分别得到 `SEARCH_*`；单个超大项也不超过 65,536 字节；SaveText 降级和模型 footer 不变。 |
| K | loopback 模型驱动 8 种 meta 和各类错误，然后继续下一 step；从磁盘独立读取 workspace、spill、attachments 和 JSONL。OpenAI Responses/Codex、Anthropic、OpenRouter 的下一请求逐字比较，没有 error/meta；token 估算和 compaction 摘要请求同样排除它们；resume 与 fork 保留原始数据，被摘要遮蔽后原字节不变，历史 spill 仍可读。新 composition 拒绝旧会话且不改文件；当前版本能正常恢复，排除“全部拒绝”的假绿。 |

每批在 `scripts/mutation-cases.json` 增加定向变异，至少覆盖：恢复取消替换错误结果、删除 error 传播、spill 时丢 meta、跳过 meta 与 call 的关联校验、把 error/meta 带进模型请求。每项先在 baseline 通过，再在无缓存变异中被具名测试拒绝。上限门禁要同时有有效和无效输入，build error、超时或零测试不算拒绝证据。

实现沿用现有插件和真实 `composeApplication` 路径；新增的纯值和 helper 没有副作用。工具贡献继续经 Scope 撤销，jobs、subagent 和各项操作的取消与 join 沿用现有服务，关闭时先停 agent，再停工具和存储。不为错误转换另建 goroutine、缓存、服务定位器或无法回收的 registry。

## Consequences

设计先单独合入。基础批合入后，各 producer 批只改自己的 adapter 或领域包和一行 token；仍在进行的 WP14 只与文件、搜索、shell 三批和基础批的 JSONL 校验有冲突面。meta 只覆盖上游的 8 个生产者，分类只来自上游码和本仓已有 Code；不为上游的普通 Error 新造码，jobs、todo、skill 因此无需改动。模型可见文本、schema、图片和生命周期都需要保持不变的证据。

严格的 meta 预算有意允许部分数据，meta 不能用作文件恢复材料；diff 实现不能削弱观察校验，也不能在成功发布后制造 metadata-only 失败。error 和片段会增加 transcript 占用，需要遵守私有权限和保留边界。按批次提升 token 会在开发期间多次让本地会话失效；当前没有发布数据，首次发布前的升级义务由 ADR-0019 拥有。

本提交没有产品修复、Go coverage 变化或修复前失败的测试；以上用例和工时都是计划，不声称已经执行。ADR-0019 已确认，随本提交标为 Accepted；最后一批合入时，将本 Note 移至 implemented，改写为实际结果、验证和证据缺口。

## Verification

本轮只修改本 Note 和 ADR-0019，不修改 Go、既有 Note、主仓库、其他 worktree 或参考 submodule，也不推送。核对时使用了 nano-plugin-development、nano-agent-notes、nano-doc-standards 和 nano-prose-standard。

- 错误映射和 meta 字段逐项对照上游源码（`core/tools`、`agent-loop`、`core/session/repair`、`llm/error`、`fs/tool-fs`、`fs-local`、`tool-fs-search`、`tool-web`、`user-questions`、`ui-user-questions`、`plan-mode`、`sandbox`）和 `52d3715` 的本仓代码（`app/tool`、`core/session`、`adapter/session/jsonl`、各工具 adapter 与 `app/web`、`app/goal`、`app/subagent`、`app/question`）。
- 其他分支的影响来自集成分支上已合入的 `1a3568a`、`cd6912b`、`2d519a0`、`7667d02`、`576547a`、`57b56a9`，以及 wp14-sandbox 的工作树。上游的取消替换前提核对了 `core/tools/src/index.ts` 中 `callerCancelled(exec) && !…isError` 的两处判断。
- `AGENT_NOTE_BASE_REF=52d3715 make agent-notes`：格式有效，判定为 note-only change。`git diff --cached --check` 没有输出。两份文档的相对链接都指向现有文件。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 在 `52d3715` 加本次文档上通过，包括逐产品文件 100% coverage、定向 mutation 和真实 cmd build/smoke；它只证明设计提交没有破坏仓库基线。本轮不新增产品行为测试，不跑 live provider 或 TUI 验证。

实施阶段每批先跑 owning package 的最小 race、golden 和 assembled 用例，再跑一次 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`；改动结果持久化或请求链路的批次另跑 `make tui-e2e`，确认原前端文本和真实入口不变，并记录没有 live provider、其他操作系统和首次发布迁移的证据。

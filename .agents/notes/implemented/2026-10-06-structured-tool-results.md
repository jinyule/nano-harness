# WP12：结构化工具结果

- Status: implemented
- Date: 2026-10-07

## Context

维护者决定：先补数据，TUI 卡片暂缓；模型可见文本不变；上游的错误分类 `{name, code}` 和结果 meta 持久化到 tool/result。协调者在 2026-10-07 按“对齐上游”确认了 [ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 的取舍：只存 `{name, code}`；普通错误不带 `error`，不新造码；接受 read 不存 `lang`、write 的 diff 至多一个 hunk，等 UI 卡片工作开始时再复审；按批次合入。ADR-0019 拥有持久化契约、错误映射、24 个工具的 meta 取舍、上限与版本策略；本记录拥有分批结果、跨批验证和证据缺口。只读参考为 DeepSeek Harness `5badb15009ae1756c3afe0ae0cef1faafc290ccc`，WP12 开始时的集成基线为 `52d3715`。

WP12 之前，`internal/app/tool` 把类型化错误渲染成文本后丢掉了类别，`session.ToolResult` 只有正文、`is_error` 和图片引用；web、goal、subagent 已有 Code 但不落盘，读取窗口、搜索计数、实际差异与 web 来源返回正文后无法重建。上游只有 read、read_image、write、edit、glob、grep、web_search、web_fetch 有 presentationMeta。

分支标签（K1、审计 3、B1–B5、WP13、WP14）见[上游工具总计划](../proposed/2026-10-04-upstream-tool-parity.md)。重叠审计：[runtime 对齐记录](2026-10-06-tool-runtime-upstream-alignment.md)、[subagent 正确性记录](2026-10-06-subagent-correctness.md)、[spill/问题校验记录](2026-10-06-spill-question-and-call-validation.md)、[搜索/spill 对齐记录](2026-10-06-search-spill-query-parity.md)与[图片存储记录](2026-10-06-content-addressed-image-attachments.md)继续拥有各自已实施的契约；下表的批次记录拥有各自 producer 的细节与修复前证据。它们都是部分补充，保留并互链，不归档。

## Decision

WP12 按批次合入，每批是独立分支和提交，在合入时的集成分支上 rebase、通过门禁后 ff 合入，并只提升自己负责的 composition token。jobs、todo、skill 自身没有新分类和 meta，不单独成批，它们的 runtime 分类随 `tool-runtime` 一起变化。

| 批次 | 提交 | 结果 | 记录 |
|---|---|---|---|
| 设计 | `52a9a35` | ADR-0019 定为 Accepted | — |
| R 取消对齐 | `7c60a92` | Execute 返回错误时保留原文与分类，只有成功结果在取消后替换为 `tool call aborted`，与上游 `!isError` 前提一致；token 不变 | [取消只替换成功结果](2026-10-07-tool-cancel-keeps-body-failure.md) |
| F 基础 | `802fc42` | `core/session` 的 `ToolError`、`ToolMeta` 与 8 种 DTO、严格校验、硬预算裁剪与深复制；`tool.Failure` 与 `Result.Meta`；runtime 自有分类；resume 写 `TOOL_OUTCOME_UNKNOWN`；`tool-runtime-v3` | [基础批](2026-10-07-structured-tool-results-base.md) |
| D 搜索、S shell | `8dde776` | `SearchError` 四类码与 glob/grep meta；shell 的 `SANDBOX_UNAVAILABLE` 与工具自身取消的 `AbortError/ABORTED`；`search-tools-v4`、`shell-tools-v5` | [搜索与 shell](2026-10-06-structured-search-shell-results.md) |
| C 文件 | `fc0022f` | `FsError` 映射；read/read_image/write/edit meta，write 在锁内保留小于 10 MiB 的旧内容；`fs-tools-v5` | [文件](2026-10-06-structured-file-results.md) |
| G subagent | `69c8a5c` | `SubagentError` 与原 Code；`subagent-tools-v5` | [subagent](2026-10-07-subagent-structured-errors.md) |
| H goal、I question | `c3be702` | `GoalError`、goal 工具的 `HarnessError`、`UserQuestionError`；exit_plan_mode 传播提问分类；`goal-tools-v3`、`question-tools-v2`、`plan-tools-v2` | [goal 与 question](2026-10-07-structured-goal-question-results.md) |
| E web | `dc7aa31` | `WebError` 与 12 个代码；web_search/web_fetch meta，`formatFetch` 一次返回正文与实际截断；`web-tools-v3` | [web](2026-10-07-structured-web-results.md) |
| K 收尾 | 本提交 | 跨 provider wire、token 估算与 compaction 摘要输入的排除证据；跨批组装、compaction 与 resume 场景；fork 种子保留；security、参考分析与 ADR 的 token 列表 | 本记录 |

收尾后的 composition 身份为 `tool-runtime-v3`、`fs-tools-v5`、`search-tools-v4`、`shell-tools-v5`、`job-tools-v2`、`subagent-tools-v5`、`todo-tools-v1`、`web-tools-v3`、`question-tools-v2`、`plan-tools-v2`、`skill-tools-v1`、`goal-tools-v3`、`spill-v1`、`attachments-v1`、`tool-result-prune-v1`、`sandbox-policy-v2`、`session-v2`；会话格式仍为 v2，旧 composition 的会话在 Open 与 Inspect 中被拒绝，文件不动。所有新增类型都是纯值，没有新插件、goroutine、缓存、配置或服务定位器，插件启动顺序与 Scope 回收不变。

K 批不改产品代码，只补以下证据：

- provider：OpenAI Responses、Codex Responses、Anthropic Messages、OpenRouter Chat Completions 四种 wire 对带分类与 metadata 的 surface 和去掉它们的同一 surface 生成逐字相同的请求体，模型可见的正文、错误文本与图片仍在。
- compaction：`estimateSurface` 不计分类与 metadata；摘要请求经同一 provider 投影发送。
- 跨批组装：同一会话依次产生 glob、grep、read、edit 的 meta，以及 `FsError`、`SearchError`、`ToolNotFoundError` 分类；下一次聊天请求与手动 compaction 的摘要请求都不含它们；compaction 前后原始 tool/result 行逐字节不变；按 transcript header 的 composition 重新打开会话，重放的结果与磁盘一致，文件不变。
- fork：以结构化样本为种子创建子会话，所有继承行逐字节相同，子会话重放出相同的分类与 metadata。

## Consequences

磁盘上的失败带上游或本仓已有的稳定分类，8 个工具的展示数据可以从已提交结果重建，spill、prune、compaction、resume 与 fork 都不丢失它们；模型请求、token 估算与 compaction 摘要的字节不变。没有分类的失败（approval、panic、参数超限、jobs/todo/skill/plan 自身的错误、图片格式与路由）只能从 `is_error` 与正文判断，与上游相同。

代价是 transcript 复制部分正文（read 行、diff、匹配行、web 来源与回答），由 256 KiB 与 65,536 字节的硬预算限制，但不能保证 64 MiB 会话永不写满；metadata 可能比正文短，使用方必须尊重 `truncated`，不能当成恢复材料。按批次提升 token 在开发期间多次让本地会话失效；当前没有发布数据，首次发布前的升级义务仍由 ADR-0019 拥有。

暂缓事项：TUI 卡片；上游的 `reason`；read 的 `lang` 与 write 的多 hunk diff（UI 卡片工作开始时复审）；jobs、subagent 等工具的 canonical value。复审触发条件见 ADR-0019。

## Verification

各批的修复前失败、定向 race、coverage、mutation、`make check` 与 `make tui-e2e` 证据见上表的批次记录。K 批：

- 新增测试：`internal/adapter/model/provider` 的 `TestStream_OmitsToolErrorsAndMetadataInEachWireFormat`；`internal/app/compaction` 的 `TestEstimateSurface_IgnoresToolErrorsAndMetadata`；`internal/adapter/session/jsonl` 的 `TestOpen_SeedCopiesStructuredResults`；`cmd/nano-harness` 的 `TestComposition_StructuredResultsSurviveCompactionAndResume`，它用真实组装、真实 rg、loopback 模型和审批 broker，从磁盘、请求体和重新打开的日志独立断言，并确认第 9 个请求是 compaction 摘要请求。这些测试在现有实现上直接通过，它们固定的是 F 批以来的行为，而不是修复缺陷，所以没有修复前失败。
- 新增 3 个 mutation：Responses 与 Messages 把整个 `ToolResult` 当作结果内容发送、compaction 估算计入整个结果；用单独清单运行 `scripts/mutation-check.py` 全部被具名测试拒绝。
- 基于集成分支 `dc7aa31`（E 批合入后）：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括全仓 race、逐产品文件 100% coverage、架构、Agent Note、lint 0 issues、全部默认 mutation 与真实 cmd build/smoke；`make tui-e2e` 通过（真实二进制与 PTY 下 19 个 root 工具调用，含 sandbox 切换、中断、恢复与 cleanup）。变更 Markdown 的相对链接全部指向现有文件。

证据缺口：没有 live provider、真实公网检索或抓取、其他操作系统原生运行或 release 矩阵的证据。web metadata 的组装证据在 E 批的独立组装测试中，没有与文件和搜索放进同一会话；fork 的保留在 JSONL 种子层验证，没有经 `subagent_fork` 工具的组装运行；spill 与 metadata 的组合由 F 批 runtime 测试和 D 批搜索测试覆盖，K 批的组装场景没有触发 spill。首次发布的迁移与升级路径未实施。

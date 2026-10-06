# ADR-0019：结构化工具错误与结果 metadata

- 状态：Accepted
- 日期：2026-10-07
- 决策者：nano-harness maintainers（先补数据，TUI 卡片暂缓，模型可见文本不变；错误分类与差异取舍由协调者在 2026-10-07 确认，原则是对齐上游）
- 实施状态：按批次实施，进度与冲突面见[WP12 实施计划](../../.agents/notes/proposed/2026-10-06-structured-tool-results-plan.md)

## 背景

当前基线是 `feat/upstream-tool-parity` 的 `52d3715`，只读参考是 DeepSeek Harness `5badb15009ae1756c3afe0ae0cef1faafc290ccc`。

本仓的 [`tool.Result`](../../internal/app/tool/define.go) 只有 `Text` 和可选 `Image`；[`session.ToolResult`](../../internal/core/session/types.go) 只持久化 `call_id/output/is_error/image`。runtime 把 Go error 渲染成 `Error: <message>`，同时丢掉了类别：web、goal、subagent 已有的 Code 在这里消失。读取窗口、搜索计数、实际写入差异和 web 来源只在工具执行时存在，返回文本之后无法可靠重建。

K1（`1a3568a`）给 runtime 加了三个取消检查点：轮到调度时和即将进入 Execute 时发现取消，返回 `Error: tool call aborted before dispatch`；Execute 返回后发现调用已取消，返回 `Error: tool call aborted`。上游只在 Execute 成功时用取消结果替换（`index.ts` 中两处替换都以 `!isError` 为前提），Execute 返回的错误保留原文本和分类；本仓在 `52d3715` 连错误也一并替换。这一偏差由 WP12 之前的独立修复对齐，本 ADR 的分类以对齐后的行为为准。

约束来自[架构](../architecture.md)、[安全规则](../security.md)和[测试策略](../testing.md)：模型输入只能从成功提交的会话事实重建；严格解码、Scope 所有权、逐文件 100% coverage 和真实 cmd 证据都要保留。相关契约有[工具定义](0007-upstream-base-tool-definitions.md)、[spill 与观察策略](0008-tool-output-spill-and-observation-policy.md)、[后台任务](0009-background-jobs.md)、[子代理](0013-background-continuable-subagents.md)、[图片结果](0015-multimodal-tool-results.md)、[附件引用](0017-content-addressed-image-attachments.md)、[goal 停止结局](0018-goal-stop-outcomes.md)、[工具结果裁剪](0020-tool-result-pruning.md)和 [web 请求审计](0022-web-search-request-audit.md)。本 ADR 接受并实施后，拥有 tool/result 的 `error`/`meta` 契约和错误映射表；模型文本、权限、文件提交点和生命周期仍归上述 ADR。

### 上游调查

| 来源（均在上述参考提交） | 观察与本仓取舍 |
|---|---|
| [`core/tools/src/index.ts`](../../third_party/deepseek-harness/packages/core/tools/src/index.ts)：`ToolErrorInfo`、`errorInfo`、`toolErrorResult` | 失败是 `{message, info?}`，`info` 为 `{name, code, reason?}`。`errorInfo` 只从 `HarnessError` 取 `name/code`；普通 Error、approval 拒绝（`deny` 不带 info）和 panic 式异常都没有分类。`reason` 是可选的原始用户反馈，只有 experimental auto-review 产生；Base 工具不产生。 |
| [`llm/llm/src/error.ts`](../../third_party/deepseek-harness/packages/llm/llm/src/error.ts)：`HarnessError` | `name = new.target.name`。子类显式设名（`FsError`、`SearchError`、`WebError`、`GoalError`、`SubagentError`、`UserQuestionError`、`SandboxUnavailableError`、`ToolArgsError` 等）；直接 `new HarnessError(...)` 的 name 就是 `HarnessError`，例如 tool-goal 的 `GOAL_TOOL_*`。 |
| 同文件：`createSuccessResult`、`dispatchToolBody`、`toolAbortedResult` | `execute` 返回 canonical value，按 output schema 校验后分别运行 `render` 和可选 `presentationMeta`；value 不持久化，meta 只为顶层调用计算。进入 body 前取消得到 `AbortError/ABORTED_BEFORE_DISPATCH`；body 返回成功后发现取消，替换为 `AbortError/ABORTED`；body 返回的错误保留原分类。语义参数检查（如 `file_path must be a non-empty string`）是普通 Error，`ToolArgsError/INVALID_ARGS` 只来自 schema 校验。 |
| [`agent-loop/src/tool-calls.ts`](../../third_party/deepseek-harness/packages/core/agent-loop/src/tool-calls.ts)：`appendToolResult` | 模型消息只取 content/isError；`result.error.info` 原样写入事件的 `error`，meta 单独写入。 |
| [`core/session/src/repair.ts`](../../third_party/deepseek-harness/packages/core/session/src/repair.ts) | 恢复时，已有 tool/call 事件的未决调用记为 `ToolOutcomeUnknownError/TOOL_OUTCOME_UNKNOWN`，只出现在 assistant 消息中的记为 `ToolNotStartedError/TOOL_NOT_STARTED`。 |
| [`fs/tool-fs`](../../third_party/deepseek-harness/packages/fs/tool-fs/src/)：`read.ts`、`read-image.ts`、`write.ts`、`edit.ts`、`diff.ts`；[`fs/fs/src/types.ts`](../../third_party/deepseek-harness/packages/fs/fs/src/types.ts)；[`fs-local`](../../third_party/deepseek-harness/packages/fs/fs-local/src/index.ts) | read meta 是 `{path, offset, lines[{number,text}], totalLines, lang?}`；read_image 是 `{path}`；write 是 `{operation, diffs}`；edit 是 `{diffs}`；diff 项是 `{path, oldText: string|null, newText}`，每个 hunk 带三行上下文，纯插入时 `oldText` 为 null。diff 基础是 CRLF 归一为 LF 的文本；write 的旧内容只在新旧两侧都小于 10 MiB（`diffBasisMaxBytes`）且为合法 UTF-8 文本时读取，否则 `before` 为 null，`diffs` 为空。create 和内容相同的覆盖写的 `diffs` 也为空。图片的尺寸、像素、格式和路由错误是普通 Error，源字节上限来自 `readBytes` 的 `FS_TOO_LARGE`。 |
| [`fs/tool-fs-search/src/presentation.ts`](../../third_party/deepseek-harness/packages/fs/tool-fs-search/src/presentation.ts) | glob 是 `{shape:"paths", paths, truncated, total}`，grep 是 `{shape:"matches", files[{path, matches[{lineNumber,line}]}], truncated, total}`。meta 预算 65,536 字节，从尾部移除路径或文件组，至少保留一项，所以是软上限；额外裁剪并入同一个 `truncated`。 |
| [`web/tool-web`](../../third_party/deepseek-harness/packages/web/tool-web/src/)：`search.ts`、`fetch.ts` | search meta 是 `{sources[{url,title?,snippet?,publishedAt?}], truncated, answer?}`；fetch 是 `{url, statusCode, truncated}`，不复制正文，truncated 是渲染后输出的实际截断。 |
| [`interaction/user-questions`](../../third_party/deepseek-harness/packages/interaction/user-questions/src/index.ts)、[`ui-user-questions` 的 slots](../../third_party/deepseek-harness/packages/client/ui-user-questions/src/client/contract/slots.ts)、[`plan/plan-mode`](../../third_party/deepseek-harness/packages/plan/plan-mode/src/index.ts) | `UserQuestionError` 码有 `ASK_ABORTED`、`DELEGATED_CALLER`、`EMPTY_QUESTIONS`、`BAD_INTENT`、`NO_PROVIDER`、`BAD_ANSWER`，客户端拒绝另有 `ASK_CANCELLED`。plan review 被关闭、选择继续规划时是普通 Error，其他提问错误原样传播。 |
| [`sandbox/sandbox/src/index.ts`](../../third_party/deepseek-harness/packages/sandbox/sandbox/src/index.ts) | 受限模式没有可用 runner 时抛 `SandboxUnavailableError/SANDBOX_UNAVAILABLE`。 |
| jobs、todo、skill、bash 的 tool 包 | 有 output definition，但没有 presentationMeta；这些工具自身的错误是普通 Error，不存在 `JOB_*`、`TODO_*`、`SKILL_*` 一类上游码。 |
| [`spill/spill-policy`](../../third_party/deepseek-harness/packages/spill/spill-policy/src/index.ts)、[`core/session/src/surface.ts`](../../third_party/deepseek-harness/packages/core/session/src/surface.ts) | spill 和 surface 替换只改 content，保留 error 与 meta。 |

非目标：调整工具名称、参数 schema、guidance、模型可见文本、审批或取消语义；TUI 卡片；持久化完整 canonical value；新增插件、存储或部署配置；自动重试；WP13 的裁剪实现；旧会话迁移。实施分批、冲突面和工作量由 [WP12 实施计划](../../.agents/notes/proposed/2026-10-06-structured-tool-results-plan.md)拥有。

## 决策

本节契约按批次落地；尚未实施的批次不改变对应工具的产品行为。

### 1. 持久化字段与 Go 所有权

`session.ToolResult` 增加两个可选字段：

```json
{"call_id":"call_1","output":"Error: cannot read \"a.txt\": not found","is_error":true,"error":{"name":"FsError","code":"FS_NOT_FOUND"}}
{"call_id":"call_2","output":"<path>b.txt</path>…","is_error":false,"meta":{"read":{"path":"b.txt","offset":1,"lines":[{"number":1,"text":"hello"}],"total_lines":1,"truncated":false}}}
```

- `error` 是 `session.ToolError{Name, Code}`，JSON 为 `{name, code}`，对应上游 `ToolErrorInfo` 的身份部分。两者都是 1–64 字节、以字母开头的 ASCII 标识符（字母、数字、`_`）。上游可选的 `reason` 没有本仓生产者，暂不定义；以后需要时作为加法字段补上。
- `meta` 是 `session.ToolMeta`，一个只含一个成员的对象，键为工具名，值为该工具的闭合 DTO。字段名沿用本仓的 snake_case。工具名键取代上游 search meta 的 `shape`，并让每条记录不依赖调用记录也能解码。
- DTO、校验和裁剪与其他记录载荷（todo、goal、subagent、notice）一样放在 `internal/core/session`，都是纯值，没有注册、缓存、goroutine 或 Scope，不包装为插件，也不新建包。

`tool.Result` 增加 `Meta *session.ToolMeta`，工具在成功时填写；工具通过 Go error 声明失败，`tool.Result` 不带错误字段。`app/tool` 作为消费方定义最小接口：

```go
// Failure is an error whose tool/result carries a stable classification.
type Failure interface {
	error
	ToolError() session.ToolError
}
```

runtime 用 `errors.As` 取得分类，不导入具体工具或领域包。已有 Code 字段的错误类型（`app/web.Error`、`app/goal.Error`、`app/subagent.Error`、goal 工具的 `toolError`）增加 `ToolError` 方法；方法名不与现有 `Code` 字段冲突。question 的哨兵错误改为带分类的指针值，`errors.Is` 继续成立。文件和搜索 adapter 在错误产生处用未导出的包装类型补分类，`Error()` 保持原字节，`Unwrap` 保留原因。工具只填写类型化 DTO，不手写 JSON，不反解析 Text 或参数来猜结果。

### 2. Runtime 归一

每个调用只有一个归一出口，顺序如下：

1. 按现有路径得到结果：prepare 失败、取消检查点、Check、approval、Execute 的成功或错误。
2. 失败时：只有本节和第 3 节列出的路径写入 `error`；runtime 自有分类直接填写，Execute 和 Check 的错误经 `Failure` 提取，取不到就没有 `error`。`Failure` 给出的分类不符合标识符规则时，同样按第 3 步的 `INVALID_TOOL_OUTPUT` 处理，避免结果在追加时被拒。失败结果不带 meta 和图片。
3. 成功且带 meta 时：先检查 meta 的工具名键等于调用的工具名，再按第 5 节裁剪并校验。检查失败说明 producer 有缺陷，结果改为 `ToolOutputError/INVALID_TOOL_OUTPUT`，文本沿用上游格式 `Error: tool "<name>" returned invalid output: <violations>`，并丢弃图片和 meta。这条路径只防御实现缺陷，不经过外部输入。
4. 沿用现有的 UTF-8 归一、spill 和 256 KiB 截断，这些步骤只作用于 Output。
5. 返回 `session.ToolResult`，engine 原样追加，然后才发布投影。

取消按上游分类：调度前和 Execute 前的检查点写 `AbortError/ABORTED_BEFORE_DISPATCH`。Execute 成功返回后发现取消，结果替换为 `AbortError/ABORTED`，同时丢弃 meta 和图片。Execute 返回错误时，无论调用是否已取消，都保留该错误的文本和分类。prepare 已失败的调用（未知工具、参数错误、参数超限）不经过取消检查点，保留自己的分类。

producer 要在外部提交前完成可能失败的 meta 构造。write/edit 的旧内容和新内容在发布前都已知，diff 在 link/rename 之前算好；第 5 节的裁剪总能收敛，所以已发布的文件不会因为 metadata 变成失败结果。

### 3. 错误映射

只持久化两类分类：上游在同一条件下产生的 HarnessError 码，以及本仓错误已经携带的 Code。不为上游是普通 Error 的路径新造码。下表以 `52d3715` 加上第 2 节的取消对齐为准，未列出的失败没有 `error`。

| name / code | 本仓路径 |
|---|---|
| `ToolNotFoundError` / `UNKNOWN_TOOL` | runtime 中不存在或已撤销贡献的工具。 |
| `ToolArgsError` / `INVALID_ARGS` | prepare 的 schema 解码与校验失败，即文本以 `invalid arguments:` 开头的结果。工具 Check 中的语义检查是普通错误，不归这一类。 |
| `ToolOutputError` / `INVALID_TOOL_OUTPUT` | 第 2 节的 meta 契约违规。 |
| `AbortError` / `ABORTED_BEFORE_DISPATCH`、`ABORTED` | 第 2 节的取消检查点。bash 自己返回的 `tool call aborted`（前台等待或交接时调用被取消）同样是 `AbortError/ABORTED`，与上游 tool-bash 设置的 name 和 code 一致。 |
| `ToolOutcomeUnknownError` / `TOOL_OUTCOME_UNKNOWN` | JSONL resume 修复写入的 `Error: interrupted before a result was committed`。nano 在执行批次前提交全部 tool/call，未决调用都可能已经开始，因此不写 `TOOL_NOT_STARTED`。 |
| `FsError` / `FS_NOT_FOUND`、`FS_NOT_REGULAR_FILE` | read/read_image 的目标不存在，或路径中间段不是目录（上游同样把 ENOTDIR 归为 `FS_NOT_FOUND`）；edit 观察到目标缺失；目标是目录或特殊文件。上游的 `FS_NOT_DIRECTORY` 只用于目录列举，本仓文件工具没有对应路径。 |
| `FsError` / `FS_NOT_TEXT`、`FS_TOO_LARGE` | read/edit 遇到二进制或非法 UTF-8；edit 超过 10 MiB；read_image 超过源字节上限。read 的窗口截断是成功，不是 `FS_TOO_LARGE`。 |
| `FsError` / `FS_NOT_OBSERVED`、`FS_STALE_VERSION` | write/edit 的 `errNotRead`、`errStale`，包括已读后变化或删除、并发创建导致的盲覆盖拒绝。 |
| `FsError` / `FS_EDIT_NOT_FOUND`、`FS_AMBIGUOUS_EDIT` | 字面替换零次匹配；多次匹配且未设 `replace_all`。 |
| `FsError` / `FS_ABORTED` | `read aborted`、`write aborted`、`edit aborted`：读取、摘要或发布前发现取消。link/rename 成功后写入即已提交，工具返回成功；若此时调用已取消，按第 2 节替换为 `ABORTED`，文件保持已发布。 |
| `FsError` / `FS_PERMISSION_DENIED`、`FS_SANDBOX_DENIED`、`FS_IO_ERROR` | `fs.ErrPermission`；workspace 越界或符号链接拒绝；其他 stat/open/read/摘要/暂存/同步/link/rename 失败。`FS_SANDBOX_DENIED` 的具体条件在 WP14 会话 sandbox 模式合入后按实际路径重新核对。 |
| `SearchError` / `SEARCH_INVALID_PATTERN`、`SEARCH_FAILED`、`SEARCH_RAW_OUTPUT_OVERFLOW`、`SEARCH_ABORTED` | rg 拒绝正则或 glob；搜索根失败、显式特殊文件、启动失败、信号、非 0/1 退出、`--json` 输出畸形；stdout 超过 20,000,000 字节；搜索超时或调用方取消。rg 缺失或版本过低是启动错误，不产生工具结果。 |
| `WebError` / `app/web` 的全部 Code | 包括上游同名码和本仓已有的 `WEB_SEARCH_TIMEOUT`、`WEB_REQUEST_RECORD_FAILED`；文本保持 `<CODE>: <message>`。非 2xx HTTP 是成功结果。 |
| `SandboxUnavailableError` / `SANDBOX_UNAVAILABLE` | 前台 bash 的 runner 缺失或失败，即 `errors.Is(err, process.ErrSandboxUnavailable)`；文本已是上游 `SandboxUnavailableError` 的原文。platform 不依赖领域层，分类在 shell adapter 补上。后台 job 的 runner 失败只记录为 failed 状态，不是工具错误。 |
| `SubagentError` / `app/subagent` 的全部 Code | `INVALID_REQUEST`、`DEPTH_LIMIT`、`ACTIVATION_LIMIT_REACHED`、`UNAUTHORIZED`、`NOT_RESUMABLE`、`PARENT_UNAVAILABLE`、`ABORTED`、`ABORTED_BEFORE_DISPATCH`、`ACTIVATION_TEARDOWN_FAILED`。`DEPTH_LIMIT` 是本仓已有码，上游深度错误没有码。`SubagentError/ABORTED` 与 runtime 的 `AbortError/ABORTED` 码相同，靠 name 区分。 |
| `GoalError` / `app/goal` 的全部 `GOAL_*` | 领域拒绝。 |
| `HarnessError` / `GOAL_TOOL_*` | goal 工具的 `toolError`。name 与上游 `new HarnessError` 一致，不另起 `GoalToolError`。 |
| `UserQuestionError` / `ASK_CANCELLED`、`DELEGATED_CALLER`、`EMPTY_QUESTIONS`、`BAD_INTENT`、`NO_PROVIDER`、`BAD_ANSWER`、`ASK_ABORTED` | `ErrCancelled`、`ErrDelegated`、空问题列表、intent 违规、`ErrUnavailable`、`ErrInvalidAnswer`、`ErrAborted`。其余 `RequestError`（题数、id、选项、长度）对应上游的普通 Error，没有分类。exit_plan_mode 原样传播这些提问错误，因此带同样的分类；它自己的关闭、继续规划和未激活错误没有分类。 |

没有分类的失败包括：工具 Check 的语义错误、approval 拒绝或记录失败、执行点未获批准、panic、参数超限、jobs/todo/skill/plan 自身的错误、图片格式/路由/像素错误，以及各服务未运行。它们在上游同样没有分类；runtime 不按文案补分类。

### 4. 每个工具的 meta

当前 24 个工具中，只有上游有 presentationMeta 的 8 个生成 meta；其余 16 个（bash、job_output、job_list、job_kill、subagent、subagent_fork、send_message、interrupt_agent、list_agents、get_goal、create_goal、update_goal、ask_user_question、exit_plan_mode、todo_write、skill）不生成，也不复制 canonical value。会话日志、job 服务、goal/todo 记录和正文已经是这些事实的所有者。

| 工具 | 成功 meta | 取值 |
|---|---|---|
| `read` | `path, offset, lines[{number,text}], total_lines, truncated` | 从实际返回的窗口生成。`number` 是 1-based 文件行号，`text` 是已截行的值，`total_lines` 精确。空文件为 `offset=1, total_lines=0, lines=[]`。`truncated` 只表示第 5 节的预算移除了尾部行；窗口本身的截断由 `lines` 与 `total_lines` 表达。不持久化上游的 `lang`，渲染方可以从 path 推导，本仓不维护扩展名映射表。 |
| `read_image` | `path` | 图片 ID、尺寸和字节仍以 `result.image` 为唯一引用，不复制图片或附件路径。 |
| `write` | `operation:"create"\|"update", diffs[{path, old_text: string\|null, new_text}], truncated` | 按上游取舍，create 和内容相同的覆盖写的 `diffs` 为空。旧文件或新内容达到 10 MiB、旧文件是二进制或非法 UTF-8 时，没有 diff 基础，`diffs` 为空且 `truncated=true`。 |
| `edit` | `diffs[{path, old_text: string\|null, new_text}], truncated` | 从替换前后的实际内容生成，`replace_all` 覆盖全部替换位置，不把 `old_string/new_string` 当成文件变化。 |
| `glob` | `paths, total, truncated` | `total` 是发现的全部路径数，`paths` 是现有按修改时间排序的前至多 100 条。`truncated` 与上游相同，是结果上限和 meta 预算的并集。 |
| `grep` | `files[{path, matches[{line_number, line}]}], total, truncated` | 使用现有前至多 250 个匹配、每行 2000 字节预览和首次出现顺序分组。`total` 是解析得到的匹配总数，不因裁剪降低。 |
| `web_search` | `sources[{url, title?, snippet?, published_at?}], answer?, truncated` | 从 `app/web.SearchResult` 复制去重后的来源和合并后的 Content。`truncated` 是现有结果截断与 meta 预算的并集。 |
| `web_fetch` | `url, status_code, truncated` | 最终 URL 和 HTTP 状态；`truncated` 是正文渲染的实际截断（provider、来源或输出预算）。格式化与 meta 共用同一次计算，正文不重复存入 meta。 |

与上游的两处差异经协调者确认接受，等 UI 卡片工作开始时再复审：read 不持久化 `lang`；write 的 diff 至多一个 hunk。当前 meta 没有消费方，这两处只影响未来卡片的展示粒度，不影响数据正确性。

diff 规则：

- 基础文本与 edit 匹配所用的文本相同：去掉 BOM，CRLF 归一为 LF。这与上游的 LF diff 基础一致，CRLF 文件不会显示为整文件改动。
- write 只在新旧两侧都小于 10 MiB 时读取旧内容，在目标锁内与现有摘要校验同一次读取完成，不额外打开文件，内存上限与 edit 现有的 10 MiB 相同。
- 每个 hunk 带三行上下文。edit 按实际匹配位置生成 hunk，重叠的上下文合并。write 用公共行前缀和后缀确定一个变化区间，至多一个 hunk。这比上游 jsdiff 的最小 hunk 更粗，但只需标准库、线性时间，不引入第三方依赖。
- 单个 hunk 超过 meta 预算时直接跳过并标记 `truncated`，不先复制完整文本再丢弃。

失败结果一律没有 meta。

### 5. 上限、裁剪与校验

字节上限作用于 meta 的最终 JSON 编码（含工具名键、转义和框架），是协议常量，不是部署配置：

| 对象 | 上限 |
|---|---|
| output | 不变：256 KiB，UTF-8 归一、spill 与截断顺序不变。 |
| meta | 256 KiB；glob/grep 取上游的 65,536 字节。 |
| error | name、code 各 1–64 字节标识符。 |
| 记录与会话 | 不变：单条记录 6 MiB，会话 64 MiB，meta 计入其中。 |

裁剪由 `core/session` 统一拥有，runtime 在归一出口调用：read 移除尾部行，glob 移除尾部路径，grep 移除尾部匹配后删除空组，write/edit 移除尾部 hunk，web_search 先移除尾部来源，再移除 answer。与上游的软上限不同，列表可以被裁到空，因此上限是硬性的。路径、URL（每个至多 2048 个 UTF-16 码元）、状态码和计数这些不可裁剪的字段有现成的输入上限，裁剪后总能放进预算。裁剪按完整 DTO 重新编码检查，不截断字节。

校验分三层，Append、Open、Inspect 和 fork seed 共用：

1. JSONL 解码沿用 `decodeStrict`。DTO 是带 JSON tag 的闭合结构体，`DisallowUnknownFields` 递归拒绝未知成员和多个顶层值，不需要 RawMessage 和额外的 token 遍历。重复键与其他记录一样按标准库语义处理，不为 meta 单独检测。
2. `Record.Validate` 校验 result：`error` 只能出现在 `is_error=true` 的结果中，且 name/code 符合标识符规则；`meta` 只能出现在成功结果中，恰好一个成员，计数和行号是不超过 2^53−1 的非负整数，read 行号从 offset 起连续且不超过 `total_lines`，`total` 不小于保留项数且只少于 `total` 的列表必须标 `truncated`，`operation` 取枚举值且 create 没有 diffs，`status_code` 在 100–599，编码后不超过上限。错误结果仍不能带图片；成功结果可以同时带图片和 meta。分类码不设静态白名单：新增或合入的领域码由 producer 测试固定，不必修改 session 格式。
3. JSONL order validator 要求 meta 的工具名键等于对应 tool/call 的 name。`arguments_omitted` 的结果必须是错误，错误结果又不能带 meta，所以它不会带 meta。meta 中的 path、URL 不授予任何权限，也不替代调用归属。

字段可选只表示格式层允许没有结构化数据，不从旧 output 补造。producer 的义务由永久测试固定：上述 8 个工具的每个成功结果都有 meta，第 3 节的每条失败路径都有对应分类。非法持久化数据整体拒绝，原文件不改写；写入或 fsync 失败不发布新投影。

### 6. Spill、图片、模型投影、WP13 与恢复

- spill、KeepInline、SaveText 降级和历史 locator 授权沿用 ADR-0008。只缩小 Output，error/meta 原样保留；错误结果被 spill 时也保留 `is_error` 和 `error`。meta 不另写 spill 文件，meta 中的路径不扩大历史授权。
- `result.image` 沿用 ADR-0017，是附件的唯一引用，先写对象再提交结果。read_image 的 `meta.path` 只是显示路径。`cloneResult`、`CloneEvent`、`Surface`、fork seed 和订阅者深复制 `Error` 与 meta 的所有切片，调用方修改结果不会回写持久状态。
- 所有 provider 请求、token 估算和 compaction 摘要输入只显式投影 `call_id/output/is_error/image`；error/meta 不进入 system、tool schema、消息文本或 wire。当前 provider 的 wire 由显式载荷编码，没有整体序列化 `ToolResult` 或 `SurfaceNode`；实施时用测试固定这一点。
- WP13 的 `compaction/prune` 只替换 surface 节点的 Output，节点的 call ID、`is_error`、`error`、`meta` 和图片保持不变；prune 校验仍只比较 Output。summary 遮蔽结果后，它不再进入模型，但原日志中的 error/meta 仍可检查。WP12 不增加裁剪事件、保留策略或 summary 内容。
- 重启修复只写 `TOOL_OUTCOME_UNKNOWN`，不写 meta。文件、todo 或 goal 可能已提交而 tool/result 未写入，恢复时不能根据参数造 diff，也不能重新执行工具。

### 7. Composition、版本与数据保留

沿用 nano session v2。新增字段是加法，由 composition 身份识别；meta 内不设版本号，因为 composition 已标识运行时语义，会话格式版本标识编码。模型 schema 和 prompt 不变，插件启动顺序和 Scope cleanup 不变，cmd 继续注入同一批实例。纯 DTO 不增加插件、服务定位器或全局错误注册表。

`52d3715` 的 composition 身份为 `tool-runtime-v2`、`fs-tools-v3`、`search-tools-v3`、`shell-tools-v3`、`job-tools-v2`、`subagent-tools-v4`、`todo-tools-v1`、`web-tools-v2`、`question-tools-v1`、`plan-tools-v1`、`skill-tools-v1`、`goal-tools-v2`、`spill-v1`、`attachments-v1`、`tool-result-prune-v1`、`session-v2`。WP14 还会提升 fs 与 shell 并增加 `sandbox-policy-v1`。WP12 之前的取消对齐只改变被取消调用的结果文本，不改变已持久化记录的含义，与 K1 引入该行为时一样不提升 `tool-runtime`。WP12 按批次合入，每批从合入时的实际基线各提升一次：

- 基础批把 `tool-runtime` 提升一档：runtime 自有分类、meta 通道和 resume 修复分类都在这一档。
- 每个 producer 批提升自己的 provider token：fs、search、web、shell、subagent、goal、question。question 批同时提升 plan，因为 exit_plan_mode 的结果会带上传播来的提问分类。
- jobs、todo、skill 自身的结果契约不变，token 不变；它们的 runtime 分类随 `tool-runtime` 一起变化。
- `session-v2`、`spill-v1`、`attachments-v1` 不因这些加法字段改变。

composition 不匹配的旧会话在 Open 和 Inspect 中都会被拒绝，与以往的身份提升相同；原 JSONL、附件和 spill 保留不动。旧二进制遇到新增字段按未知字段拒绝。新增手写样本 `session-v2-structured-results.jsonl`，现有样本保持原样，避免 writer 和 reader 一起漂移。

当前没有已发布会话的升级承诺，实施前核查发布状态。首次向用户发布会话数据前，必须复审拒绝旧 composition 是否仍可接受；若已有发布数据，先明确离线迁移、备份、回退和校验策略，不能因为 API 尚不稳定就推断数据可以丢弃。WP12 默认不迁移、不截断、不清理旧会话；有效的中断日志仍只追加修复记录。metadata 含源文件片段和 web answer，继承 transcript 的 owner-only 权限、保留期和备份范围，不得出现在诊断日志中；它不是静态加密的。

## 后果

错误分类和 8 个工具的展示数据可以从已提交的结果重建，spill 后仍可诊断，未来的 TUI 不必解析文本。分类只来自上游已有码和本仓已有 Code，没有新造词汇需要维护。工具声明领域错误，runtime 统一处理边界失败，DTO 和校验留在 `core/session`，依赖方向和所有权不变。模型输入的字节和图片形态不变。

代价是 metadata 占用会话空间并带来复制开销，read、search 和 diff 会重复部分正文。独立的硬上限限制了最坏开销，但不能保证 64 MiB 的会话永远写不满。write 需要在锁内读取此前只做摘要的旧内容，上限沿用 edit 的 10 MiB。diff、read 和 search 的 meta 可能比模型正文更短，使用方必须尊重 `truncated`，不能把 meta 当成完整内容或恢复材料。

没有分类的失败（approval、panic、参数超限、jobs/todo/skill/plan 的错误）只能从 `is_error` 和 output 判断，与上游相同。

本仓的 snake_case 字段和工具名键与上游的 session wire 不互通。TUI 卡片、jobs/subagent 的 canonical value 和 WP13 的保留或裁剪语义需要各自的需求和验证，不能借这次数据补齐隐式发布。

## 被否决方案

- **从 Error() 或模型正文提取 code 和 meta**：spill、截断、文案调整和包装的原因都会丢信息，也无法证明文件的实际差异。
- **为上游是普通 Error 的路径新造本仓码**（如 `APPROVAL_*`、`JOB_*`、`TODO_*`、`SKILL_*`、`PLAN_*`、`IMAGE_*`、`TOOL_PANIC`）：没有消费方，扩大了需要维护的词汇和并行分支的冲突面，也偏离“持久化上游分类”的维护者决定。需要时由具体消费方的需求单独提出。
- **在 error 中加入本仓自定义的 `info`（stage、outcome、limit、actual）**：上游没有这个结构，也没有消费方；阶段和结局已经能从记录顺序和 turn 结局得到。
- **`json.RawMessage` 加 token 遍历和重复键检测**：RawMessage 绕过 `DisallowUnknownFields`，需要第二套解析器；闭合结构体直接复用现有严格解码，与其他记录的严格程度一致。
- **在 meta 内设 `version` 和 `kind` 字段**：版本由 composition 身份承担；工具名键已经区分种类。
- **新建 `core/toolresult` 包**：持久化载荷类型都在 `core/session`，另建包只会增加依赖边。
- **分类码静态白名单**：每个新领域码都要修改 session 格式，校验收益只是拒绝语法合法但未知的码。
- **runtime 按工具名导入所有领域包来分类**：消费方与每个工具的错误耦合，破坏依赖方向；最小 `Failure` 接口已经足够。
- **每个工具手写 map 或 JSON**：字段和预算容易漂移，也缺少重放契约。
- **照搬上游 canonical output schema、render 和 PTC 框架**：本仓 Execute 已经类型化，当前只有 metadata 和错误这两个消费方。
- **只靠 spill 或单条记录上限约束 meta**：spill 不缩小 meta；超大 JSON 会在工具副作用之后让结果提交失败。
- **给全部工具复制 canonical value**：上游这些工具大多没有 meta；目标、任务和交互已有权威记录或正文。
- **把 error/meta 发给模型，或同时做卡片**：违反已确定的模型文本和产品范围。
- **恢复时把未决调用标为未开始**：nano 在执行前提交 tool/call，不能证明 body 没有进入。
- **一次原子交付全部改动**：所有 producer 都要等最慢的分支合入；按批次合入时，每批提升自己的 token，每个中间状态都自洽。
- **新的会话格式版本或静默接受旧 composition**：加法字段在 v2 中可显式识别；静默接受会让缺少 metadata 的旧会话看起来满足新契约。

## 复审触发条件

并行分支合入后改变了错误路径、DTO、token 或持久化策略（尤其是 WP14 的 sandbox 拒绝）；首次发布用户会话数据；WP13 需要实际裁剪、移动 metadata 或改变 summary 输入；TUI 卡片需要 jobs、subagent 等新数据，或需要上游的 `reason`；实测 metadata 导致会话容量、写入延迟或常驻内存问题；UI 卡片工作开始（复审 `lang` 与单 hunk diff 两处差异）；上游改变 error/meta 形态、取消替换规则或引入更严格的通用上限。

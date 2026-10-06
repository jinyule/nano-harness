# 结构化工具结果的基础批：格式、校验与 runtime 分类

- Status: implemented
- Date: 2026-10-07

## Context

[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 决定把上游的错误分类 `{name, code}` 和 8 个工具的结果 metadata 持久化到 `tool/result`，模型可见文本不变；[WP12 实施记录](2026-10-06-structured-tool-results.md)把它拆成批次。本批是 F 基础批：所有工具共用的会话格式、严格校验、resume 分类和 runtime 自有分类。它建立在 R 批的取消对齐之上（[取消只替换成功结果](2026-10-07-tool-cancel-keeps-body-failure.md)），Execute 返回的错误因此能保留领域分类。

D/S producer 的结构化分类与搜索 metadata 由[搜索与 shell producer 记录](2026-10-06-structured-search-shell-results.md)补充；本记录继续拥有上述机制与原验证证据。

非目标：任何工具 producer 的分类或 metadata（文件、搜索、web、shell、subagent、goal、question 各批负责）、provider wire 的跨协议证据（收尾批）、TUI 卡片和旧会话迁移。

## Decision

- `internal/core/session` 新增 `ToolError`、`ToolMeta` 与 8 种闭合 DTO。`ToolResult` 增加可选的 `error` 与 `meta`。`Record.Validate` 要求分类只出现在错误结果上，名字与 code 是 64 字节以内、以字母开头的 ASCII 标识符；metadata 只出现在成功结果上，恰好一个成员，字段一致（read 行号从 offset 连续且不超过 total，列表非 null，partial 列表标 `truncated`，create 没有 diffs，状态码 100–599），字符串是合法 UTF-8，JSON 编码不超过 256 KiB（glob/grep 65,536 字节）。`ToolMeta.Fit` 返回修复 UTF-8 后的深拷贝，并用二分查找丢弃尾部项直到放进预算：read 行、glob 路径、grep 匹配（随后删除空组）、diff hunk、web 来源，最后是 answer；完整列表放不下时至少丢一项，不靠只改 `truncated` 省下的字节冒充放下。`cloneResult`/`CloneEvent` 深复制分类与 metadata。
- JSONL 解码仍是 `decodeStrict`，闭合结构体让未知成员直接被拒。order validator 要求 metadata 的成员属于对应 call 的工具。resume 为未决调用补写的结果分类为 `ToolOutcomeUnknownError/TOOL_OUTCOME_UNKNOWN`。
- `app/tool` 定义消费方接口 `Failure`（`error` 加 `ToolError() session.ToolError`），`Result` 增加 `Meta`。runtime 为未知工具（`ToolNotFoundError/UNKNOWN_TOOL`）、prepare 的 schema 失败（`ToolArgsError/INVALID_ARGS`）、两个调度前取消点（`AbortError/ABORTED_BEFORE_DISPATCH`）和成功后取消（`AbortError/ABORTED`，同时丢弃 metadata 与图片）写入分类；Check 与 Execute 的错误经 `errors.As` 找 `Failure`，找不到就不分类。成功结果的 metadata 先 `Fit` 再校验，校验失败或成员不属于本工具时结果改为 `ToolOutputError/INVALID_TOOL_OUTPUT`，文本为上游的 `tool "<name>" returned invalid output: <violation>`；`Failure` 给出的非法分类同样如此。panic 丢弃图片、分类与 metadata。spill 只改文本。参数超限、approval 失败、panic 与普通错误不分类。
- composition 的 `tool-runtime` 提升到 v3；session 仍为 v2。新增手写样本 `session-v2-structured-results.jsonl`，现有样本不变。纯值类型和 helper 没有副作用，不是插件；插件、Scope 与关闭顺序不变。

## Consequences

所有工具批都可以只改自己的 adapter 或领域包：实现 `Failure` 并填 `Result.Meta`，格式、预算、校验和重放由本批统一负责。磁盘上的失败已经能区分 runtime 自有的几类失败，模型输入的字节不变。

代价是 `Record.Validate` 和 runtime 在有 metadata 时要多做 JSON 编码（二分裁剪最多约 log₂(n) 次）。分类码不设白名单，语法合法但未知的码会被接受，正确性靠各 producer 的测试固定。提升 `tool-runtime` 后，旧 composition 的会话在 Open 和 Inspect 中都被拒绝，文件保留不动；当前没有发布数据。

## Verification

- 新行为先写独立预期：`core/session` 的 metadata 字段规则、预算边界与裁剪、分类标识符、`Record.Validate` 和深复制测试；`app/tool` 的 runtime 分类、`Failure` 穿过包装与取消、非法分类与他人成员、spill 保留与 panic 丢弃测试；JSONL 的固定样本字节比较、10 种变更负例和 resume 分类测试；cmd 的真实组装测试证明分类写入磁盘、下一次模型请求不含 `ToolNotFoundError`/`UNKNOWN_TOOL`/`INVALID_ARGS`，以及旧 `tool-runtime` 身份的会话在 Open 与 Inspect 中被拒绝且字节不变。
- 新增 6 项定向 mutation（丢弃 `Failure` 分类、不记录 metadata、跳过成员与 call 的关联、裁剪不强制丢项、resume 写错分类、允许成功结果带分类），用单独清单运行 `scripts/mutation-check.py` 全部被具名测试拒绝。resume 那一项最初删除字段导致编译失败，不算拒绝证据，改为替换成 `TOOL_NOT_STARTED` 后被拒绝。
- `go test -race -count=1 -coverprofile` 下 `internal/core/session`、`internal/app/tool`、`internal/adapter/session/jsonl` 的语句覆盖率都是 100%。
- R 批的 mutation `tool-cancel-keeps-body-failure` 定位的注释随本批更新，首轮 `make check` 报告 stale-site；更新定位后它仍被 `TestRuntime_CancellationKeepsTheBodyFailure` 拒绝。首轮 lint 的 `modernize`（改用 `new(expr)` 与 `bytes.Cut`）和 `revive`（context 参数在前）问题修正后重跑。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race、逐产品文件 100% coverage、架构、Agent Note、lint 0 issues、全部定向 mutation 和真实 cmd build/smoke。`make tui-e2e` 通过（真实二进制与 PTY 下 19 个 root 工具调用，含中断与恢复），终端文本不变。没有 live provider 或其他操作系统的证据。

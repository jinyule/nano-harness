# 搜索、错误 spill 与历史定位符的上游对齐

- Status: implemented
- Date: 2026-10-06

## Context

基线 `d01c5f42bb1d06d0a579737f19886fbb5fc1903d` 与只读参考 `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 存在五项差距：Go 空白集与 JS trim 不同；grep 在识别 framing 前解码 match 数据且接受 null；search stderr 尾部少 1,536 字节；runtime 跳过错误 spill；更换写入 root 后已有定位符失去读取权限。永久测试先于产品修改运行，全部以目标断言失败。

本次只在 `wp/codex-search` 专用 worktree 工作，不修改参考 submodule、web fetch/tool adapter、engine 或 job。没有新依赖、协议字段、后台 worker 或数据迁移。旧的[工具定义 Note](2026-10-04-upstream-tool-definitions.md)、[spill Note](2026-10-05-tool-output-spill-and-read-before-write.md)、[web Note](2026-10-04-web-search-and-fetch.md) 与[多模态 Note](2026-10-05-multimodal-tool-results.md) 保留能力建立、图片豁免与原始验证证据；本 Note 部分补充它们的边界与恢复行为，旧 Note 回链并同步过时事实，不归档。skill 描述归一化和 spill 创建目录校验的 Note 属于独立契约，保持原有 owner。

集成基线为 `feat/upstream-tool-parity` 的 `566a7d452501bca5711b7557df70dc708416ecb7`：保留 WP11 的附件引用与请求时读取、WP3 的取消及通知持久化、WP8 的会话自身规划投影和锁，以及 ADR 的取代指向。新增 cmd 测试复用已显式配置 `attachmentRoot` 的 `todoConfig`；mutation 清单保留附件用例并追加本次用例，ID 与变异位置唯一。

D/S producer 的结构化分类与搜索 metadata 由[搜索与 shell producer 记录](2026-10-06-structured-search-shell-results.md)补充；本记录继续拥有上述机制与原验证证据。

## Decision

- `app/tool.IsBlank` 是无副作用纯函数，固定 ECMAScript WhiteSpace 与 LineTerminator 集合；glob pattern/path、grep path/include 与 app/web 查询复用，原参数不修改，grep 非空空格正则保留。
- grep 先解析 JSON、拒绝 null/标量、跳过非 match framing，再解码 match 数据；畸形 match 拒绝整个搜索，不返回部分匹配。search 专用 `Request.StderrLimit` 为 65,536 字节，runner 默认与 bash 仍为 64,000；立即终止进程组的偏离由 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md) 记录。
- runtime 在各返回路径统一修复 UTF-8、应用文本 spill、再做 durable 截断；超预算错误保存完整信封并保留 call ID、`IsError` 与错误码文本。图片与 `KeepInline` 仍豁免；`KeepInline` 是不可变工具定义元数据，schema 失败也能保留豁免；缺 store/session 或保存失败保留有界原结果。spill 收尾在外层 panic 边界内，store 的 panic 保持为调用错误。
- `workspace.Root.ReadableFrom` 只从调用方原始已提交结果的标准尾注授权精确历史 spill 文件。日志身份、workspace 分区、命名、普通文件、私有权限和 root/partition/session/file 无链接在执行点强制；无可读日志或失败时拒绝。fork 继承与 compaction 遮蔽记录仍参与，历史权限不扩大目录、写工具、glob 或 shell。安全取舍与允许/拒绝矩阵归 [ADR-0008](../../../docs/decisions/0008-tool-output-spill-and-observation-policy.md)，查询契约归 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md)。
- 实现沿既有 `search-tools`、`fs-tools`、`web`、`tools` 与 `spill-local` 插件及 cmd composition；没有新 effect 或可变授权缓存，Scope 仍撤销贡献、取消并等待进程、等待文件与 sweep 静止。spill root 不进入 composition ID，本改动不再改变文件布局、session v2 或工具 schema；沿用 WP11 的 `attachments-v1` 身份与附件引用契约，当前有效日志可恢复。
- 定向 mutation 固定 BOM、search 诊断预算、null 拒绝、错误 spill、精确历史定位符、workspace 分区和历史链接边界。

## Consequences

搜索和 web 的空白边界一致，framing 的未来数据形状不会导致误拒，长错误受同一上下文预算限制。恢复、fork 与 compaction 后可以读回仍存在的历史结果，新增文件只写入新 root。

历史授权每次按请求路径扫描调用方日志，不缓存授权；日志上限仍为 64 MiB，只有原路径边界拒绝时才扫描。标准尾注不提供来源认证，结构和私有路径限制防止借此开放任意绝对路径；同一用户的 TOCTOU 风险、旧 root 的清理/丢失和 Windows 权限位差异沿用 ADR-0008。没有 live provider 或其他 OS 原生运行证据。

## Verification

- 修复前：`go test -race -count=1 ./internal/adapter/tool/search ./internal/app/web ./internal/app/tool -run 'TestSearch_(ECMAScriptBlankArguments|Retains65536ByteDiagnosticTail)|TestGrep_ParsesFramingBeforeMatchData|TestParseQueries_ECMAScriptWhitespace|TestRuntime_SpillsErrorsBeforeDurableTruncation'` 退出码 1。BOM 被接受、NEL 被误拒；begin 字符串数据被拒而 null 被跳过；stderr 总结果 64,055 字节而预期 65,591；错误只有 262,144 字节内联截断，没有 spill 预览。
- 修复前：`go test -race -count=1 ./cmd/nano-harness -run '^TestComposition_ReadsHistoricalSpillsAfterRootChange$'` 退出码 1；真实恢复成功、旧完整文件仍在，两个历史读取结果均为 outside workspace。
- 修复后：上述测试通过。composition 用真实 ripgrep 生成完整列表与约 60,000 字节的长正则错误，检查 `is_error`、预览及完整诊断文件；显式 compaction 遮蔽所有原结果后更换 root 恢复，再从 root 与真正的 subagent_fork 读回旧列表/错误文件。断言来自磁盘日志、完整文件字节、model surface 与 child 自有记录。
- `TestRoot_HistoricalSpillAllowDenyMatrix`、`TestRoot_HistoricalSpillRejectsUnsafeFilesAndDirectories` 覆盖允许/拒绝矩阵；日志取消、Lstat/链接解析错误均保留根因。`TestRunnerRun_RetainsCallerStderrBudgetIndependently` 从真实进程验证默认与自定义 stderr 不改变 stdout 尾部。
- `TestComposition_ECMAScriptBlankQueriesAndSearchArguments` 验证真实配置、工具 runtime 与 loopback provider 的计费调用边界：BOM 拒绝，NEL 保留原文，精确重复只执行一次，搜索空白校验与空格正则结果写入磁盘。
- 收尾回归先失败再修复：`go test -race -count=1 ./internal/app/tool -run '^TestRuntime_ContainsSpillImplementationPanics$'` 在单层 defer 实现上以 store panic 退出；`go test -race -count=1 ./internal/app/tool -run '^TestRuntime_KeepInlineErrorsStayInline$'` 在仅向 schema-valid call 传递豁免的实现上报 `schema error bypassed KeepInline: bytes=49933 artifacts=1`。两项永久测试及 schema/语义/执行错误、图片、保存失败的原有边界均通过。
- `make coverage` 通过：全部产品源文件 100.0%，原始 profile 没有未覆盖语句。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race tests、架构、submodule、Note/skill/workflow 格式与契约、lint（0 issues）、逐文件 100.0% statement coverage、[清单](../../../scripts/mutation-cases.json) 中全部定向 mutation 以及真实 cmd 构建/版本 smoke 均通过。mutation 使用私有副本与无缓存命令，基线均先通过，变异均由命名测试断言失败拒绝。
- `make tui-e2e` 通过：真实 binary/PTY 的 19 次 root 工具调用、图片结果、todo、后台 job 通知、提问、计划、goal、spawn/fork、approval、文件、粘贴、resize、wrap、interrupt、resume 与 cleanup 均有外部证据。
- 环境为 Go 1.27.0、darwin/arm64、golangci-lint 2.12.2。`git diff --check`、变更 Markdown 相对链接与禁止修改范围检查通过；参考 submodule 的指针未变、工作树干净。未运行 live provider、其他 OS 原生测试、vulnerability 或 release 矩阵，本次没有依赖或发布面修改。

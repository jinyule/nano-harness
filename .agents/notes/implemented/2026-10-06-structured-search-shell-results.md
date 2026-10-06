# 搜索与 shell producer 持久化结构化结果

- Status: implemented
- Date: 2026-10-07

## Context

[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 与 [WP12 实施记录](2026-10-06-structured-tool-results.md)要求 D 搜索批与 S shell 批在模型正文不变的条件下补齐 error/meta。[基础批](2026-10-07-structured-tool-results-base.md)已经提供 `tool.Failure`、类型化 DTO 与 runtime 的硬预算；[WP14](2026-10-06-session-sandbox-modes.md)已经提供三档策略和实际 launch mode 文案。producer 尚只返回普通 error 与正文，磁盘无法得到分类或搜索展示数据。

永久测试先于产品修复执行：search 的四类失败、根失败与成功 metadata 都在真实 JSONL 文件中得到 nil；shell 的 read-only/workspace-write 缺失或失败 backend、前台等待取消和 job 上限回退取消也得到 nil。真实 composition 测试用修复前 producer 的 Go overlay 重演，两个 meta 与两个 error 的独立磁盘断言均失败，而正文相同。

本记录只拥有 D/S 的 producer 数据与验收证据。[搜索/spill 对齐](2026-10-06-search-spill-query-parity.md)继续拥有正文、query、历史 locator 与 parser；[shell 文案](2026-10-07-shell-job-upstream-text.md)继续拥有 runner 诊断和 drain；基础批继续拥有格式、归一与裁剪；WP14 继续拥有模式/授权。它们是部分补充，全部保留，不归档。其他 producer 批、TUI 卡片、新码、新 ADR、迁移和推送不在范围内。

## Decision

- search 在失败产生处使用未导出的 `searchFailure`，通过 `ToolError` 返回上游 `SearchError` 与四个既有码；`Error` 保留原文，`Unwrap` 保留 launch、路径/I/O 与调用取消原因。Check 的语义失败和启动版本检查仍无工具分类。
- glob 的 meta 使用实际修改时间顺序的前 100 个路径与总数；grep 使用实际前 250 个匹配、首次出现的文件分组和同一行预览。零结果使用空列表。SaveText 成功、失败或缺 store 都保留 meta；最终 JSON 的 65,536 字节硬预算由基础批的 `Fit` 拥有，单个超大项可以裁到空，total 不减少。正文与 footer 不变。
- shell 在 `finish` 中用 `errors.Is` 识别 `process.ErrSandboxUnavailable`，补 `SandboxUnavailableError/SANDBOX_UNAVAILABLE`，保留实际 read-only/workspace-write 文案及 runner 原因。工具自身取消在后台启动前、前台等待/交接和回退执行返回 `AbortError/ABORTED`，正文保持 `tool call aborted`；错误链保留可用原因。普通失败没有分类，非零退出、信号和超时仍是成功文本，后台基础设施失败仍只是 job failed，bash 没有 meta。
- composition 只提升 `search-tools-v3` 到 v4、WP14 的 `shell-tools-v4` 到 v5。旧身份的 Open/Inspect 拒绝并保留文件。既有 `search-tools`/`shell-tools` 插件和显式注入不变；包装/metadata 是纯值，不增加注册、goroutine 或文件 effect，进程、job、临时目录和贡献仍由原 Scope 清理。

## Consequences

磁盘可独立重建搜索展示和两类 shell 失败，spill 后仍保留分类与 metadata；模型请求仍只包含原正文。搜索 metadata 增加有界日志空间与复制开销，预算可以使它比正文短，consumer 必须尊重 truncated。直接取消错误的原因链更完整，模型文案不变化。

拒绝从正文反解析预期或在 platform 引入领域分类；复用已有 Failure/DTO/裁剪，避免第二套预算和新抽象。模式、进程后代回收、数据保留及旧 composition 的风险沿用 ADR-0019/0021。本机证据不替代其他 OS 原生或 live provider 验证。

## Verification

- 修复前 `go test -count=1 ./internal/adapter/tool/search -run 'TestSearch_(Persists|Drops)'` 与 `go test -count=1 ./internal/adapter/tool/shell -run 'TestBash_(Persists|LeavesOrdinary)'` 退出 1，目标磁盘字段为 nil。`go test -count=1 -overlay <原 producer 源码 overlay> ./cmd/nano-harness -run '^TestComposition_PersistsSearchAndShellStructuredResults$'` 退出 1，四个目标结果缺字段；overlay 不修改仓库源码。
- 修复后 focused race 测试通过 search、shell 和 cmd。真实 rg 的 0/1/100/101 与 0/1/250/251、分组重复、Unicode/CRLF、SaveText 三种结果、完整 artifact、meta 硬预算、错误 spill、模式文案与 host 文件、取消/静止、原因链和普通失败都有独立预期。真实 composition 从磁盘比较数据并逐字比较下一模型请求正文，结构化字段不进入模型；旧 token 测试同时拒绝 Open/Inspect 且文件不变。
- 新增 8 个默认 mutation，覆盖 producer 删除分类、glob/grep 保存后丢 meta、重复文件错误分组、runtime spill 丢 meta、shell sandbox 与等待/回退取消分类。第一次分组 mutation 存活，fixture 只有第一组重复；补第二组重复匹配后断言覆盖该回归。mutation 在私有副本中先通过 baseline，再无缓存拒绝具名变异；编译错误和超时不算通过。
- `go test -race -count=1 -coverprofile=/tmp/wp12-producers.cover ./internal/adapter/tool/search ./internal/adapter/tool/shell` 通过，两个 package 的全部产品源文件/函数均为 100.0%；`go test -race -count=1 ./cmd/nano-harness -run 'TestComposition_(PersistsSearchAndShellStructuredResults|StructuredResultsRejectOldRuntimeSessions)'` 通过。
- 已 rebase 到 `feat/upstream-tool-parity` 的 `c7d6012951b80bc8828507d605f343a108372b51`。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race、架构、submodule、Agent Note/skills/workflow 门禁、lint 0 issues、每个产品源文件 100.0%、151 个默认 mutation 全部 killed、真实 cmd build/version。首轮 lint 的测试 helper context 参数位置和可执行 fixture 权限标注已修正，重跑完整门禁通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 通过：真实 binary/PTY 的 19 次 root 工具调用、sandbox 切换、审批、图片、后台任务、提问、规划、goal、spawn/fork、中断、恢复与 cleanup。变更 Markdown 相对链接和 `git diff --check` 通过；只改专用 worktree，submodule 指针/工作树不变，没有推送。
- 环境为 Go 1.27.0、darwin/arm64；没有 live provider、其他 OS 原生、漏洞扫描或 release 矩阵证据，本改动没有依赖或发布面变化。

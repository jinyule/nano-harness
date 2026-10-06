# web 工具持久化结构化结果

- Status: implemented
- Date: 2026-10-07

## Context

[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 与 [WP12 计划](../proposed/2026-10-06-structured-tool-results-plan.md)要求 E 批在模型正文不变的条件下补齐 web 的 error/meta。[基础批](2026-10-07-structured-tool-results-base.md)已经提供 `tool.Failure`、`WebSearchMeta`/`WebFetchMeta` 与 runtime 的裁剪和校验；`app/web.Error` 已有 12 个稳定代码，但 runtime 拿不到分类，web_search 和 web_fetch 也只返回正文。`formatFetch` 内部已经算出 provider、转换输入和完整输出三种截断，但只体现在 footer 中。

永久测试先于产品修改执行：工具层的分类、检索 metadata 与抓取 metadata 三个测试在原 producer 上都得到 nil 字段；真实组装测试在原 producer 的 Go overlay 上以 `search metadata = <nil>` 失败。

本记录只拥有 E 批的 producer 数据与验收证据。[web 工具记录](2026-10-04-web-search-and-fetch.md)继续拥有 provider、抓取策略与生命周期，[检索请求审计记录](2026-10-06-web-search-request-audit.md)继续拥有审计事实。它们是部分补充，保留并互链。其他批次、TUI 卡片、新码和迁移不在范围内。

## Decision

- `app/web.Error` 实现 `ToolError`，返回上游的 `WebError` 与原代码，包括本仓已有的 `WEB_SEARCH_TIMEOUT` 和 `WEB_REQUEST_RECORD_FAILED`。`Error()` 的 `<CODE>: <消息>` 与 `Unwrap` 不变。查询校验、服务停止和缺少 journal 的错误没有分类。
- web_search 返回 `WebSearchMeta`：来源按 `app/web` 去重轮转后的顺序复制 url/title/snippet/published_at，answer 是合并后的 Content，truncated 是现有的结果截断；正文由同一个 `SearchResult` 渲染。空结果的来源是空列表。
- `formatFetch` 一次返回正文和实际截断标记，后者恰好在添加 footer 时为 true；web_fetch 用它和最终 URL、HTTP 状态构造 `WebFetchMeta`，不复制页面内容，HTML 也只转换一次。非 2xx 仍是成功结果。
- composition 只把 `web-tools-v2` 提升到 v3。`web-tools` 插件、显式注入和 Scope 不变；metadata 是纯值，不增加 goroutine、注册或文件 effect。

## Consequences

磁盘可以独立重建检索来源、回答和抓取状态，失败带稳定的 web 代码；模型请求仍只含原正文。检索 metadata 会复制 answer 与 snippet，增加日志占用；基础批的 256 KiB 预算先丢尾部来源再丢 answer，consumer 必须尊重 `truncated`。旧 `web-tools-v2` 会话在 Open 与 Inspect 中被拒绝，文件保留不动；当前没有发布数据。

## Verification

- 修复前：`go test -count=1 -run 'TestProvider_Persists' ./internal/adapter/tool/web/` 的三个测试失败，结果的 `Error` 与 `Meta` 为 nil，输出在被忽略的 `.cache/wp12-web/red.log`。`go test -count=1 -overlay .cache/wp12-web/overlay.json -run '^TestComposition_PersistsWebStructuredResults$' ./cmd/nano-harness/` 用原 `internal/adapter/tool/web/web.go` 与 `internal/app/web/web.go` 失败；overlay 不修改仓库源码。
- 修复后：工具层测试覆盖全部 12 个代码在检索与抓取上的分类和不变正文、三类无分类失败、检索 metadata 与正文的来源顺序一致、空结果、抓取的非 2xx、provider 截断、转换输入超限、header 使输出超限和最终 URL；`formatFetch` 的既有预算表同时断言截断标记。`app/web` 测试证明分类穿过 `%w` 且可持久化。真实组装测试从磁盘比较两种 metadata 和未配置检索的 `WebError/WEB_PROVIDER_UNAVAILABLE`，下一次聊天请求不含 `status_code`、metadata 键或 `WebError`；旧 `web-tools-v2` 身份的会话在 Open 与 Inspect 中被拒绝且字节不变。
- 新增 3 个 mutation（分类改成固定代码、检索 metadata 丢 answer、抓取截断只看 provider 标记），用单独清单运行 `scripts/mutation-check.py` 全部被具名测试拒绝。
- `go test -race -count=1 -coverprofile` 下 `internal/adapter/tool/web` 与 `internal/app/web` 的语句覆盖率都是 100%。
- 基于 `feat/upstream-tool-parity` 的 `f39e9fe`（rebase 时 token 行、旧 token 测试表、ADR-0019 版本列表、architecture 的 composition 段落和 mutation 清单与并行合入的 sandbox、approval、subagent、goal/question 修复冲突，按并集解决）：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括全仓 race、逐产品文件 100% coverage、架构、Agent Note、lint 0 issues、全部默认 mutation 和真实 cmd build/smoke；首轮因新测试文件未 gofmt 在 fmt-check 失败，格式化后重跑通过。`make tui-e2e` 通过（真实二进制与 PTY 下 19 个 root 工具调用，含 sandbox 切换、中断与恢复），终端文本不变。没有 live provider、真实公网抓取或其他操作系统的证据。

# web_search 发送前提交会话请求审计

- Status: implemented
- Date: 2026-10-06

## Context

WP15 由维护者确定采用上游发送前审计：记录失败不发送。`internal/adapter/tool/web` 的执行函数丢弃 Invocation；`app/web` 与三个 provider 没有 durable request 接缝，`tool/call` 不能证明独立检索 route、账户选择的 endpoint 类别和实际预算。只读参考提交 `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 的 provider.ts/index.ts、README、session surface 与 repair 路径表明 `web/deepseek-search-llm-request` 保存无认证 body、endpoint 与 API version，记录异常阻止 fetch，恢复后保留 log-only 证据而不重发。

修复前永久测试 `TestComposition_WebSearchAndFetchEndToEnd` 失败：`disk transcript has 0 search request audits, want 1`；`TestProvider_SearchWithoutJournalFailsClosed` 失败：`searches=1` 且返回成功结果。运行命令见 Verification。

合入共享 ECMAScript 空白判断后，LLM 请求边界和新增审计 decoder 仍使用 Go `TrimSpace`：前者拒绝非空 U+0085，后者还接受空白 U+FEFF。永久领域、LLM 与 assembled 测试在修正前分别报告 NEL 被拒、BOM 被接受和 `searches=0`，具体命令见 Verification。

与 [web 工具 Note](2026-10-04-web-search-and-fetch.md) 部分重叠：该 Note 保留 provider/fetch/lifecycle 证据，本 Note 拥有发送前审计和 `web-tools-v2`，双方互链。总体 [工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md) 将 WP15 标记为已实现，并继续拥有其他未完成工作包；不归档仍有效的 Note，不修改归档记录。`internal/adapter/web/fetch` 和 `internal/adapter/tool/web/html.go` 不在本改动范围。

## Decision

长期字段、安全、恢复和兼容决定见 [ADR-0022](../../../docs/decisions/0022-web-search-request-audit.md)，它取代 ADR-0011 原先的不新增检索记录决定。

工具显式传递 Journal/Turn/Step/CallID；app 用有序 gate 将每个 distinct query 的 `web/search-request` 追加到调用方日志，provider 在最终发送边界填入冻结 route/effort、endpoint 类别与实际预算。追加失败阻止对应 HTTP 请求，取消并等待其余工作，返回安全的 `WEB_REQUEST_RECORD_FAILED` 并通过错误链保留原因。journal 缺失失败关闭。记录不包含传输 URL、认证 headers、账户标识或完整 prompt，不进入模型 surface。LLM 使用共享 `app/tool.IsBlank`，领域校验使用相同的 ECMAScript 集合，查询原文不变。

领域类型及校验、JSONL 严格 decoder/order、CloneEvent 隔离、固定样本和反例同步；resume 保留意图，只补 interrupted result 与 step/turn 结束，不补造审计或重发。format 保持 v2，composition 使用 `web-tools-v2`。原有 `web`、`web-tools` 与 model provider 插件沿 `composeApplication` 组装，不新增组件或 effect；query goroutine 仍由 web 操作取消并等待，Scope cleanup 保持先拒绝新操作、取消、等待的契约。

## Consequences

磁盘证据可以绑定检索意图与当前 pending web_search call，并说明实际 provider、模型和预算；有序追加不串行化网络查询。每个 query 多一次 fsync 和查询副本，增加 I/O 与敏感内容留存。意图不能证明远端执行；后续审计失败不能撤回已发送的兄弟请求。旧 composition 会话被严格拒绝继续，原文件保留。没有获得 live provider、跨平台原生执行或 fsync 性能测量证据。

## Verification

- `git rebase feat/upstream-tool-parity`：基准为 `8722b5ee3fb1ced198da521be6abcdd5877cfaea`。保留附件 token/读取、cmd 的 attachmentRoot、共享空白判断、抓取传输/HTML 预算、通知取消与规划隔离契约。fetch spill 测试替身同步 Search 签名；mutation 清单只增加本工作包的两个 ID，不恢复已删除的图片容量用例。
- 原审计缺失的修复前证据：`go test -count=1 ./cmd/nano-harness ./internal/adapter/tool/web -run 'TestComposition_WebSearchAndFetchEndToEnd|TestProvider_SearchWithoutJournalFailsClosed'`：两个永久测试分别以 `disk transcript has 0 search request audits, want 1` 和 `searches=1` 失败。
- 空白回归的修复前证据：`go test -count=1 ./internal/core/session ./cmd/nano-harness -run 'TestRecord_WebSearchQueryUsesECMAScriptBlankSet|TestComposition_WebSearchAuditsNELQuery'`：领域拒绝 NEL、接受 BOM，assembled 返回错误且 provider 收到 0 次请求。`go test -count=1 ./internal/app/llm -run TestRuntime`：以 `nonblank search query="\u0085": invalid LLM configuration` 失败。
- rebase 后 `go test -race -count=1 ./internal/core/session ./internal/app/llm ./internal/app/web ./internal/adapter/tool/web ./internal/adapter/model/provider ./internal/adapter/session/jsonl ./cmd/nano-harness`：全部通过。三个 provider（含 API key/OAuth）的字段与失败时零发送、缺 journal、错误链、查询去重/顺序/并发、严格样本拒绝、复制与恢复都有永久证据。真实配置/composition 中三个 provider 各 1–4 查询的 HTTP handler 从磁盘确认审计先提交，再用 barrier 证明网络并发；最终 transcript 验证 call/audit/result 因果顺序，下一模型 input 没有审计元数据。NEL 的原文、发送与磁盘记录一致；旧 composition 被拒绝且磁盘字节不变。
- rebase 后 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过。fmt/mod/vet、全量 race tests、architecture、submodule、Agent Note、skills、workflow-tools、lint（`0 issues`）、每个产品源文件 100% coverage、清单中的定向 mutation（均 killed）和真实 binary build/version 均通过。新增 `web-search-audit-failure` 与 `web-search-audit-call-kind` 分别拒绝忽略记录失败和其他 pending 工具引用。
- rebase 后 `make tui-e2e`：通过，真实 binary/PTY 验证脚本列举的 root 工具调用、附件图片、计划、后台通知、问答、规划审查、目标、spawn/fork、审批、文件、粘贴、缩放、打断、resume 与 cleanup。PTY 没有发送 web_search，检索的磁盘证据由上述 assembled 测试拥有。
- `git diff --check` 与改动 Markdown 的本地链接检查：通过。范围为专用 worktree；只读 submodule、抓取 adapter 和 `html.go` 未修改，没有凭据或无关生成物进入提交。material diff 的 Agent Note 携带规则由指定上述 base 的 `make agent-notes` 验证。

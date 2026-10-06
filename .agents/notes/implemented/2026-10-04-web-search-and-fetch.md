# web_search 与 web_fetch 对齐参考 Base 工具

- Status: implemented
- Date: 2026-10-04

## Context

[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP5 要求补齐参考 Base 组合的 `web_search` 与 `web_fetch`。参考提交 `5badb15009ae` 的 `packages/web/` 把能力拆为 `ctx.web` 服务、DeepSeek Messages 检索 provider、匿名 HTTP 抓取 provider 和 `tool-web` 消费方；生成目录给出两个工具的精确 schema，Base 把检索时限设为 60 s，抓取只到公网 HTTP(S)、逐个校验并固定连接，两者都不需要逐次确认。维护者确定 `web_search` 复用已配置 LLM provider 的服务端检索，不新增凭据。

本仓此前没有 web 能力、HTML/字符集依赖或网络地址策略；provider 请求会跟随 HTTP 重定向。WP1（[ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md)）已提供 `tool.Spec`/`tool.Define`、guidance 与 `Error: ` 结果格式，本 WP 基于它实现。

请求审计由 [WP15 Note](2026-10-06-web-search-request-audit.md) 与 [ADR-0022](../../../docs/decisions/0022-web-search-request-audit.md) 拥有；本 Note 保留 provider/fetch/lifecycle 的实施证据。

非目标：HTTP 代理、交互浏览、结构化来源持久化（参考 web 结果卡片的 meta）、逐 URL 授权策略和 live provider 验证。

## Decision

长期决定见 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md)，事实归 [架构](../../../docs/architecture.md#web-检索与抓取)与[安全规则](../../../docs/security.md#网络边界)。本次实施：

抓取的压缩、URL/IDNA、UTF-16 预算和并发连接回退实施证据由[传输对齐 Note](2026-10-06-web-fetch-transport-alignment.md)拥有；本 Note 保留 web 服务、工具、provider 与 composition 的实施证据。

- **插件与 composition。** `internal/app/web.Service`（ID `web`）位于 compaction 之后、sessions 之前；cleanup 先拒绝新操作，再取消全部在途操作，并等待每个操作的 provider 调用返回、操作注销。调用方在自己的 goroutine 上收到结果，可能晚于 cleanup 返回；静止保证只覆盖 service 拥有的工作。`internal/adapter/tool/web.Provider`（ID `web-tools`）位于 subagent tools 之后，Start 依次登记两个 `tool.Define` 编译的工具，每次登记由 tool runtime 在同一 Scope 中挂 cleanup；第二次登记失败时关闭该 Scope 会回收第一项。`internal/adapter/web/fetch.Client` 没有生命周期 effect（每跳 transport 在返回前关闭），作为依赖注入 `app/web`，不是插件。`cmd` 的 `dependencies` 增加 `webResolver`/`webDial`，生产为 nil。
- **llm 与 provider。** `llm.PreparedModel` 增加 `Search`，`llm.Call.Search` 校验非空查询和正的结果上限后委托 provider。`internal/adapter/model/provider/search.go` 实现三种 wire；`responsesTarget`、`anthropicHeaders` 从对话路径抽出供两者共用；通用 `send` 取代原 `streamRequest` 主体；对话、检索和 OAuth 请求都经 `Provider.do` 发出，拒绝重定向（protocol 错误）。
- **settings。** `Document.Web.Search{Provider,Model}` 默认为空，YAML/JSON 在为空时省略；两者必须同时给出且 model 在 provider 目录中。
- **工具定义。** 两个工具用 `tool.Spec` 声明，`Concurrent` 恒为 true，没有 `Approval`，也没有 `Check`（语义校验只在 `app/web`）。根对象未声明参数按 ADR-0007 被拒绝且不触达 service。`internal/app/tool/define.go` 增加参考 section 表的 `OrderWebSearch = 2000`、`OrderWebFetch = 2100`；guidance 由 `Runtime.Catalog` 渲染，prompt assembler 没有 web 专用分支。
- **证据 fixture。** `cmd/nano-harness/testdata/tool-catalog.json` 与 `upstream-base-tools.json` 收录两个工具，后者的条目已与参考 `docs/tool-catalog.md` 的 JSON 块逐项比对。
- **composition ID** 的 web 工具 token 使用 `web-tools-v2`，发送前审计语义由 ADR-0022 定义。WP12 E 批为结构化结果提升到 `web-tools-v3`，见[web 结构化结果](2026-10-07-structured-web-results.md)。
- **依赖。** `golang.org/x/net` v0.59.0 提供 HTML tokenizer 与 WHATWG charset 查找，`golang.org/x/text` v0.42.0 提供编码表；两者 Go 团队维护、BSD-3-Clause、纯 Go。`golang.org/x/sync` 作为传递依赖从 v0.22.0 升到 v0.23.0。`CGO_ENABLED=0 go build -trimpath` 的 darwin/arm64 二进制从 14,395,842 增至 15,676,146 字节（+1.28 MB，含本 WP 全部代码）。替代方案是自写 tokenizer 与多字节编码表或只支持 UTF-8，前者安全负担高，后者无法解码 GBK/Shift_JIS 等页面，与参考 `TextDecoder` 不一致。依赖只承担词法与字符集边界，Markdown 转换、元素栈和容错规则仍为本仓实现。
- **定向 mutation** 增加 `web-fetch-public-address` 与 `web-fetch-redirect-origin`。

与其他工作包共享的接触面：`internal/app/llm/runtime.go`（类型与接口）、四个 PreparedModel 测试替身（agent、compaction、subagent、llm）各加一个 `Search` 桩、`internal/app/tool/define.go`（两个 order 常量）、`internal/app/settings/settings.go`、provider 包的 `wire_common.go`/`responses.go`/`anthropic.go`、`cmd/nano-harness/application.go`/`main.go`/`main_test.go`（上游工具数量断言 7→9；e2e 复用 todo 测试的 `readTranscript`）、两个 testdata fixture 和 `scripts/mutation-cases.json`。

## Consequences

模型获得与参考 schema 一致的检索和抓取；检索复用现有账户，抓取以地址策略阻断 SSRF 与 DNS 重绑定，并在设置热重载时保持工具集合不变。对话请求也不再跟随重定向，这是对既有凭据转发风险的收紧；依赖 endpoint 重定向的部署需要改为直接配置最终 HTTPS 地址。

代价：检索默认关闭，用户必须在 `settings.yaml` 选择 route，每次检索额外计费；Codex Responses 边界对 `web_search` 工具的接受度没有 live 证据。检索 endpoint 复用所选 provider，无法独立配置，取舍见 ADR-0011。抓取不读取代理环境变量；没有逐次确认，模型仍可把数据编码进公网 URL。两个工具的文本成功结果已进入 [通用 spill 策略](2026-10-05-tool-output-spill-and-read-before-write.md)；抓取先按 ADR-0011 的 UTF-16 格式化预算限额，再保存超过内联预算的完整格式化结果，存储不可用时仍受 runtime 的字节兜底。查询空白判定的补充证据见[对齐 Note](2026-10-06-search-spill-query-parity.md)。HTML 语义与预算的实施证据由 [转换与输出预算 Note](2026-10-06-web-fetch-html-and-output-budget.md)补充，等价排版差异归 ADR-0011。旧会话因 composition ID 变化而拒绝恢复，本仓尚无发布数据。

参考 `docs/reference-deepseek-harness.md` 中“新工具暂缓”的那一行由总体计划在全部 WP 合并后更新，本 WP 未改动。

## Verification

在 worktree `wp/wp5-web` 上实际运行：

- `go test -race -count=1 ./...`：全部包通过。
- `make check`（fmt-check、mod-check、vet、race test、architecture、submodule、agent-notes、skills、workflow-tools、golangci-lint、coverage、mutation、build）：通过；`coverage: every product source file is 100.0%`，lint `0 issues`，十个 mutation 全部 killed，`./bin/nano-harness version` 输出 `nano-harness dev`。
- `make vuln`：`No vulnerabilities found.`
- `python3 scripts/mutation-check.py`：两个新变异分别被 `TestFetch_AddressPolicyMatrix`（放行私网答案后 loopback 被联系）和 `TestFetch_RefusesUnsafeRedirects`（跨源/跨 scheme 重定向返回错误代码改变）杀死。

行为证据：

- provider：`TestSearch_WireRequestsAndNormalizedResults` 对 OpenAI API key、Codex OAuth、Anthropic API key/OAuth、OpenRouter 断言精确请求体、路径、Accept 与认证头，以及归一后的回答与来源；未设置 effort 时三种 wire 都省略字段。`TestSearch_RefusesRedirectsWithoutContactingTarget` 证明检索与对话请求都不联系重定向目标。另覆盖凭据前置失败不触网、429 的 retry hint 与远端正文不泄漏、transport 失败、请求中取消，以及畸形/不完整/缺检索证据/超大响应。
- app：真实 LLM runtime 与 settings 下验证查询校验、未配置、barrier 证明并发、首个失败取消兄弟并等待、轮转合并、60 s 时限（测试缩短）、调用方取消、缺账户、prepare 失败，以及 shutdown 取消并等待在途检索和抓取。
- fetch：loopback HTTP/TLS 与注入 resolver/dialer 验证允许/拒绝矩阵、dialer 只收到已校验 IP:端口、重绑定、5 跳上限、同源/跨源、无/坏 Location、charset（GBK）、gzip 与解压炸弹截断、字节与字符截断、超时、取消、断开正文和 TLS 主机名；`TestNew_ProductionDefaultsRefuseLoopback` 用真实系统 resolver 证明 `127.0.0.1` 与 `localhost` 被拒且 server 未被联系。
- 工具：`Runtime.Catalog` 中的 schema 与参考逐字节比较，guidance 文本、顺序（检索先于抓取）和可见性变体与参考一致；真实 tool runtime 中两个工具通过 barrier 证明同一并发组、approval 从未被调用；未声明根参数、类型错误和缺失必填都返回 `Error: invalid arguments: ...` 且不调用 service，service 失败返回 `Error: <CODE>: <消息>`；检索/抓取展示与 512 层嵌套省略。
- 目录：`TestComposition_ToolCatalogGolden` 与 `TestComposition_MatchesUpstreamBaseTools` 从真实 composition 的 `request/header` 和 provider 收到的请求比较 14 个工具，其中 9 个与参考 Base 逐字节一致。
- assembled：`TestComposition_WebSearchAndFetchEndToEnd` 经真实 settings 文件与 composition 让模型一步调用两个工具，从磁盘 transcript 断言 request header 中冻结的 schema、system prompt 指引、检索来源与转换后的页面（脚本被删除），抓取只拨号 `93.184.216.34:80`；`TestComposition_WebSearchUnconfiguredFailsClosed` 证明默认配置返回 `WEB_PROVIDER_UNAVAILABLE` 且不联系 provider。
- settings 文件：未配置时不写入 `web:`；配置值往返；README 示例可解析；未知子字段被 strict YAML 拒绝。

### 关闭测试的偶发失败（2026-10-05）

WP7 在集成分支上跑全量 race 测试时，`TestService_ShutdownCancelsAndWaitsForInFlightOperations` 偶发失败，首个稳定失败特征是 `shutdown returned before search settled`；之后单独重跑时通过。

根因在测试的观察方式，不在产品代码。`Search`/`Fetch` 用 `defer done()` 在返回时执行 `group.Done()`，cleanup 的 `group.Wait()` 随之返回；旧测试在 cleanup 返回后用带 `default` 的 `select` 读取调用方 goroutine 的结果 channel，而那次发送发生在 `Search` 返回之后。`group.Done()` 与发送之间没有 happens-before，测试 goroutine 可以在另一个 P 上先到达 `select`。被测的产品保证成立：`done()` 之前，provider 调用和 `runQueries` 的子 goroutine 都已结束，结果映射也已完成，service 不再执行任何代码。

复现：在 `00803e7` 上执行 `go test -race -c -o /tmp/wp5-web.test ./internal/app/web/`，再运行 `/tmp/wp5-web.test -test.run '^TestService_ShutdownCancelsAndWaitsForInFlightOperations$' -test.count 3000 -test.cpu N`。不加外部负载时，`-cpu 1` 为 0/3000，`-cpu 2` 为 12/3000，`-cpu 8` 为 66/3000，search 和 fetch 两处断言都出现过；同时运行 `go test -race ./internal/...` 时，`-cpu 8` 为 17/1000。单 P 时被唤醒的 goroutine 通常要等发送方让出才运行，所以单独重跑难以复现。

修复只改测试：provider 替身在观察到取消后报告 `cancelled`，再阻塞到测试 `release` 才报告 `returned` 并返回。测试在另一个 goroutine 中关闭 scope，确认两个调用都已取消且 cleanup 仍未返回，并确认关闭期间的新检索被拒绝；释放后等待 cleanup 返回，再断言两个 `returned` 都已发生、操作表为空，最后阻塞读取调用方结果并检查 `WEB_ABORTED` 与 `context.Canceled`。对正确实现，每个断言都由 channel 的 happens-before 关系决定，不依赖调度。文档和 `Service` 注释写明，静止只覆盖 service 自身的工作。

修复后的证据：同一命令下，新测试在 `-cpu 1,2,8` 各 3000 次共 9000 次运行中全部通过；在同时运行 `go test -race ./internal/...` 的负载下，另外 9000 次也全部通过，而旧测试在同一负载下仍为 17/1000 失败。在私有副本中删除 cleanup 的 `group.Wait()` 后，新测试在 `-count=200 -cpu 1,2,8` 的 600 次运行中全部失败：598 次报 “still running”，2 次报 provider 调用尚未返回。这一变异的检出依赖调度，没有加入 `make mutation`，以免产生偶发的 survived。

使用私有 `GOLANGCI_LINT_CACHE` 第一次运行 `make check` 时，失败只出现在 coverage 阶段的 `TestComposition_SubagentsEndToEnd`（WP7，`subagent_test.go:270` 期望 continuable child 排在 catalog 首位）。该失败与本修复无关，在未改动的 `00803e7` 上同样出现：编译测试二进制后，`-test.count 100 -test.cpu 1,4,8` 失败 38/300，`-cpu 1` 失败 46/100。原因是模型在同一步中并发调用 `subagent` 与 `subagent_fork`，两条 `subagent/catalog` 记录写入根会话的顺序不确定，已交给 WP7 处理。第二次运行 `make check` 通过：coverage 逐文件 100%，lint `0 issues`，17 个 mutation 全部 killed，build smoke 正常。

### 整体审查修复（2026-10-06）

整体审查在 `d9ad07b` 上发现三个问题，修复都先用稳定失败的测试复现：

- **B1：IPv6 字面量绕过 NAT64 校验。** 旧 `resolve` 对 IP 字面量只做 `publicAddress` 就返回，NAT64 发现只在主机名路径执行。网络使用 `2000::/3` 内的 network-specific DNS64 前缀时，`http://[<pref64>::0a00:0001]/` 这类字面量通过公网校验，再经 NAT64 网关到达 `10.0.0.1`。参考 `network.ts` 把字面量放入同一答案集再检查。新增的 `TestFetch_RejectsNAT64TranslatedLiterals` 为 /32、/40、/48、/56、/64、/96 六种布局各构造一个发现前缀和嵌入 `10.0.0.1` 的字面量，修复前首个被检查的字面量即返回 `err=<nil>`。现在字面量与解析答案共用同一个校验循环和 NAT64 检查：六个字面量都以 `WEB_BLOCKED_URL` 拒绝且未拨号，嵌入 `8.8.8.8` 的字面量允许，每个 IPv6 字面量都会查询 `ipv4only.arpa`，发现失败时以 `WEB_PROVIDER_ERROR` 关闭，IPv4 字面量不触发发现。
- **S1：OAuth 令牌请求跟随重定向。** `doOAuth` 直接用 `provider.client.Do`，307/308 会把 refresh token、code verifier 或 key 交换参数重发到 Location。新增的 `TestOAuth_RefusesRedirectsWithoutContactingTarget` 修复前因目标返回空响应而得到普通 protocol 错误，证明请求被转发。现在 `send` 与 `doOAuth` 共用 `Provider.do`，表单与 JSON 两种 OAuth 请求都返回 `errProviderRedirect`，重定向目标收到 0 次请求。
- **S3：非 IP 解析答案的错误类别。** 旧实现把非 IP 答案当作非公网地址，返回 `WEB_BLOCKED_URL`；参考返回 `WEB_PROVIDER_ERROR`（resolved to an invalid IP address）。现已对齐参考，`TestFetch_AddressPolicyMatrix` 的期望随之修改，修复前该用例失败。

定向 mutation：`web-fetch-public-address` 同时运行字面量测试；新增 `web-fetch-literal-policy`（恢复字面量提前返回，即 B1 本身）和 `web-fetch-nat64-translation`（禁用 NAT64 拒绝），三者都被杀死。`docs/security.md`、`docs/architecture.md`、`docs/testing.md` 与 ADR-0011 第 4、6 条已同步。

未获得的证据：没有对 OpenAI、Codex、Anthropic 或 OpenRouter 的 live 检索调用，也没有访问真实公网页面；`make tui-e2e` 与跨平台构建未在本 WP 运行。

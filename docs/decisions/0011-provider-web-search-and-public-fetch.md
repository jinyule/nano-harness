# ADR-0011：web 检索复用 provider 服务端检索，抓取只到公网 HTTP(S)

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers（“web_search 复用已配置 LLM provider 的服务端检索、不新增凭据”由维护者确定）

## 背景

[工具对齐计划](../../.agents/notes/proposed/2026-10-04-upstream-tool-parity.md)要求补齐参考 Base 组合的 `web_search` 与 `web_fetch`，模型可见定义与参考默认组合一致。参考实现（`third_party/deepseek-harness/packages/web/`，提交 `5badb15009ae`）把能力拆为 `ctx.web` 服务、检索/抓取 provider 和 `tool-web` 消费方：Base 用 DeepSeek 的 Anthropic 兼容 Messages API 加 `web_search_20250305` 服务端工具检索（`searchTimeoutMs: 60000`），匿名抓取只到公网 HTTP(S)，逐个解析并校验目标并固定实际连接；两者都不需要逐次确认，结果是外部不可信数据。

本仓已有 OpenAI（含 ChatGPT Codex Responses 边界）、Anthropic 与 OpenRouter 三个 provider，它们各自提供服务端检索。本仓没有 web 能力、HTML 解析或字符集解码依赖，也没有可直接复用的网络地址策略。

非目标：交互浏览、内容抽取模型、按 URL 授权策略、HTTP 代理路由，以及把结构化来源作为独立持久化记录。

## 决策

1. **定义与调度。** 两个工具用 [ADR-0007](0007-upstream-base-tool-definitions.md) 的 `tool.Spec`/`tool.Define` 声明。`web_search` 的描述和参数 schema 为 `{"type":"object","properties":{"queries":{"type":"array","description":"1–4 search queries; their results are merged.","items":{"type":"string"}}},"required":["queries"]}`，`web_fetch` 为 `{"type":"object","properties":{"url":{"type":"string","description":"The HTTP(S) URL to fetch."}},"required":["url"]}`，与参考生成目录逐字节一致，并收录在 `cmd/nano-harness/testdata/upstream-base-tools.json`。参考根对象对未知成员开放；本仓按 ADR-0007 的统一规则拒绝未声明的根成员，schema 不变。两者始终注册、`Concurrent` 恒为 true、不请求 approval，delegated agent 同样可用。
2. **能力接缝。** `internal/app/web.Service` 是插件，拥有查询校验、检索合并、时限、错误分类和在途操作的取消与等待；它依赖具体的 `llm.Runtime` 与 `settings.Service`，抓取通过自身定义的 `Fetcher` 接口注入。`internal/adapter/tool/web` 定义消费的 `Service` 小接口，只负责定义、guidance 和展示；查询数量、空白查询和空 URL 等语义校验只由 `app/web` 负责。`internal/adapter/web/fetch` 是无连接池、无生命周期 effect 的抓取实现，由 `cmd` 注入。当前每种能力只有一个实现，不建立 provider 注册表。
3. **检索 route。** settings 新增可选 `web.search.provider/model`，两者同时给出，provider 必须是已安装的三者之一且 model 在其目录中，否则加载失败。默认未配置：每次检索都是一次额外计费的模型请求，计费账户和模型必须由用户显式选择，不回退到会话 route。未配置时工具仍注册，以保持 schema 和 prompt 在热重载中稳定，调用返回 `WEB_PROVIDER_UNAVAILABLE`，与参考“已启用工具在 provider 不可用时可见并在执行时失败”一致。
4. **provider wire。** `llm.PreparedModel` 增加 `Search`，`llm.Call.Search` 复用 `PrepareCall` 冻结的 endpoint、目录项（含 `effort`，映射同 [ADR-0004](0004-provider-neutral-effort.md)）和刷新后的账户。三个 provider 都实现它，提示词沿用参考的 `Perform a web search for the query: <query>`：
   - OpenAI Responses 与 Codex Responses：`tools: [{"type":"web_search"}]`、`tool_choice: "auto"`、`stream: true`、`store: false`，带固定简短 instructions；读取 `response.output_item.done`，必须出现 `web_search_call`，回答取 `output_text`，来源取 `url_citation`。
   - Anthropic Messages：与参考相同的非流式请求体（`max_tokens: 4096`，`web_search_20250305`，`max_uses: 5`）；必须出现 `web_search_tool_result`，来源取 `web_search_result`，片段取 citation 的首个 `cited_text`，回答取 text 块；全部结果块为工具错误时，`too_many_requests` 映射限流、`unavailable` 映射服务端、其余映射非法请求。
   - OpenRouter Chat Completions：非流式 `openrouter:web_search` server tool，`max_results` 为 8；回答取 message content，来源取 `url_citation`。
   每个响应的回答最多 `session.MaxTextBytes`，来源按 URL 去重、跳过空或超过 4096 字节的 URL，最多 64 条。检索不自动重试。所有 provider 请求（包括既有对话请求和 OAuth 令牌、key 交换请求）拒绝跟随重定向，归类为 protocol 错误，以免把凭据、OAuth 秘密或请求体转发到其他 URL。
5. **检索语义。** 一次调用接受 1–4 个非空查询并折叠精确重复项；只准备一次账户，查询并发执行，首个失败取消其余并在全部结束后返回。单查询来源截到 8 条；多查询先逐个截断，再按 rank 轮转合并、按 URL 去重并截到 8 条，回答以 `### <查询>` 标注。整个调用限时 60 s，与 Base 相同。
6. **抓取策略。** URL 最长 2048 字节、只允许 HTTP(S)、拒绝 userinfo；每一跳确定目的地址集合（IP 字面量即其本身，主机名取全部解析答案，与参考把字面量放入同一答案集一致），拒绝任何非全局单播地址（IPv4-mapped 按内嵌 IPv4 判断；集合含 IPv6，包括 IPv6 字面量时，按 RFC 7050 发现 DNS64 前缀并拒绝翻译到非公网 IPv4 的地址；解析器返回非 IP 答案时与参考一样以 `WEB_PROVIDER_ERROR` 失败），只拨号已校验的 IP:端口，TLS 仍按 URL 主机名校验。每跳使用独立 transport 并在结束时关闭；最多 5 次同源重定向，跨源重定向要求模型另发调用；不发送 cookie 或凭据，不读取代理环境变量。总时限 30 s，原始正文最多 5,000,000 字节（声明超限失败，流式或解压超限截断），解码文本最多 100,000 个字符；只接受文本类内容，charset 按 WHATWG 标签解码。这些都是固定安全上限，不是部署设置。
7. **展示。** 工具输出以参考的外部内容说明开头。检索输出包含可选回答、`- [标题或主机名](URL) — 片段 (日期)` 来源列表、截断提示和引用要求；抓取输出为 `Fetched <url> (HTTP <status>)`、说明和正文，HTML 转为 Markdown 并删除脚本、样式、嵌入对象与隐藏元素，嵌套超过 512 层时输出固定省略标记。完整输出不超过 `session.MaxTextBytes`，截断时附参考提示。两个工具通过 `tool.Guidance` 贡献参考的 `tool:web_search`、`tool:web_fetch` 段落，order 取参考 section 表的 `TOOL_WEB_SEARCH: 2000`、`TOOL_WEB_FETCH: 2100`，由 `Runtime.Catalog` 只在工具可见时渲染；检索段落只在 `web_fetch` 同时可见时建议用它抓取全文。
8. **错误。** `app/web.Error` 携带稳定代码（`WEB_PROVIDER_UNAVAILABLE`、`WEB_PROVIDER_CREDENTIAL_MISSING`、`WEB_PROVIDER_ERROR`、`WEB_ABORTED`、`WEB_SEARCH_TIMEOUT`、`WEB_INVALID_URL`、`WEB_BLOCKED_URL`、`WEB_REDIRECT_BLOCKED`、`WEB_FETCH_TOO_LARGE`、`WEB_FETCH_TIMEOUT`、`WEB_UNSUPPORTED_CONTENT_TYPE`）和不含凭据或远端错误正文的消息，经 tool runtime 以上游 `Error: <message>` 格式进入结果，即 `Error: <CODE>: <消息>`。时限与取消先按操作 context 判断，再看 provider 错误类别。
9. **持久化。** 不新增 session 记录或字段：schema 在 `request/header` 冻结，渲染文本在 `tool/result` 中。composition ID 增加 `web-tools-v1`，旧会话按现有严格规则拒绝恢复；本仓尚无发布 tag，没有已发布用户数据需要迁移。settings 新字段在未配置时不写入 YAML，未知子字段被 strict decoder 拒绝。
10. **依赖。** 新增 `golang.org/x/net` v0.59.0（HTML tokenizer 与 `html/charset`）和 `golang.org/x/text` v0.42.0（WHATWG 编码），均为 Go 团队维护、BSD-3-Clause、无 cgo；`golang.org/x/sync` 作为传递依赖从 v0.22.0 升到 v0.23.0。它们替代自写 HTML 解析和多字节编码表。

## 后果

模型获得与参考一致的检索与抓取工具，三个 provider 共享同一消费方契约，provider wire 和账户仍由 provider 拥有。检索复用现有账户，不新增凭据面；抓取以可测试的地址策略阻断 SSRF 与 DNS 重绑定。工具在设置变化时 schema 不变，失败以稳定代码返回给模型。

代价与风险：每次检索向所选账户额外计费；Codex Responses 边界是否接受 `web_search` 工具只有协议测试证据，尚无 live 验证。抓取不支持 HTTP 代理，需要代理的网络中会连接失败。没有逐次确认，模型可以把数据编码进公网 URL 外发；本决策只防止访问非公网目的地。结构化来源只存在于渲染文本，UI 不能重建参考的 web 结果卡片。provider 检索工具版本（如 `web_search_20250305`）或 annotation 形状变化时，需要同步更新协议测试与本 ADR。

## 被否决方案

- **新增独立检索服务与凭据（Exa、Perplexity 等）**：维护者已确定复用现有 provider，新凭据扩大存储和泄露面。
- **默认使用会话 route 检索**：会把计费账户和模型隐式绑定到当前对话，热切换 route 时检索行为随之改变。
- **未配置时不注册工具**：设置热重载会改变模型可见 schema 和 system prompt，与参考可见性语义不一致。
- **以可选接口探测 provider 是否支持检索**：三个已安装 provider 都支持，类型断言只会留下不可达分支；未来 provider 必须显式实现 `Search`。
- **共享带连接池的 transport**：池化连接的地址校验与后续请求解耦，需要额外的生命周期与缓存所有权；逐跳独立 transport 更简单且没有残留连接。
- **自动跟随跨源重定向并重新校验**：参考要求新调用，让模型看到并决定新源；自动跟随会扩大一次调用可触达的目的地。
- **使用 `ProxyFromEnvironment`**：代理代为解析 DNS，会绕过地址校验和固定连接；需要代理时另行设计。
- **逐次 approval**：参考在所有沙箱与审批模式下都不确认 web 工具；只读检索与公网 GET 的边界由地址策略承担，需要确认的部署应新增执行点策略。
- **自写 HTML 解析与字符集解码**：解析容错和多字节编码表的自有代码与安全负担明显高于成熟的 Go 团队依赖。

## 复审触发条件

provider 删除或替换服务端检索工具、Codex Responses live 验证拒绝 `web_search`、用户需要 HTTP 代理或逐次确认外联、UI 需要持久化结构化来源、新增 provider，或参考实现改变 web 工具 schema、上限与可见性语义时，重新评估本决策。

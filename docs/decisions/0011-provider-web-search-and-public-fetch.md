# ADR-0011：web 检索复用 provider 服务端检索，抓取只到公网 HTTP(S)

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers（“web_search 复用已配置 LLM provider 的服务端检索、不新增凭据”由维护者确定）

## 背景

[工具对齐计划](../../.agents/notes/implemented/2026-10-04-upstream-tool-parity.md)要求补齐参考 Base 组合的 `web_search` 与 `web_fetch`，模型可见定义与参考默认组合一致。参考实现（`third_party/deepseek-harness/packages/web/`，提交 `5badb15009ae`）把能力拆为 `ctx.web` 服务、检索/抓取 provider 和 `tool-web` 消费方：Base 用 DeepSeek 的 Anthropic 兼容 Messages API 加 `web_search_20250305` 服务端工具检索（`searchTimeoutMs: 60000`），匿名抓取只到公网 HTTP(S)，逐个解析并校验目标并固定实际连接；两者都不需要逐次确认，结果是外部不可信数据。

本仓已有 OpenAI（含 ChatGPT Codex Responses 边界）、Anthropic 与 OpenRouter 三个 provider，它们各自提供服务端检索。本仓没有 web 能力、HTML 解析或字符集解码依赖，也没有可直接复用的网络地址策略。

非目标：交互浏览、内容抽取模型、按 URL 授权策略、HTTP 代理路由，以及把结构化来源作为独立持久化记录。

## 决策

1. **定义与调度。** 两个工具用 [ADR-0007](0007-upstream-base-tool-definitions.md) 的 `tool.Spec`/`tool.Define` 声明。`web_search` 的描述和参数 schema 为 `{"type":"object","properties":{"queries":{"type":"array","description":"1–4 search queries; their results are merged.","items":{"type":"string"}}},"required":["queries"]}`，`web_fetch` 为 `{"type":"object","properties":{"url":{"type":"string","description":"The HTTP(S) URL to fetch."}},"required":["url"]}`，与参考生成目录逐字节一致，并收录在 `cmd/nano-harness/testdata/upstream-base-tools.json`。参考根对象对未知成员开放；本仓按 ADR-0007 的统一规则拒绝未声明的根成员，schema 不变。两者始终注册、`Concurrent` 恒为 true、不请求 approval，delegated agent 同样可用。
2. **能力接缝。** `internal/app/web.Service` 是插件，拥有查询校验、检索合并、时限、错误分类和在途操作的取消与等待；它依赖具体的 `llm.Runtime` 与 `settings.Service`，抓取通过自身定义的 `Fetcher` 接口注入。`internal/adapter/tool/web` 定义消费的 `Service` 小接口，只负责定义、guidance 和展示；查询数量、空白查询和空 URL 等语义校验只由 `app/web` 负责。`internal/adapter/web/fetch` 是无连接池、无生命周期 effect 的抓取实现，由 `cmd` 注入。当前每种能力只有一个实现，不建立 provider 注册表。
3. **检索 route。** settings 新增可选 `web.search.provider/model`，两者同时给出，provider 必须是已安装的三者之一且 model 在其目录中，否则加载失败。默认未配置：每次检索都是一次额外计费的模型请求，计费账户和模型必须由用户显式选择，不回退到会话 route。未配置时工具仍注册，以保持 schema 和 prompt 在热重载中稳定，调用返回 `WEB_PROVIDER_UNAVAILABLE`，与参考“已启用工具在 provider 不可用时可见并在执行时失败”一致。检索复用所选 provider 的 endpoint，不能独立配置检索 endpoint；参考 DeepSeek 检索 provider 支持独立 endpoint。本仓用同一冻结 route 复用认证、传输与配置校验，避免增加第二套 endpoint 配置和路由状态；代价是修改该 provider 的 endpoint 会同时影响其对话与检索，无法单独把检索送往另一个网关。
4. **provider wire。** `llm.PreparedModel` 增加 `Search`，`llm.Call.Search` 复用 `PrepareCall` 冻结的 endpoint、目录项（含 `effort`，映射同 [ADR-0004](0004-provider-neutral-effort.md)）和刷新后的账户。三个 provider 都实现它，提示词沿用参考的 `Perform a web search for the query: <query>`：
   - OpenAI Responses 与 Codex Responses：`tools: [{"type":"web_search"}]`、`tool_choice: "auto"`、`stream: true`、`store: false`，带固定简短 instructions；读取 `response.output_item.done`，必须出现 `web_search_call`，回答取 `output_text`，来源取 `url_citation`。
   - Anthropic Messages：与参考相同的非流式请求体（`max_tokens: 4096`，`web_search_20250305`，`max_uses: 5`）；必须出现 `web_search_tool_result`，来源取 `web_search_result`，片段取 citation 的首个 `cited_text`，回答取 text 块；全部结果块为工具错误时，`too_many_requests` 映射限流、`unavailable` 映射服务端、其余映射非法请求。
   - OpenRouter Chat Completions：非流式 `openrouter:web_search` server tool，`max_results` 为 8；回答取 message content，来源取 `url_citation`。
   每个响应的回答最多 `session.MaxTextBytes`，来源按 URL 去重、跳过空或超过 4096 字节的 URL，最多 64 条。检索不自动重试。所有 provider 请求（包括既有对话请求和 OAuth 令牌、key 交换请求）拒绝跟随重定向，归类为 protocol 错误，以免把凭据、OAuth 秘密或请求体转发到其他 URL。
5. **检索语义。** 一次调用接受 1–4 个按 ECMAScript `trim()` 空白集判定非空的查询（共享 `core/text` 的空白集合，含 U+FEFF、不含 U+0085），保留原文并折叠精确重复项；只准备一次账户，查询并发执行，首个失败取消其余并在全部结束后返回。单查询来源截到 8 条；多查询先逐个截断，再按 rank 轮转合并、按 URL 去重并截到 8 条，回答以 `### <查询>` 标注。整个调用限时 60 s，与 Base 相同。
6. **抓取策略。** URL 最长 2048 个 UTF-16 code unit、只允许 HTTP(S)、拒绝 userinfo；URL/IDNA 规范化后，每一跳确定目的地址集合（IP 字面量即其本身，主机名取全部解析答案，与参考把字面量放入同一答案集一致），拒绝任何非全局单播地址（IPv6 只接受 `2000::/3` 全球单播块内、不属于特殊用途范围的地址；上游 `ipaddr.js` 的 `unicast` 分类还接受块外的未分配地址，例如 `4000::1`，本仓更严格：全球单播只从该块分配，块外没有可达的公网目的地，按块允许使未来的特殊用途分配默认被拒绝；IPv4 拒绝范围与上游一致。IPv4-mapped 按内嵌 IPv4 判断；集合含 IPv6，包括 IPv6 字面量时，按 RFC 7050 发现 DNS64 前缀并拒绝翻译到非公网 IPv4 的地址；解析器返回非 IP 答案时与参考一样以 `WEB_PROVIDER_ERROR` 失败），并发回退只拨号已校验的 IP:端口，TLS 仍按 URL 主机名校验。每跳使用独立 transport 并在结束时关闭；最多 5 次同源重定向，跨源重定向要求模型另发调用；不发送 cookie 或凭据，不读取代理环境变量。总时限 30 s，最多 5 个 Content-Encoding 项，编码非空时的网络输入、每个中间解压流与最终正文各限 5,000,000 字节（声明、编码输入或中间层超限失败，最终正文流式超限截断），解码文本最多 100,000 个 UTF-16 code unit；只接受文本类内容，charset 按 WHATWG 标签解码。这些都是固定安全上限，不是部署设置；细节与上游取舍见下文。
7. **展示。** 工具输出包含参考的外部内容说明。检索输出包含可选回答、`- [标题或主机名](URL) — 片段 (日期)` 来源列表、截断提示和引用要求；抓取输出为 `Fetched <url> (HTTP <status>)`、说明和正文。HTML 转为 Markdown，保留删除线、任务框的 checked 状态、代码块语言和代码中的空白；普通文本及链接/图片标签中的 Markdown 字面量转义，代码内容不转义，反引号围栏避开正文中的反引号。树由 `golang.org/x/net/html` 的解析器按 HTML tree construction 构建：片段在 body 上下文中解析，与参考把输入包在 body 内元素中一致；脚本标志关闭，与参考 domino 的解析一致，因此 noscript 内是普通标记。隐式结束标签、自闭合标记（HTML 中非 void 元素的 `/>` 被忽略，SVG/MathML 中关闭元素）、raw-text、foreign content 与 integration point、CDATA，以及误嵌套格式元素的重建与 adoption agency 都由解析器处理；转换器只遍历树，删除脚本、样式、嵌入对象与隐藏元素。因此被外层结束标签或隐式结束关闭的隐藏格式元素会带着属性重建，其后的文本仍隐藏。解析器拒绝开放元素超过 512 个（片段根计入）的输入，此时输出固定省略标记；参考按词法计数未闭合的开始标签，超过 512 时省略，两者计数方式不同。深度上限不约束累计建树量：属性各不相同的格式元素都留在活动格式元素列表中，后续每个块都会重建它们，几 KB 输入可展开为数十万个节点，且建树发生在隐藏内容删除和输出截断之前。因此建树前先做一次词法扫描：每个开始标签（含自闭合标签，HTML 忽略其斜杠）计一个元素，再加上词法栈上仍打开的格式元素数；只有匹配栈顶的结束标签出栈，不模拟 Noah's Ark 和 marker，畸形输入只会高估。脚本标志关闭，noscript 内容按标记扫描。累计超过固定上限 65,536 时输出同一省略标记，不建树。上限是安全不变量，不提供配置；在 200,000 单元输入上限下，已测真实页面得分不超过约 7,400，接受的树约为 13 万个节点以内。参考的词法深度 guard 只约束深度，同类放大样本在参考 domino 中同样生成约 50 万个节点；本仓的成本上限比参考更严。转换不接收 context：扫描为线性，接受的输入建树量有固定上界，是有界的同步 CPU 步骤；抓取在转换前和 runtime 在转换后照常处理取消。

   已知与参考的差异如下，测试同时记录参考的实际输出。domino 2.2.0 不把自闭合 raw-text 标签记为最后的开始标签，因此 `<script/>`、`<style/>`、`<iframe/>`、`<textarea/>`、`<title/>` 之后的原始文本会在错误的结束标记处结束：吞掉页面剩余内容、输出字面结束标记，或在祖先的结束标记处结束并泄漏脚本文本；本仓遵循 HTML 标准。domino 实现的是加入“SVG/MathML 中的 `</p>`、`</br>` 结束 foreign 内容”规则之前的标准，外层隐藏元素保持打开；本仓遵循当前标准，其后文本在浏览器中同样可见。参考的移除规则按大写 HTML 节点名比较，会保留 SVG/MathML 中 script 与 style 的文本；本仓在所有命名空间中移除它们。x/net/html 在 SVG 或 MathML 元素打开时处理 template 开始标签，会忽略其后的全部输入（其源码注明的偏差）；本仓保留已解析的内容，并在词法 template 开始标签多于解析出的 template 元素时追加省略标记。普通 tokenizer 把 SVG title、style 当作原始文本，其中的 template 不会被计数，这种情况下丢弃的内容不带标记。

   与 Turndown 对照的排版差异限于等价的列表标记间距与嵌套缩进、强调/分隔线标记、表格单元格填充、引用空行的尾部空格、块间空行，以及省略内容为空的格式元素（参考输出 `~~~~`、`[](url)` 等空包装）；硬换行和代码内空白不属于可忽略的排版。

   `web_fetch` 的转换输入和完整格式化输出上限均为参考的 200,000 个 UTF-16 code unit；完整预算包含标题、说明、正文和截断提示，超限时预留参考 footer。截断在 UTF-8 rune 边界进行，补充平面字符占两个单元；若最后只剩一个单元则省略整个字符，避免半个 surrogate 破坏 UTF-8。格式化结果随后进入 [ADR-0008](0008-tool-output-spill-and-observation-policy.md) 的通用 spill 策略：超过内联预算时保存完整的有界格式化结果，再产生预览；工具层不先按 256 KiB 截断。没有 store、没有会话或保存失败时仍按 runtime 的 256 KiB 兜底，不能保证完整正文可读回。

   两个工具通过 `tool.Guidance` 贡献参考的 `tool:web_search`、`tool:web_fetch` 段落，order 取参考 section 表的 `TOOL_WEB_SEARCH: 2000`、`TOOL_WEB_FETCH: 2100`，由 `Runtime.Catalog` 只在工具可见时渲染；检索段落只在 `web_fetch` 同时可见时建议用它抓取全文。
8. **错误。** `app/web.Error` 携带稳定代码（`WEB_PROVIDER_UNAVAILABLE`、`WEB_PROVIDER_CREDENTIAL_MISSING`、`WEB_PROVIDER_ERROR`、`WEB_REQUEST_RECORD_FAILED`、`WEB_ABORTED`、`WEB_SEARCH_TIMEOUT`、`WEB_INVALID_URL`、`WEB_BLOCKED_URL`、`WEB_REDIRECT_BLOCKED`、`WEB_FETCH_TOO_LARGE`、`WEB_FETCH_TIMEOUT`、`WEB_UNSUPPORTED_CONTENT_TYPE`）和不含凭据或远端错误正文的消息，经 tool runtime 以上游 `Error: <message>` 格式进入结果，即 `Error: <CODE>: <消息>`；结果同时持久化 `WebError` 与同一代码，映射见 [ADR-0019](0019-structured-tool-results.md)。时限与取消先按操作 context 判断，再看 provider 错误类别。
9. **持久化。** schema 在 `request/header` 冻结，渲染文本在 `tool/result` 中。发送前的检索请求另以 `web/search-request` 审计，记录失败不发送；字段、恢复与版本契约由 [ADR-0022](0022-web-search-request-audit.md) 取代本条最初的不新增记录决定。两个工具的成功结果另带 ADR-0019 的 metadata（检索来源、回答与截断；抓取的最终 URL、状态与实际截断），不进入模型输入。web 工具 token 由 ADR-0022 提升为 `web-tools-v2`、ADR-0019 提升为 `web-tools-v3`，当前值以 `cmd/nano-harness/main.go` 的 `compositionID` 为准，旧会话按现有严格规则拒绝恢复；本仓尚无发布 tag，没有已发布用户数据需要迁移。settings 新字段在未配置时不写入 YAML，未知子字段被 strict decoder 拒绝。
10. **依赖。** 新增 `golang.org/x/net` v0.59.0（HTML 解析器、tokenizer 与 `html/charset`）和 `golang.org/x/text` v0.42.0（WHATWG 编码），均为 Go 团队维护、BSD-3-Clause、无 cgo；`golang.org/x/sync` 作为传递依赖从 v0.22.0 升到 v0.23.0。依赖替代的是自写的 HTML 树构建、tokenizer、charset 标签查找和多字节编码表；HTML 转 Markdown 与展示规则仍由 `internal/adapter/tool/web` 自写实现，以参考行为的表驱动测试约束，不采用 Turndown。

### 抓取传输语义

- **压缩。** 参考锁定 Undici 8.10.0：gzip/deflate 解压之外，HTTPS 还声明 br/zstd，deflate 同时识别 zlib 和 raw stream。本仓采用 Go 标准库 gzip/zlib/flate，HTTP(S) 均只声明 `gzip, deflate`，兼容 `x-gzip` 与已支持编码的逆序叠加；未声明的 br/zstd 与未知、空编码项以 `WEB_PROVIDER_ERROR` 拒绝。服务端遵守协商时可返回已支持编码或 identity；仅提供 Brotli/zstd 的资源不可抓取。增加 Brotli/zstd 依赖会扩大解码器和供应链审计面，当前无需为可协商的优化承担这一成本；协议测试必须证明不把压缩字节当作文本。标准库严格校验头、流完整性与 checksum；与 Undici 的宽松 finish-flush 不同，截断或损坏的压缩流失败。多层编码按 [Undici 8.10.0 的实现](https://github.com/nodejs/undici/blob/v8.10.0/lib/web/fetch/index.js#L2069-L2129)逆序处理，最多 5 个声明项（含 identity），超限在读取正文前以 `WEB_FETCH_TOO_LARGE` 拒绝。参考 `web-fetch-http/src/provider.ts` 的 `readCapped` 只限制声明的 Content-Length 和 Undici 解压后的正文，没有独立的压缩网络输入预算；Undici 使用有背压的解码管道，也没有逐层累计字节预算。本仓在非空 Content-Encoding（含 identity）下限制网络输入为 5,000,000 字节，每个中间解压流另有同样预算，包含被消费却不产生最终正文的空 gzip member 头尾。超限以 `WEB_FETCH_TOO_LARGE` 拒绝，消息沿用 `response exceeds the maximum of 5000000 bytes`，原始原因保留在错误链。公网服务端可以用大量空 member 消耗带宽和 CPU，30 s 时限不能限制时限内的累计输入，因此采用严格于上游的字节预算，不能仅凭最终正文很小判定安全。预算针对 HTTP body 的编码实体，不含传输 framing；读取额外最多一字节识别超限，恰好达到上限不误报。Content-Encoding 缺省时保留最终正文的流式截断语义。最终解压输出仍在 charset 解码前限制为 5,000,000 字节，流式超限返回有界正文并标记 `Truncated=true`，恰好达到上限不误报。
- **解码取消。** 网络源与每个解压输出在读取前后检查操作 context，单次解码输出读取最多 32 KiB。中间层输入同样受检查，连续空 gzip member 即使没有最终输出，也不能跳过取消检查。调用方或 shutdown 取消返回 `WEB_ABORTED`，抓取自身期限返回 `WEB_FETCH_TIMEOUT`，优先于大小或 codec 错误；保留原始错误原因。解码由抓取同步执行，退出时关闭全部已创建 decoder，没有取消后仍运行的解码 goroutine。检查点之间的标准库解码与 OS 调度可能使返回略晚于期限，不宣称硬实时中断。
- **URL 与 IDNA。** 保留 `net/url` 的严格语法，复用现有 `golang.org/x/net/idna.Lookup` 的 UTS #46 映射，不引入完整 WHATWG URL parser。接受首尾 U+0000–U+0020 空白和 `http:example.com`；域名转为 ASCII/punycode，主机/scheme 小写、默认端口省略、空路径补 `/`，literal/percent-encoded 点段折叠，路径、查询和 fragment 的非 ASCII 字节编码，已存在的转义与 escaped slash 保留。HTTP Host、TLS、同源校验、解析器输入和结果 URL 使用同一规范化对象。长度与参考一样在规范化前按 UTF-16 计数；重定向的 resolved URL 则在发送前重新检查。
- **严格 URL 取舍。** 拒绝内部控制字符、反斜杠、无效 UTF-8、host/path/fragment 的无效 percent escape、缺失 authority 的 `/` 变体、非规范 IPv4 数字拼写（缩写、整数、八进制、十六进制）、IDNA lookup 拒绝的域名（例如下划线或非法 joiner）。这些拼写需要额外的 WHATWG 宽松规则，继续拒绝可避免 DNS 与 HTTP 对同一个输入作不同解释；IDNA 映射后的标准 IP 字面量仍必须通过完整公网/NAT64 校验。空 userinfo 和端口 0 也继续拒绝。少见路径/fragment 标点按 Go 的较严格转义序列化，空 fragment 的尾部 `#` 不保留；不承诺这些拼写与 WHATWG 字节一致。
- **字符预算。** 正文和 URL 上限均按 UTF-16 code unit，补充平面字符计两个单元。正文截断保持有效 UTF-8 与完整 Unicode scalar；上限落在代理对中间时省略整个字符并返回 `Truncated=true`，可能比参考 `slice` 少一个单元。这个差异避免把孤立代理项或替换字符交给模型。
- **连接回退与所有权。** 候选只来自当跳已完整校验的集合，保留族内顺序、交替地址族，以 250 ms 间隔启动候选，失败立即推进；首个成功取消其余拨号并等待全部结束，关闭所有未采用连接。注入 dialer 必须支持并发和 context 取消。拨号发生在调用方同步路径，避免 Transport 请求取消先返回而 DialContext 仍在后台运行。连接和 goroutine 由单次抓取操作拥有，`app/web` 的 Scope cleanup 取消并等待该操作；没有新的长驻组件或连接池。

## 后果

模型获得与参考一致的检索与抓取工具，三个 provider 共享同一消费方契约，provider wire 和账户仍由 provider 拥有。检索复用现有账户，不新增凭据面；抓取以可测试的地址策略阻断 SSRF 与 DNS 重绑定。工具在设置变化时 schema 不变，失败以稳定代码返回给模型。

代价与风险：每次检索向所选账户额外计费；Codex Responses 边界是否接受 `web_search` 工具只有协议测试证据，尚无 live 验证。抓取不支持 HTTP 代理，需要代理的网络中会连接失败。没有逐次确认，模型可以把数据编码进公网 URL 外发；本决策只防止访问非公网目的地。结构化来源与回答由 [ADR-0019](0019-structured-tool-results.md) 的 metadata 持久化，但尚无 UI 卡片消费它们。provider 检索工具版本（如 `web_search_20250305`）或 annotation 形状变化时，需要同步更新协议测试与本 ADR。

## 被否决方案

- **新增独立检索服务与凭据（Exa、Perplexity 等）**：维护者已确定复用现有 provider，新凭据扩大存储和泄露面。
- **默认使用会话 route 检索**：会把计费账户和模型隐式绑定到当前对话，热切换 route 时检索行为随之改变。
- **未配置时不注册工具**：设置热重载会改变模型可见 schema 和 system prompt，与参考可见性语义不一致。
- **以可选接口探测 provider 是否支持检索**：三个已安装 provider 都支持，类型断言只会留下不可达分支；未来 provider 必须显式实现 `Search`。
- **共享带连接池的 transport**：池化连接的地址校验与后续请求解耦，需要额外的生命周期与缓存所有权；逐跳独立 transport 更简单且没有残留连接。
- **自动跟随跨源重定向并重新校验**：参考要求新调用，让模型看到并决定新源；自动跟随会扩大一次调用可触达的目的地。
- **使用 `ProxyFromEnvironment`**：代理代为解析 DNS，会绕过地址校验和固定连接；需要代理时另行设计。
- **逐次 approval**：参考在所有沙箱与审批模式下都不确认 web 工具；只读检索与公网 GET 的边界由地址策略承担，需要确认的部署应新增执行点策略。
- **自写 HTML tokenizer 与字符集解码表**：词法解析、charset 标签和多字节编码表的维护负担高于成熟的 Go 团队依赖；采用依赖不免除本仓转换器的容错责任。
- **建树后按节点数检查，或只采用参考的词法深度 guard**：完整的树在检查前已经分配，事后检查太晚；深度 guard 不计累计重建量，无法限制放大。
- **在 tokenizer 之上自写树构建**（此前的实现）：自闭合标记、foreign content 规则和误嵌套格式元素先后出现隐藏内容泄漏；补齐格式元素重建与 adoption agency 需要在流式渲染中移动已经输出的子树。x/net/html 的解析器实现完整树构建，唯一已知的相关偏差（foreign 内容中的 template）只会少输出内容，并已加省略标记。

## 复审触发条件

provider 删除或替换服务端检索工具、Codex Responses live 验证拒绝 `web_search`、用户需要 HTTP 代理或逐次确认外联、UI 需要结构化来源卡片、新增 provider，或参考实现改变 web 工具 schema、上限与可见性语义时，重新评估本决策。

# ADR-0022：web_search 在发送前持久化检索请求意图

- 状态：Accepted
- 日期：2026-10-06
- 决策者：nano-harness maintainers（发送前追加、记录失败不发送由维护者确定）

## 背景

[ADR-0011](0011-provider-web-search-and-public-fetch.md) 定义三个 provider 的服务端检索，但仅用 `tool/call` 和 `tool/result` 保存参数与展示。检索 route 独立于会话 route，模型目录与账户决定实际 endpoint 和预算，单靠调用参数不能审计实际请求。

只读参考提交 `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 的 `packages/web/web-search-deepseek/src/provider.ts` 定义 `DeepSeekSearchLlmRequest`：resolved endpoint、`apiVersion` 与精确 body（model、max_tokens、查询生成的 user message、web_search_20250305/max_uses），排除认证 headers。`search` 先解析凭据并检查取消，再同步调用 `recordRequest`；异常向上传播，fetch 不执行，记录后再次检查取消。`index.ts` 将其接到 initiating agent 的 session，事件名为 `web/deepseek-search-llm-request`；没有 initiating session 时上游接线可省略记录。

上游 README 明确该事件是 log-only；`core/session/src/surface.ts` 的模型 surface 类型集合不包含它，`repair.ts` 不重发检索，只关闭未决工具和 turn/step。事件在 resume 后作为原日志中的审计证据保留，没有独立的模型投影或重试用途。本仓采纳先记录再发送与 log-only 行为，按已有 Invocation 约定在没有 journal 时失败关闭，并把完整 endpoint 改为固定协议类别。

同一参考提交的 `packages/web/tool-web/src/search.ts` 在 schema 中只要求 string，在 `parseSearchArgs` 中只检查查询数量、非空白与精确去重；三个检索 provider 原样使用 query，没有单条查询字节上限。本仓复用既有工具参数预算，不为检索另设更小的上限。

非目标：抓取审计、持久化响应结构、费用计量、恢复后自动检索、保存完整 wire body 或新增存储/插件。

## 决策

1. **记录与所有权。** 新增领域类型 `session.WebSearchRequest`、记录 `web/search-request` 和 `Record.search`。工具将 Invocation 的 Journal/Turn/Step/CallID 显式传给 `app/web`。每个 provider 在构造实际请求和认证成功后、发送前调用 `llm.SearchRequest.RecordRequest`，以自己的冻结快照填写 provider/model/effort 与预算；`app/web` 填写调用归属和查询序号并追加到调用方 journal。root 与 child 使用自己的日志，不共享审计。
2. **字段与边界。** 必填字段为 `call_id`、`index`、`provider`、`model`、`endpoint`、`query`、`timeout_ms`、`max_results`；可选字段为 `effort`、`max_uses`、`max_tokens`。endpoint 只接受 `openai-responses`、`codex-responses`、`anthropic-messages`、`openrouter-chat-completions`，分别绑定 openai/openai/anthropic/openrouter。不保存 URL 或账户信息。原始查询不去首尾空白，保证与实际请求相同；须按 ECMAScript `trim()` 集合判定非空白（含 U+FEFF、不含 U+0085）、有效 UTF-8、无 NUL。durable decoder 以 `session.MaxArgumentsBytes`（当前 768 KiB）为单条 query 的防御性字节上界；工具入口对整个调用的原始与 JSON 转义后参数都执行同一预算，包含所有查询及 JSON envelope，单条可发送查询因此还受总量约束，见 [ADR-0002](0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败)。不另设独立查询限值。timeout 记录整个操作的总预算（包含 prepare/追加/所有查询），为 1–60000 ms，不代表发送时的剩余时间；max_results 为 1–8，所有 provider 的结果均受它约束，只有 OpenRouter 将其发送为 wire hint。Anthropic 必须记录 wire 的 max_uses=5 与 max_tokens=4096；其他协议省略两者，因为没有发送对应上限。effort 沿用领域枚举。
3. **顺序与并发。** 调用参数先由 `app/web.ParseQueries` 限制到 1–4 个非空白输入查询，保留原文并按首次出现折叠精确重复项；每个 distinct query 追加一条。发送与 JSONL order validator 复用同一解析函数。读取与追加时解析 pending call 的 queries；记录的 index 从 1 连续增长，不得超过 accepted 的长度，且 `query == accepted[index-1]`。参数无效、参数外查询、顺序不符、超出去重后数量或重复审计均拒绝；没有审计的无效 tool/call 仍可保存并取得错误结果。order validator 在读取与追加时要求当前活动 turn/step 中已有同名、尚未得到 result 的 `web_search` call，且 `arguments_omitted` 不为 true；省略参数的调用不能产生检索请求审计，在解析 queries 前即按无可执行 pending call 拒绝。错误关联、重复或跳号、call 前或 result/step/turn 后追加均拒绝。只串行化审计提交，HTTP 请求可以并发，多个 call 的审计可以交错；结果仍按原查询顺序合并。
4. **失败与取消。** journal 缺失返回 `web_search requires an owning agent session`，不解析账户或发送检索。native provider 缺少 recorder 也失败关闭。追加或 fsync 失败时，对应请求不发送，工具返回 `Error: WEB_REQUEST_RECORD_FAILED: could not persist web search request; request was not sent`，底层原因用 `%w` 保留，但不进入模型文本。首个失败取消其余查询并等待全部结束；之前已提交并发送的兄弟请求可能已经计费，不能撤回。凭据失败或提交前取消不创建审计；提交后取消或 HTTP/protocol 失败保留审计。意图记录不是远端接受、执行或完成证明。
5. **恢复与模型 surface。** 记录不进入模型 surface、prompt、compaction summary 或专用 UI 投影；模型仍从自己的 tool call 与结果获取检索内容。resume 保留已提交审计及原字节前缀，未决 call 仅补 interrupted error result，然后关闭 step/turn。不补造缺失查询审计，不重新发送已有意图；崩溃发生在提交后、发送前或响应后、result 前均采用同一保守语义。用户需要新检索时发起新的工具调用。
6. **版本与保留。** session format 保持 v2，加法记录不使不含它的 v2 日志产生 schema 歧义；旧 binary 遇到未知记录严格拒绝。composition token 从 `web-tools-v1` 升为 `web-tools-v2`，旧组合会话按 composition mismatch 拒绝恢复，不迁移或静默忽略审计要求。当前无已发布用户会话；首次发布的升级承诺仍须另行确认。记录在同一个 owner-only、只追加、写后 fsync 的 JSONL 中，受 6 MiB 单记录与 64 MiB 会话限制，compaction 不删除 raw audit；保留期、备份和恢复路径与所在 transcript 相同。查询与调用参数不一致也属于非法数据；本次因果校验修补不改变字段、format 或 composition token。非法或 torn 数据整体拒绝，原文件保留供离线检查，不截断或改写。

## 后果

日志可以证明检索请求意图绑定到哪个工具调用、使用了哪个冻结 route 和预算。记录失败关闭阻止未审计检索；原有插件与 Scope 持续拥有操作取消、query goroutine 和等待，不增加运行时 effect 或新的生命周期。

每个 distinct query 多一次验证与 fsync，增加延迟、磁盘占用和额外查询副本。日志本身仍可能敏感：查询来自已提交的工具参数，并不做内容脱敏；私有文件权限不是静态加密。完整 URL 与 wire body 未保存，因此不能仅凭审计还原部署目的地或逐字节请求；固定协议模板由 provider 实现与协议测试拥有。旧 composition 的会话不能由新组合继续，原文件仍保留。

## 被否决方案

- **从 tool/call 推导审计**：缺少独立检索 route、认证选择的 endpoint 类别与实际预算，也不能证明发送前提交。
- **发送后记录或忽略记录失败**：外部请求可能已经执行或计费却没有持久化证据。
- **保存完整 URL、headers 或会话 prompt**：扩大敏感信息留存，审计所需信息可由固定类别、查询和预算表达。
- **把审计注入模型 surface**：上游为 log-only，会重复查询并改变模型输入。
- **resume 自动重发或补造审计**：意图没有远端幂等凭证，无法判断旧请求是否执行，可能重复计费或伪造历史。
- **升级 format v3**：加法类型没有 schema 歧义，composition token 已表达运行时要求；无需拒绝所有 v2 解析。
- **先全批记录再统一发送**：需要额外 provider 预备 API 与请求状态，当前每个 query 发送边界的审计已满足要求；部分已发请求的失败语义显式记录。
- **独立的 128 KiB 查询上限**：上游没有该限制；现有整次工具参数与会话记录预算已约束资源，额外限制会拒绝这些预算内原本合法的查询。

## 复审触发条件

新增 provider 或可配置检索预算、协议 template/工具版本变化、需要完整目的地审计或远端幂等恢复、首次发布旧会话迁移承诺，或实测 fsync 延迟影响检索预算时，重新评估字段和恢复契约。

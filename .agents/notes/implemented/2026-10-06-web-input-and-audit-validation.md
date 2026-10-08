# Web 输入预算与审计查询绑定

- Status: implemented
- Date: 2026-10-06

## Context

基线 `d7d199d` 的首层 decoder 直接读取网络 body，预算只覆盖后续 decoder 输入。有效空 gzip member 可以消耗 10 MB 输入却成功返回 `ok`；同样大小的真实 chunked 单层响应也通过。JSONL 仅检查 pending call、连续 index 与查询不重复，未把 query 绑定到调用参数；冻结样本改为未请求的 `python`、交换查询顺序或减少 distinct query 数量均被接受。领域审计另有一份 ECMAScript 空白实现，LLM 为一个 helper 依赖 app/tool。

只读参考 submodule `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 的 `packages/web/web-fetch-http/src/provider.ts` 只限制 Content-Length 与 Undici 解压后的正文，`network.ts` 使用 Undici fetch，没有独立压缩网络输入预算。公网服务端可在时限内持续消耗带宽和解码 CPU，最终小正文不能证明输入消耗有界。

本 Note 与[解压边界](2026-10-06-web-fetch-decompression-boundaries.md)、[请求审计](2026-10-06-web-search-request-audit.md)、[交互空白规则](2026-10-06-interaction-state-upstream-alignment.md)部分重叠：这里拥有编码网络输入、query/index 绑定与 helper 收敛证据，旧 Note 保留其余证据并互链，不归档或修改冻结记录。

查询预算的文档对齐与省略调用 guard 的补充测试由[审计参数边界 Note](2026-10-06-web-search-audit-argument-constraints.md)拥有；本 Note 保留网络预算、query 绑定和共享空白证据。

## Decision

长期取舍修补既有 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md#抓取传输语义)与 [ADR-0022](../../../docs/decisions/0022-web-search-request-audit.md)，不新建 ADR。

非空 Content-Encoding（含 identity）先给网络 body 加 5,000,000 字节预算，再建立 decoder；中间层预算独立存在。超限用上游消息 `response exceeds the maximum of 5000000 bytes` 返回 `WEB_FETCH_TOO_LARGE`，错误链保留原因；编码声明项超限仍说明编码项数量。恰好预算可成功，只额外探测一字节区分 EOF 和溢出。缺省编码与最终解压正文保留流式截断，取消和期限优先于资源错误。

`app/web.ParseQueries` 是发送与 JSONL order validator 共用的纯解析函数。读取与追加在 pending web_search call 参数上执行同一非空白、1–4 输入、精确去重保序规则，要求 index 连续且不超出 accepted，query 精确等于对应原文。只保存已提交序号，无需重复保存审计查询列表；无效参数没有审计时仍可记录并返回工具错误。解析原因以 `%w` 保留，拒绝不写入文件。

LLM、web 查询解析与领域审计直接复用 `core/text.TrimSpace`；工具入口仍通过其现有 helper 使用同一集合。N2 无行为变化。组件沿原 cmd composition 和 Scope 生命周期运行，fetch 同步拥有 decoder，web 操作拥有取消与等待；没有新增插件、goroutine、缓存、依赖或注册 effect。

## Consequences

小正文无法绕过编码实体预算；带宽或框架超过预算的资源明确失败。预算针对 response.Body，不是包含 HTTP/TLS framing 与 transport 预读的 socket 精确流量计量；同步解码检查点仍不提供硬实时中断。

审计可证明 query 属于该调用并位于正确序号，仍不能证明远端执行。错误关联的旧日志属于非法事实，整体拒绝且保留原文件；合法 v2 字段和 composition token 不变，不迁移或静默接受错误审计。query 参数在日志验证时重新解析，成本受现有调用参数与查询数量预算约束。

## Verification

- 修复前：`go test -count=1 ./internal/adapter/web/fetch ./internal/adapter/session/jsonl -run 'TestFetch_(BoundsEncodedNetworkInput|RejectsSingleLayerCompressedNetworkBomb|IdentityNetworkOverflowRequiresEncodingHeader)$|TestSessionV2WebSearch_RejectsChangedContract$|TestLog_WebSearchAuditMatchesDistinctCallQueries$'` 退出码 1。5,000,001 与 10,000,000 字节的 gzip/x-gzip/identity 混合声明、真实 chunked gzip 与 identity 超限都返回 nil；七个冻结参数/查询变体和三个真实 Append 子测试接受非法关联。永久测试不使用 sleep。
- `go test -race -count=1 ./internal/adapter/web/fetch ./internal/adapter/session/jsonl ./internal/core/text ./internal/core/session ./internal/app/llm ./internal/app/web ./internal/adapter/tool/web ./cmd/nano-harness`：通过。计数读取与真实 chunked HTTP 验证网络预算，真实日志 Append/Inspect 验证拒绝不改字节、精确去重、原始空白和 NEL；已有 BOM/NEL、provider、恢复、模型 surface 与 composition 证据验证 N2 行为不变。
- `go test -count=1 -overlay .cache/r3-tools-baseline/overlay.json ./cmd/nano-harness -run '^TestComposition_WebFetchRejectsEncodedNetworkBomb$'`：overlay 仅加载 `d7d199d` 的 fetch/compression 产品源码，永久 assembled 测试以 `IsError:false` 与正文 `ok` 失败；当前源码的同名 `go test -race -count=1` 通过。真实 composition 的磁盘 tool/result 和下一模型请求都含精确的 too-large 错误。
- `git rebase --autostash feat/upstream-tool-parity`：最终基线为 `802fc427713990d2f79febe10f1ea840442030d3`；保留该基线全部 116 个 mutation，增加本 Note 的两项与审计参数边界 Note 的一项，共 119 项；架构文档的 shell、结构化工具结果语义与本次共享空白规则同时保留。
- `scripts/mutation-cases.json` 增加移除编码网络预算与移除 query/index 精确比较的两个反例，均由 owning 永久测试约束。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=feat/upstream-tool-parity make check`：通过。fmt/mod/vet、全仓 race、架构、submodule、Agent Note、skills 与 workflow-tools 通过；lint 为 0 issues；全仓每个产品源文件/函数 100.0%，原始 profile 无零执行 block；默认 119 项 mutation 全部 killed，真实 cmd build/version smoke 通过。mutation 报告的清单、每个 site 与完整产品/测试树哈希均与最终工作树一致。
- `python3 scripts/mutation-check.py --manifest internal/adapter/web/fetch/testdata/transport-mutations.json --report .cache/mutation/r3-tools-fetch-transport-final.json`：14/14 killed，原有多层预算、取消、期限、UTF-16、URL 和拨号回退断言仍能拒绝对应回归；报告 site 哈希与当前源文件一致。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e`：通过，真实 binary/PTY 的 19 次 root 工具调用、图片、计划、后台通知、提问、目标、spawn/fork、审批、文件、打断、resume 与 cleanup 通过；web 的模型可见错误由上述 assembled 测试验证。
- `make agent-notes`、`git diff --check` 与改动 Markdown 的相对文件链接检查：通过。完整 diff 仅含本次工具边界、测试与文档修补，没有依赖、submodule、归档 Note、凭据或无关生成物；没有修改其他 worktree 或推送。
- 未执行上游 TypeScript 测试、live provider、真实公网或其他 OS 原生矩阵；本机确定性 HTTP/JSONL 与 assembled 证据不等同这些验证。

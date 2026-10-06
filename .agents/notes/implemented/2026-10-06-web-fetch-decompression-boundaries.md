# web_fetch 逐层解压预算与取消

- Status: implemented
- Date: 2026-10-06

## Context

基线 `726a95f470246be0b5bde1e15290eaa261cb566c` 的抓取只限制最终解压正文，串接空 gzip member 后再压缩的响应可把中间流展开到超过 10,000,000 字节，却成功返回 `ok`。标准库 decoder 消费已缓存的字节时不再触达带 context 的 HTTP 连接，因此首次读取后的取消、已经发生的抓取期限均可被忽略。永久测试在产品修复前分别得到 `error=<nil>`，而预期为 `WEB_FETCH_TOO_LARGE`、`WEB_ABORTED` 和 `WEB_FETCH_TIMEOUT`；6 项编码声明还会读取禁止消费的正文并返回错误类别不符的 provider failure。

本变更仅涉及 fetch、其测试与相关文档。[传输对齐 Note](2026-10-06-web-fetch-transport-alignment.md)保留 URL/IDNA、UTF-16、拨号回退和初次编码支持的证据，本 Note 补充解压资源与取消边界，二者部分重叠，不归档旧 Note。HTML 转换、展示预算和 app/web 服务不在修改范围内。

## Decision

长期契约由 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md#抓取传输语义)与[网络边界](../../../docs/security.md#网络边界)拥有。参考 submodule 仍为 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`，锁定 [Undici 8.10.0](https://github.com/nodejs/undici/blob/v8.10.0/lib/web/fetch/index.js#L2069-L2129)：逆序建立流管道、最多 5 个编码项，没有逐层累计字节预算。本仓对齐编码项上限，并对每个中间解压输出设置独立预算，超限返回有原因链的 `WEB_FETCH_TOO_LARGE`；最终正文保留现有截断语义，identity 不额外制造解压层。

网络源和各层输出使用同一操作 context，在有界读取前后检查取消；即使网络字节已读尽，缓存 decoder 也不能在取消后继续返回正文。抓取期限与调用方/shutdown 取消沿既有错误分类优先于资源或 codec 错误。解码在调用 goroutine 同步执行，抓取操作拥有并关闭全部 decoder；没有新增 goroutine、插件、生命周期 effect 或依赖，app/web 的 Scope 继续取消并等待抓取操作。

## Consequences

小传输体或小最终正文不能绕过中间展开预算，连续空 member 也会触发预算与取消检查。需要超过编码项上限或中间预算的资源明确失败；最终超限仍返回有界且标记截断的正文。读取检查点之间的标准库运算和 OS 调度可能使返回略晚于期限，不提供硬实时保证；注入的网络边界仍须遵守 context，不能强制中断一个忽略取消的外部 Reader。

没有依赖或 submodule 指针变化。原有逐跳公网解析校验、固定 IP 拨号、NAT64、同源重定向、拒绝凭据和代理边界继续由原路径强制。

## Verification

- 修复前：`go test -count=1 ./internal/adapter/web/fetch -run 'TestFetch_(RejectsIntermediateDecompressionBomb|CancellationAfterFirstBodyReadStopsDecoding|ExpiredDeadlineStopsBufferedMultiLayerDecoding|RejectsExcessiveContentEncodingLayersBeforeReading)$'` 失败。真实 HTTP 多层炸弹成功返回；空/identity/gzip/zlib/raw deflate/叠加共 6 个首次读取取消分支全部成功；20 ms 期限屏障释放缓存数据后仍成功；6 项声明消费了正文。测试以读取动作与 context.Done 屏障固定交错，不使用 sleep。
- `go test -race -count=1 -coverprofile=.cache/fetch-bomb-cover.out ./internal/adapter/web/fetch`：通过，四个产品文件所有函数 100.0%，原始 profile 无未执行语句；另覆盖中间流恰好预算和多一字节、5 层成功、identity 两侧的最终截断、网络读尽后的缓存解码取消。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过，完整 race、架构、submodule、Agent Note 格式、skills、workflow tools、lint（0 issues）、全仓逐文件/函数 100% coverage、54 项默认 mutation 全部 killed、真实 cmd 构建与 version smoke 均通过。首次检查发现新增 HTTP fixture 缺少 Body.Close，补齐显式 cleanup 与返回值处理后重跑通过，没有关闭 lint 规则。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e`：通过，真实 binary/PTY 的 19 次根工具调用、图片、审批、提问、目标、子代理、文件、恢复和 cleanup 均通过。
- `python3 scripts/mutation-check.py --manifest internal/adapter/web/fetch/testdata/transport-mutations.json --report .cache/mutation/fetch-bomb.json`：14 项全部 killed，含 6 项新增的中间预算、编码项上限、读取前取消、读取后取消、缓存 decoder 取消和期限保留；8 项初次传输对齐变异仍通过。执行器在私有副本中先跑正常用例，再无缓存变异编译与具名测试；报告 source SHA-256 与最终产品源码一致。
- `git diff --check` 与受影响文档的相对链接/fragment 检查：通过；完整 diff 审查没有新增分层、生命周期或 SSRF 绕过问题。没有改动 tool/web、app/web、submodule、其他 worktree 或依赖，也没有新增凭据与无关生成物。
- loopback 和读取屏障提供确定性证据，没有真实公网故障网络、live provider 或其他 OS 原生执行证据。

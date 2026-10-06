# web_fetch 传输规范化、解压与双栈回退

- Status: implemented
- Date: 2026-10-06

## Context

参考 submodule `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 的 `packages/web/web-fetch-http` 使用 WHATWG URL、Undici 解压与 autoSelectFamily、UTF-16 length/slice。基线 `d01c5f42bb1d06d0a579737f19886fbb5fc1903d` 的 fetch 仅依赖 Transport 自动 gzip，未把 IDNA 主机转为 ASCII，按 rune 截正文、按字节限制 URL，并逐地址串行拨号。永久测试在产品修复前稳定复现四项差距：deflate/raw deflate/叠加编码返回压缩乱码且 `err=nil`；br/zstd 等返回成功；Unicode 主机序列化为 percent escape、空白/opaque HTTP URL 被拒；50,001 个 emoji 不截断；阻塞的 IPv6 拨号让可达 IPv4 等到总期限耗尽。

范围仅为 `internal/adapter/web/fetch`、相关文档与本 Note。`internal/adapter/tool/web` 的 HTML 转换和格式化预算、`internal/app/web` 的查询语义属于其他工作。旧[web 能力 Note](2026-10-04-web-search-and-fetch.md)部分重叠，保留它对服务、工具、provider 和 composition 的证据，并双向链接；没有归档或改写冻结 Note。

## Decision

长期契约与偏离理由见 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md#抓取传输语义)，网络边界见[安全规则](../../../docs/security.md#网络边界)。实现复用标准库 gzip/zlib/flate 和既有 BSD-3-Clause 的 `x/net/idna`，不新增或升级依赖，不引入 Brotli/zstd 解码器。显式声明 `gzip, deflate`，未支持编码失败；解压链之后继续用 bounded reader 限制字节，坏头、流和 checksum 保留错误原因。

URL 规范化在解析与拨号前完成；解析器、Host、TLS、同源校验和最终 URL 共享规范化对象。URL 与正文按 UTF-16 计数，正文不拆 Unicode scalar，代理对放不下时整体省略且标记截断。异常 URL 拼写继续严格拒绝，具体集合由 ADR 拥有。

拨号在已校验地址集合内按地址族交替回退：首个成功后取消并 join 其余尝试，关闭未采用和取消期间得到的连接，再交给每跳独立 transport。`DialFunc` 明确支持并发与 context 取消。不能只把 race dialer 塞进 Transport：Transport 在取消请求时可能先返回、拨号回调仍未结束；同步抓取 owner 保证 `Fetch` 返回前已回收拨号。`app/web` 的 Scope cleanup 仍通过取消、等待抓取操作拥有这些短期 effect，没有新增常驻插件、连接池或全局状态。

## Consequences

正常 URL、IDNA、压缩文本、emoji 预算和双栈网络回退与参考的语义接近，并保留逐跳公网校验、固定 IP、NAT64、同源重定向、拒绝凭据与代理的边界。br/zstd-only 服务不可用，损坏压缩流按标准库严格校验而失败；异常 URL、少见标点及空 fragment 的序列化不承诺完整 WHATWG 一致。正文代理对边界可少一个 UTF-16 单元，结果始终是有效 UTF-8。

已有 fetch 客户端没有跨调用资源；其 operation 内的拨号 goroutine 现在有显式取消与 join。网络注入方必须遵守取消契约，不能强制回收一个忽略 context 的外部 dialer。没有真实双栈故障公网、live provider 或其他 OS 原生运行证据；loopback wire 与 barrier 测试证明确定性行为，不替代部署网络证据。

## Verification

- 修复前：`go test -count=1 ./internal/adapter/web/fetch -run 'TestFetch_(DecodesSupportedContentEncodings|RejectsUnadvertisedContentEncodings|NormalizesURLBeforeResolutionAndRedirects|TruncatesUTF16WithoutSplittingEmoji|FallsBackWhileFirstValidatedAddressIsBlocked)|TestParseURL_(NormalizesNormalURLs|CountsUTF16Units)'` 失败。deflate 有压缩头乱码且 `err=nil`；br/zstd 为 `error=<nil>`；Unicode URL host 非 punycode；emoji 200,004 字节且 `Truncated=false`；barrier 保持首个拨号阻塞时返回 `WEB_FETCH_TIMEOUT`（测试 watchdog 为 2 s，顺序由 channel 决定）。
- `go test -race -count=1 -coverprofile=/tmp/codex-fetch-coverage.out ./internal/adapter/web/fetch`：通过，四个产品源文件全部函数 100.0%，原始 profile 无未覆盖语句。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint golangci-lint run ./internal/adapter/web/fetch`：`0 issues`。
- loopback HTTP/TLS fixtures 验证已知正文与最终 URL、原有 SSRF/NAT64/重绑定/同源拒绝矩阵；可控 tick/barrier 验证另一个地址族回退、取消 join、晚到连接关闭；压缩炸弹损坏尾部位于限制之后仍返回有界且标记截断的正文。
- `go test -race -count=1 ./internal/adapter/web/fetch ./internal/adapter/tool/web ./internal/app/web`：三个包通过；TLS fixture 证明规范化主机继续用于证书校验，代理环境 fixture 证明 HTTP(S)/ALL_PROXY 均不影响固定 IP 连接。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过；完整 race、架构、submodule、Agent Note 格式、skills、workflow tools、lint、全仓每文件/函数 100% coverage、33 个仓库定向 mutation 全部 killed、真实 cmd build/version smoke 均通过。
- `make tui-e2e`：通过；真实 binary/PTY 的十九次根工具调用、图片、提问、审批、目标、子代理、文件与恢复/cleanup 均通过。既有 web assembled 测试由 `make check` 的真实 composition 测试执行。
- `make vuln`：`No vulnerabilities found.`；没有依赖版本、工具链或 submodule 指针变更。
- `python3 scripts/mutation-check.py --manifest internal/adapter/web/fetch/testdata/transport-mutations.json --report .cache/mutation/fetch-alignment.json`：八个补充变异全部 killed，分别取消解压字节边界、绕过解压、恢复 rune 计数、改变 IDNA 映射、移除族间交替、遗漏迟到连接关闭、发布取消期间的 winner、阻止定时回退。manifest 随 fetch 测试提交，执行器只在私有副本中变异；没有修改仓库共享 mutation 清单。
- `git diff --check` 与受影响文档文件链接检查：通过。改动仅涉及 fetch 包、相关文档和 active Notes，未修改 `internal/adapter/tool/web`、`internal/app/web`、submodule 或其他 worktree。

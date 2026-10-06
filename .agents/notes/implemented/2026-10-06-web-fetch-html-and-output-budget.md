# web_fetch 的 HTML 语义与格式化预算对齐参考

- Status: implemented
- Date: 2026-10-06

## Context

基线 `d01c5f42bb1d06d0a579737f19886fbb5fc1903d` 的 HTML 转换缺少删除线、任务状态、代码语言和 Markdown 字面量转义；元素栈只按显式结束标签退栈，`<p hidden>secret<p>visible` 得到空字符串。工具又在通用 spill 之前按 256 KiB 限额，100,000 个汉字的 300,119 字节格式化正文只能保存 262,142 字节。新增永久测试在产品修复前分别稳定失败。

参考为 submodule `5badb15009ae` 的 `packages/web/tool-web/src/fetch.ts`，锁定 Turndown 7.2.4、GFM 1.0.67。修改仅在专用 worktree 的 `internal/adapter/tool/web`、测试与文档；传输、`internal/app/web` 和通用 spill 实现归现有 owner。[WP5 Note](2026-10-04-web-search-and-fetch.md)保留能力与生命周期证据，[spill Note](2026-10-05-tool-output-spill-and-read-before-write.md)保留存储与预览决策；本 Note 部分补充二者，不归档旧记录。

## Decision

长期展示契约更新在 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md)，组件事实归[架构](../../../docs/architecture.md#web-检索与抓取)，测试责任归[测试策略](../../../docs/testing.md)。

- 转换器保留 del/s/strike、checkbox 的布尔 checked 状态、`language-*`、代码围栏和字面量；代码空白与硬换行不参与普通文本清理。词法解析仍由 x/net tokenizer 提供，转换栈与隐式闭合由本仓实现。隐式闭合发生在继承 hidden 前，列表、表格、template 等作用域限制对祖先元素的关闭。
- 格式化输入和完整输出按 200,000 UTF-16 单元限额，含 header、说明和 footer；汉字计一单元，补充平面字符计两单元，截断不拆 UTF-8。格式化结果进入既有 runtime spill，存储完整的有界结果后生成预览。
- `web-tools` 仍由 `cmd` 的有序 composition 发布两个注册贡献，Scope 撤销它们；没有新增运行时 effect、goroutine 或配置。算法为工具层的纯转换，错误和取消继续由 service/runtime 处理。已有会话文本仍以已提交 `tool/result` 为准，session 格式与工具定义不变。
- WP5 删除过期的“未提供 spill”表述；ADR 澄清 tokenizer 与自有转换栈的职责，并记录复用 provider endpoint、无法独立配置检索 endpoint 的代价。

## Consequences

模型保留删除、勾选、代码语言和字面文本语义，未显式结束的隐藏段落不再吞掉后续可见内容。正常中文正文不因 UTF-8 字节较多而在 spill 前永久丢失；HTML 转换展开后的输出仍受完整格式化预算限制。

保留的等价排版差异见 ADR-0011；转换器仍需对照上游维护，tokenizer 不提供 DOM 修复。没有 spill store、缺少会话或保存失败时仍按 runtime 的 256 KiB 兜底，不能保证完整保存；超过抓取提供方或 200,000 单元预算的原文不会保存在格式化 spill 中。UTF-16 预算只剩一个单元时省略整个补充平面字符，最多少用一单元。检索 endpoint 的配置限制保留。没有新增依赖、兼容层或持久化承诺。

## Verification

- 修复前：`go test -count=1 ./internal/adapter/tool/web -run 'TestRenderHTML_MatchesUpstreamSemantics|TestFormatFetch_MatchesUpstreamUTF16Budget'` 失败，删除线变普通文本、任务状态/语言丢失、字面量未转义，隐藏段落输出为空，汉字与 emoji 提前截断。
- 修复前：`go test -count=1 ./internal/adapter/tool/web -run TestProvider_FetchSpillsCompleteFormattedOutput` 失败，CJK 文件为 262,142 字节，期望 300,119；Markdown 展开与完整预算边界也失败。补充的图片标签、URL 括号/空格/尖括号、硬换行、代码空白（含 blockquote）、字面 tilde 围栏/单个 `=` 和 paragraph 作用域 fixture 在修复各自行为前失败。
- 锁定依赖的本机 Node probe 只读加载参考 `fetch.ts` 并调用其 formatter，42 个表驱动 fixture 的 upstream 预期逐项一致；不把上游实现复制进本仓，普通 Go 测试不依赖 Node、网络或 submodule。
- `go test -race -count=1 -coverprofile=.cache/html-coverage.out ./internal/adapter/tool/web` 通过；原始 profile 无未覆盖语句，两个产品文件全部 100.0%。测试独立比较 ASCII、汉字、emoji、source 截断与 footer，以及真实 spill 文件的完整内容和模型预览定位符。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 在最终 Go diff 上通过：全量 race（含真实 `cmd` composition 的 web 场景）、architecture、submodule、Agent Note、skills、workflow-tools、lint、逐文件 coverage、33 个 mutation、真实入口构建 smoke 全部通过；lint 为 `0 issues`，原始产品 coverage profile 的未覆盖语句 block 为 0。中断的门禁没有作为成功证据；最后的完整运行结果用于交付。
- `python3 scripts/mutation-check.py --manifest .cache/html-mutation-cases.json --report .cache/html-mutation-report.json` 使用私有源码副本，无缓存验证四个本次回归：改回字节计数、把工具上限设为 256 KiB 数值、禁用隐式闭合、禁用字面量转义，全部 killed。对应测试分别为 UTF-16 预算表、真实 spill 文件表和上游语义表；没有修改仓库的通用 mutation 集合。第一版字节变异因移除唯一 import 用途而 build-error，未计为成功，保留 import 的可编译变异重新运行后 killed。
- `make tui-e2e` 通过：真实编译二进制/PTY、19 次根工具调用、图片结果、todo、后台通知、提问、规划、目标、spawn/fork、审批、文件、粘贴、resize、interrupt、resume 和 cleanup。
- `make agent-notes`、`git diff --check` 通过；改动文档的 43 个本地链接目标存在。范围自审确认只改 web 工具、同目录测试与所属文档，传输层、app/web、只读 submodule 没有 diff。没有新增插件 effect、依赖或跨层调用。

未运行 live provider、真实公网抓取或跨平台原生执行；本机为 Go 1.27.0、darwin/arm64。

# web_fetch 按 HTML 规则处理自闭合标记

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查的 S2（astra 报告，Fable 交叉核实确认并补充了更多泄漏形式）指出，`internal/adapter/tool/web/html.go` 遇到 `SelfClosingTagToken` 时对所有非 void 元素都立即 `close`。HTML 标准规定 HTML 命名空间中非 void 元素的 `/>` 被忽略，所以 `<div hidden/>secret</div><p>visible</p>` 的 hidden 状态被提前解除，输出 `secret\n\nvisible`，上游只输出 `visible`。同样的提前关闭也让 `aria-hidden`、`display:none`、`visibility:hidden` 以及 `<script/>`、`<style/>`、`<iframe/>` 的内容出现在输出里。

x/net tokenizer 在 `<script/>` 这类自闭合 raw-text 标签之后仍按名称进入 raw text，这对 HTML 命名空间是正确的；但在 SVG/MathML 中，它把 `<svg><title/>` 之后的标记也当作 raw text 吞掉。转换器此前没有记录命名空间。

上游参照为 submodule `5badb15009ae` 的 `packages/web/tool-web/src/fetch.ts`，锁定 Turndown 7.2.4、`@joplin/turndown-plugin-gfm` 1.0.67 和 `@mixmark-io/domino` 2.2.0（与上游 `pnpm-lock.yaml` 一致）。差分输出由审查者在仓库外安装的同版本依赖和逐字复制的 `fetch.ts` 转换配置得出。本 Note 补充 [HTML 语义与格式化预算 Note](2026-10-06-web-fetch-html-and-output-budget.md) 和 [WP5 Note](2026-10-04-web-search-and-fetch.md)，不归档它们。

## Decision

长期契约写在 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md) 第 7 条，组件事实归[架构](../../../docs/architecture.md#web-检索与抓取)，边界归[安全规则](../../../docs/security.md#网络边界)。

- 元素栈的每一帧记录命名空间和是否为 HTML integration point。`svg`/`math` 开始标签进入 foreign 内容；SVG 的 foreignObject/desc/title、MathML 的 mi/mo/mn/ms/mtext 和 HTML 编码的 annotation-xml 是 integration point；在 foreign 内容中遇到 HTML 标准列出的 breakout 标签（font 仅在带 color/face/size 时）时，先弹出到最近的 HTML 元素或 integration point，再按 HTML 元素处理。
- HTML 命名空间：开始标签和自闭合标签同样处理，`/>` 被忽略，元素保持打开；raw-text 元素沿用 tokenizer 的 raw text，在自身结束标记处结束。void 元素不变。
- foreign 命名空间：调用 `NextIsNotRawText`，raw-text 名称是普通元素；`/>` 打开后立即关闭元素。foreign 元素只承载可见性，不套用 HTML 的块、链接和列表格式。
- 与上游的已知差异只保留两项，都写入 ADR-0011 并有测试记录上游输出：domino 2.2.0 不把自闭合 raw-text 标签记为最后的开始标签，`<script/>`、`<style/>`、`<iframe/>`、`<textarea/>`、`<title/>` 之后的 raw text 会在错误的结束标记处结束（吞掉页面剩余内容、输出 `</textarea></x-turndown>` 这样的字面结束标签，或在祖先的 `</div>` 处结束而泄漏脚本文本），本仓遵循 HTML 标准；上游的移除规则只匹配大写 HTML 节点名，会输出 SVG/MathML 中 script 与 style 的文本，本仓在所有命名空间中移除它们。
- 运行时 composition、插件 effect、session 格式和工具定义都不变。

## Consequences

自闭合的隐藏元素、raw-text 元素和 foreign 内容不再泄漏应隐藏的文本，可见内容也不会被吞掉。转换器仍是在 tokenizer 之上的部分树构建：命名空间、integration point 和 breakout 按标准实现，但不包含完整的 HTML 树构建算法（例如表格 foster parenting），这些仍由上游差分样本约束。上游 domino 的自闭合 raw-text 缺陷没有被复制；如果参考指针更新后上游修复了它，两侧会重新一致。

## Verification

- 修复前：把 `74f420b` 的 `html.go` 经 `go test -overlay` 替换进当前测试，`go test -count=1 -overlay <map> -run 'TestRenderHTML_(MatchesUpstreamSemantics|SelfClosingRawTextFollowsHTMLStandard)' ./internal/adapter/tool/web/` 失败：`TestRenderHTML_MatchesUpstreamSemantics` 有 31 个新增用例失败，包括自闭合的 hidden、aria-hidden、`display:none`（含大写写法和 span）、`visibility:hidden`、`<p hidden/>`、隐藏子元素和列表项、`<b/>`/`<a/>`、noscript/template/object、结尾的 `<script/>`、foreign 中的 title/style、foreignObject、breakout、font breakout、MathML 的 text integration 与 mtext，以及 HTML 编码的 annotation-xml；`TestRenderHTML_SelfClosingRawTextFollowsHTMLStandard` 的 script、style、iframe 和祖先内 script 四个用例失败。失败输出都含被隐藏的文本。
- 修复后，上游差分表共 42 个新增样本，每个样本的期望值都取上游对该样本的实际输出；只有 ADR-0011 已列出的等价列表标记间距不同。Fable 列出的 `<span style="display:none"/>`、`<p hidden/>`、`<template/>` 形式在差分表中，`<script/>alert(1)</script>`、`<style/>body{…}</style>` 在 raw-text 差异表中，上游对这两项输出空串。另有 6 个自闭合 raw-text 样本和 3 个 foreign script/style 样本记录上游输出并断言 HTML 标准行为。深度测试覆盖 foreign 自闭合在 512 层边界内外的行为。
- `go test -race -count=1 ./internal/adapter/tool/web/`：通过，逐语句 coverage 100.0%。
- 新增 mutation `web-html-self-closing`（HTML 自闭合元素重新走立即关闭路径）、`web-html-foreign-self-closing`（foreign 自闭合不关闭）和 `web-html-foreign-breakout`（禁用 breakout），用 `python3 scripts/mutation-check.py --manifest <这三项>` 运行，均被 `TestRenderHTML_MatchesUpstreamSemantics` 杀死。
- `make check`（私有 `GOLANGCI_LINT_CACHE`）：通过；逐产品文件 coverage 100.0%，lint `0 issues`，清单中 192 个 mutation 全部 killed，build smoke 输出 `nano-harness dev`。

未获得的证据：没有对真实网页做批量差分；差分只覆盖表中列出的样本。

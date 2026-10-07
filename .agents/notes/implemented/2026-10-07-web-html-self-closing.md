# web_fetch 由 HTML 解析器构建转换树

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查的 S2（astra 报告，Fable 交叉核实并补充了更多泄漏形式）指出，`internal/adapter/tool/web/html.go` 遇到 `SelfClosingTagToken` 时对所有非 void 元素都立即关闭。HTML 标准规定 HTML 命名空间中非 void 元素的 `/>` 被忽略，所以 `<div hidden/>secret</div><p>visible</p>` 输出 `secret\n\nvisible`，上游只输出 `visible`。同样的提前关闭也让 `aria-hidden`、`display:none`、`visibility:hidden` 以及 `<script/>`、`<style/>`、`<iframe/>` 的内容出现在输出里。第一次修复（`2600c6e`）在 tokenizer 之上加入命名空间、integration point 与 breakout 的模拟。

复审随后又发现同一模拟的偏差。astra 指出 font breakout 按属性值而非属性是否存在判断，annotation-xml 下的 svg 没有切换命名空间，以及 mtext 下 mglyph/malignmark 例外缺失造成可见文本丢失。Fable 指出误嵌套格式元素被重建或经 adoption agency 拆分后不再携带隐藏属性（`<i><b hidden>x</i>SECRET</b>` 输出 `SECRET`），以及 foreign CDATA 被吞掉。opus 用 1,105 个合成样本对上游做差分，结果相符。根因是转换器在 tokenizer 之上自写部分树构建：每轮修补只覆盖一部分规则，表格、误嵌套格式与 adoption agency 等其余规则仍会改变隐藏范围。

上游参照为 submodule `5badb15009ae` 的 `packages/web/tool-web/src/fetch.ts`，锁定 Turndown 7.2.4、`@joplin/turndown-plugin-gfm` 1.0.67 和 `@mixmark-io/domino` 2.2.0（与上游 `pnpm-lock.yaml` 一致）。差分输出来自仓库外安装的同版本依赖，转换配置直接从 submodule 的 `fetch.ts` 截取。本 Note 接管 [HTML 语义与格式化预算 Note](2026-10-06-web-fetch-html-and-output-budget.md) 中隐式闭合与作用域的部分；该 Note 仍负责格式化规则与输出预算。[WP5 Note](2026-10-04-web-search-and-fetch.md) 保留能力与生命周期证据，两者都不归档。

astra 复审合入的 `4c8d690` 时报告 B1：512 层开放元素上限不约束累计建树量。属性各不相同的格式元素都留在活动格式元素列表中，后续每个块都会重建它们；8,395 字节输入生成 502,502 个节点，12,395 字节输入累计分配约 161 MB，而且建树发生在隐藏过滤和输出截断之前，同步解析也无法取消。上游的词法深度 guard 拒绝 astra 的样本，但同类样本只要不超过词法深度就能通过，并在 domino 中同样生成约 50 万个节点。

## Decision

长期契约写在 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md) 第 7 条与第 10 条，组件事实归[架构](../../../docs/architecture.md#web-检索与抓取)，边界归[安全规则](../../../docs/security.md#网络边界)，差异登记在[参考分析](../../../docs/reference-deepseek-harness.md)。

- `renderHTML` 用 `html.ParseFragmentWithOptions` 在 body 上下文中解析，并关闭脚本标志（与参考 domino 一致，noscript 内是普通标记）。隐式结束标签、自闭合标记、raw text、foreign content 与 integration point、CDATA、格式元素重建和 adoption agency 都由解析器完成。转换器只遍历树：HTML void 元素走原有 void 处理，其余元素按命名空间入栈。foreign 元素只承载可见性，不套用 HTML 的块、链接和列表格式。删除规则与格式化规则不变。
- 删除自写的 dispatcher、integration point、breakout、作用域关闭和深度计数。解析器在开放元素超过 512 个（片段根计入）时返回错误，转换器输出固定省略标记 `[HTML content omitted: unable to convert safely.]`。内存 reader 不会产生其他解析错误。
- x/net/html 在 SVG/MathML 元素打开时处理 template 开始标签会忽略其后的全部输入，这是其源码注明的偏差。转换器用 tokenizer 统计词法 template 开始标签；多于树中的 template 元素时，在已解析的内容后追加同一省略标记。tokenizer 把 SVG title/style 当作 raw text，其中的 template 不被计数，这种丢弃不带标记。
- 与上游的已知差异写入 ADR-0011，测试同时记录上游的实际输出：
  - domino 的自闭合 raw-text 缺陷（本仓遵循标准）。
  - SVG/MathML 中的 `</p>`、`</br>` 按当前标准结束 foreign 内容；domino 实现此前的规则，外层隐藏元素保持打开，本仓输出其后的文本，浏览器同样显示。
  - foreign script/style 在所有命名空间中删除。
  - 上述 x/net template 偏差。
  - 深度计数方式不同。
  - 误嵌套产生的空格式包装（上游输出 `~~~~`、`[](/a)`）不输出，归入等价排版。
- 建树前，`exceedsConversionCost` 用 tokenizer 扫描一次：每个开始标签（含自闭合标签）计一个元素，再加上词法栈上仍打开的格式元素数；只有匹配栈顶的结束标签出栈，noscript 内容按标记扫描。累计超过固定上限 `maxConversionCost`（65,536）时输出同一省略标记，不建树。扫描不模拟 Noah's Ark 和 marker，只会高估。转换不接收 context：扫描为线性，接受的输入建树量有固定上界。
- 运行时 composition、插件 effect、session 格式和工具定义都不变。依赖仍是已有的 `golang.org/x/net` v0.59.0，未改变 `go.mod`。

## Consequences

隐藏范围由完整的 HTML 树构建决定，不再取决于自写规则的覆盖程度；`html.go` 相对 `92e3599` 净减少 107 行，三轮复审报告的泄漏类别都由解析器处理。误嵌套格式元素的泄漏选择对齐而不是记为偏差：泄漏会把应隐藏的文本交给模型，在流式渲染中补齐 adoption agency 需要移动已输出的子树，改动面比换用解析器更大，风险也更高。

代价有三项。第一，引入 x/net 的 template 偏差：只会少输出内容，可检测时有标记，SVG title/style 中的情形无标记。第二，foreign `</p>`、`</br>` 之后的文本与上游不同。第三，深度上限改按解析器的开放元素栈计算，与上游的词法计数在边界处不同。x/net 修复 template 偏差、参考指针更新 domino，或上游改变移除规则时，需要重新评估这些差异和对应测试。建树成本上限比上游更严：格式元素长期不闭合、又跟着大量块的页面会被省略；已测真实页面得分不超过约 7,400，上限留有约 9 倍余量。上游加入累计建树预算或解析器提供节点预算时，重新评估这项上限。隐藏检测仍是尽力而为，样式层的视觉隐藏（白色文字、零字号、屏幕外定位）不在范围内。

## Verification

- 第一次修复前（`74f420b`）：经 `go test -overlay` 替换 `html.go`，自闭合 hidden、aria-hidden、`display:none`、`visibility:hidden`、`<p hidden/>`、raw-text 与 foreign 样本共 31 个差分用例和 4 个标准行为用例失败，输出都含被隐藏的文本。
- 本次修复前：把 `5483499`（tokenizer 模拟的最终版本）的 `html.go` 经 overlay 替换进当前测试，`go test -count=1 -overlay <map> -run TestRenderHTML ./internal/adapter/tool/web/` 失败：
  - 7 个误嵌套样本泄漏隐藏文本（Fable 的 4 个重建样本、跨段落重建，以及 adoption agency 保留隐藏块的 2 个样本）。
  - 2 个 noscript 样本丢失可见文本。
  - orphan cells、重建的粗体与链接、adoption 拆分的粗体共 4 个排版与上游不同。
  - foreign `</p>`、`</br>`、template 标记和 512 边界用例也失败。
  - 更早一轮的 27 个 foreign 样本（font 属性存在性、annotation-xml→svg→foreignObject、mglyph/malignmark、CDATA、`</br>`、未匹配 `</p>`）在模拟修补前有 11 个失败，现由解析器通过。
- 新增 11 个 integration point 不 breakout 的样本，覆盖 foreignObject、desc、title、mtext/mi/mo/mn/ms、两种 annotation-xml 和普通 svg g，取代 Fable 报告中存活的两个 integration point mutation；对应的本地判断已删除，没有可变异的位置。
- 从 `html_test.go` 抽取三张表全部 152 个上游期望值，用 submodule 的 `fetch.ts` 转换配置与锁定依赖重算，全部一致。
- opus 的 1,105 个样本差分：本地输出上游隐藏的标记 0 例，上游输出本地隐藏的标记 0 例，本地丢失上游可见文本 0 例。本地丢失 `POST` 的 25 例都是 foreign 中的 template，都带省略标记。上游丢失而本地保留 `POST` 的 24 例是 domino 的 raw-text 缺陷。补充集 c2–c6 没有仅本地输出的隐藏文本。
- 深度表：511 个 span 输出 `x`；512 个 span、512 个 `<div/>`、511 个 span 加 `</p>`、510 个 span 加 svg 时输出省略标记；509 个 span 加 svg 输出 `x`。
- `go test -race -count=1 ./internal/adapter/tool/web/`：通过，逐语句 coverage 100.0%。
- mutation：删除只针对模拟代码的 `web-html-self-closing`、`web-html-foreign-self-closing`、`web-html-foreign-breakout` 以及 `5483499` 中针对模拟规则的 5 项。新增 4 项，单独运行均被杀死：
  - `web-html-scripting-disabled`：开启脚本标志。
  - `web-html-body-context`：上下文改为 head。
  - `web-html-depth-omission`：深度错误输出空串。
  - `web-html-template-marker`：禁用 template 标记。
- 解析器切换（`4c8d690`）时的 `make check`（私有 `GOLANGCI_LINT_CACHE`）：通过；逐产品文件 coverage 100.0%，lint `0 issues`，当时清单中 207 个 mutation 全部 killed，build smoke 输出 `nano-harness dev`。

### 建树成本上限（astra B1）

- 修复前：用 `d8b4090` 的 `html.go` 加一个不拦截的桩替换进当前测试，`go test -count=1 -overlay <map> -run 'TestRenderHTML_(OmitsAmplifiedTreeConstruction|BoundsAmplifiedAllocation)' ./internal/adapter/tool/web/` 失败：未闭合格式元素、被段落留下的格式元素、noscript 和自闭合四种放大形式都没有省略标记，超过上限的开始标签数也照常转换；13,393 字节的放大样本分配 80,500,152 字节，超过 4 MiB 断言。
- 修复后：astra 复现材料中的三个输入（4,795、8,395、12,395 字节）都输出省略标记，分配约 32 KB（此前分别约 8.3 MB、80.5 MB、160.8 MB）。仓库外保存的 8 个真实页面按 200,000 单元截断后得分 1,106–7,342，全部正常转换，分配 0.8–3.2 MB。恰在上限的输入（65,536 个块）可转换，分配约 25–29 MB；多一个开始标签即省略。
- `go test -race -count=1 ./internal/adapter/tool/web/`：通过，逐语句 coverage 100.0%。
- 新增 5 个 mutation，单独运行均被杀死：`web-html-conversion-cost`（跳过检查）、`web-html-cost-formatting-pressure`（不计格式元素）、`web-html-cost-self-closing`（不计自闭合标签）、`web-html-cost-noscript-markup`（noscript 按 raw text 扫描）、`web-html-cost-release`（结束标签不释放格式元素）。自闭合用例最初用不带引号的属性值，斜杠被并入属性值，mutation 存活；改为带引号的属性后被杀死。
- `make check`（私有 `GOLANGCI_LINT_CACHE`）：通过；逐产品文件 coverage 100.0%，lint `0 issues`，清单中 221 个 mutation 全部 killed，build smoke 输出 `nano-harness dev`。第一次运行因测试辅助函数的循环写法被 modernize 报告，改为 range over int 后重跑通过。

未获得的证据：没有对真实网页做批量差分；差分覆盖测试表和合成语料。成本上限只用 8 个真实页面校准，没有大规模网页语料。

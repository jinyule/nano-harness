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
- 建树前，`conversionCost` 按「1 + 属性数」的权重过估计建树量，因为 x/net 的 clone 复制整份属性切片。HTML 内容用一个复用的 x/net tokenizer 计价：解析器只经 5 处回馈改变 tokenizer（`parse.go:645`、`748`、`1096`、`2122` 的 `NextIsNotRawText` 与 `2233` 的 `AllowCDATA`），HTML 内容里只有 noscript 一处，扫描同样处理，所以 token 流与解析器一致，名字、属性、注释、raw text 与引号值都精确，F\* 只在结束标签关闭最内层开放元素时扣除。非自闭合的 `<svg>`/`<math>` 子树内改为按字节计价，权重只增不减；`foreignEnd` 找恢复点时越过注释、CDATA、伪注释、引号属性值与 raw-text 内容的最远终点，SVG/MathML 根分别配平，只会晚恢复。每个子树新建一个 tokenizer，最多 256 个。表格标签另加 `impliedElements`（2），adoption 计 `adoptionClones`（32）乘最大格式权重。累计超过 `maxConversionCost`（2^18）时输出省略标记，不建树。上界论证与取舍写在 ADR-0011。
- 渲染器在输出预算处停止：根层写入达到 `maxRenderBytes` 时停止，不切断 rune。捕获层不消耗这个预算，但任何捕获都不超过根层剩余的预算，所有捕获合计不超过 `maxCapturedBytes`。`renderHTML` 返回是否丢弃，`formatFetch` 把它与其他截断原因取或，所以出现省略标记时 `truncated` 一定为真。
- 运行时 composition、插件 effect、session 格式和工具定义都不变。依赖仍是已有的 `golang.org/x/net` v0.59.0，未改变 `go.mod`。

## Consequences

隐藏范围由完整的 HTML 树构建决定，不再取决于自写规则的覆盖程度；`html.go` 相对 `92e3599` 净减少 107 行，三轮复审报告的泄漏类别都由解析器处理。误嵌套格式元素的泄漏选择对齐而不是记为偏差：泄漏会把应隐藏的文本交给模型，在流式渲染中补齐 adoption agency 需要移动已输出的子树，改动面比换用解析器更大，风险也更高。

代价有四项。第一，引入 x/net 的 template 偏差：只会少输出内容，可检测时有标记，SVG title/style 中的情形无标记。第二，foreign `</p>`、`</br>` 之后的文本与上游不同。第三，深度上限改按解析器的开放元素栈计算，与上游的词法计数在边界处不同。x/net 修复 template 偏差、参考指针更新 domino，或上游改变移除规则时，需要重新评估这些差异和对应测试。第四，建树成本上限比上游更严：格式元素长期不闭合、又跟着大量块的页面会被省略；已测真实页面得分不超过约 7,400，上限留有约 9 倍余量。上游加入累计建树预算或解析器提供节点预算时，重新评估这项上限。隐藏检测仍是尽力而为，样式层的视觉隐藏（白色文字、零字号、屏幕外定位）不在范围内。

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

### 预扫改为过估计（Codex 复审）

Codex 复审 `633844b` 时指出预扫并不保守：它沿用 tokenizer 的默认 raw text 行为，而 SVG `title` 内的标记在解析器中按上下文继续建树。9,724 字节样本的估算为 2，实际建出 101,103 个元素；100,000 单元样本放行后累计分配 1.11 GiB（隐藏版）到 18.9 GiB（可见版）。隐式元素（`x</p>` 生成的段落）与表格补全也被低估，因此 ADR 中“畸形输入只会高估”“接受的树约为 13 万个节点以内”两句不成立。

- 修复前：把 `633844b` 的 `html.go` 经 overlay 换入当前测试（加一层把旧扫描暴露成新签名的壳），`go test -count=1 -overlay <map> -run 'TestRenderHTML_OmitsAmplifiedTreeConstruction|TestConversionCost' ./internal/adapter/tool/web/` 失败：SVG `title` 样本未省略；`TestConversionCost_BoundsParsedNodes/svg_title` 报告实际 20,504 个节点、上界 32（cost=2、tokens=6）；各上下文的精确费用用例与预算边界用例同样失败。
- 修复后：Codex 的两个 100,000 单元样本经 `formatFetch` 都返回省略标记，分配 0.21 MiB。
- 过估计的取舍：旧扫描把 `<b><p>x</b>`×500 估成 500,500，实际只有 2,000 个节点；新扫描按 adoption agency 必然移除的规则放行，费用降到 1,500，该输入正常转换。
- 已测页面费用为 556–9,244（上限 262,144，余量约 28 倍）：8 个仓库外保存的真实页面与 Codex 的 3 个合成页面，放行分配 0.8–3.3 MB。恰好等于上限的输入转换耗时约 51 ms、分配约 37 MB。
- 最坏放行形态：512 层嵌套格式元素用满预算时分配约 159 MB，量级为 `深度上限 × 上限`，因为渲染器把每层嵌套的捕获缓冲复制给上层；已写入 ADR-0011，降低上限会按比例降低它。
- `TestConversionCost_BoundsParsedNodes` 用 15 个反例样本和 4,000 个生成样本断言 `tokens×(1+impliedElements)+cost` 不低于生产解析器实际建出的节点数。开发期间用同一断言跑过 52 万个随机样本：第一版（marker 栈）在 `<select>`、`<svg><title>`、`<svg><foreignObject>` 三处违反，改为词法良构规则后 0 违反，最紧样本的实际/上界比为 0.40。
- mutation：替换 `633844b` 的 5 项，现为 8 项，单独运行均被杀死：`web-html-conversion-cost`（跳过检查）、`web-html-cost-boundary`（`>` 改 `>=`）、`web-html-cost-reversed`（`>` 改 `<`）、`web-html-cost-raw-text`（去掉 `NextIsNotRawText`，即本次缺陷）、`web-html-cost-pressure`（不计格式元素）、`web-html-cost-well-nested`（结束标签不检查栈顶）、`web-html-cost-context`（忽略上下文元素）、`web-html-cost-self-closing`（自闭合标签不入栈）。
- `make check`（私有 `GOLANGCI_LINT_CACHE`）：通过；逐产品文件 coverage 100.0%，lint `0 issues`，清单中 224 个 mutation 全部 killed，build smoke 输出 `nano-harness dev`。期间修掉两处 lint（生成样本的 `math/rand` 需要带理由的 `//nolint:gosec`，反向遍历改用 `slices.Backward`），并把随之失配的 `web-html-cost-context` 重新对准新代码。

### 预扫改为按权重的字节扫描（Codex、opus、Fable 三方复审）

Codex 指出按 tokenizer 默认 raw text 扫描会低估（SVG `title` 内的标记被当作文本，9,724 字节样本估算 2、实际 101,103 个元素）。我据此改成"关闭 raw text"的 tokenizer 扫描后，opus 与 Fable 又各自证明了三类低估：

- 关闭 raw text 后，`<script><!--</script>` 之后的 `<!--` 被当作注释开头吞掉全部标记（估算 1、实际 101,102 个元素）。注释与 raw text 的切分取决于解析器上下文，两个方向都会低估。
- adoption agency 外层 8 轮上限会让克隆残留在活动格式元素列表中，`<a>` 开始标签的 `remove` 删除的是旧指针（实际 51,892 个元素）。因此"同名结束标签就减一"不成立。
- clone 复制整份属性切片：8,000 个属性的格式元素只占 1 个元素名额，1,000 段文本后实际权重 8,010,002、分配约 376 MB。按元素或节点计数都约束不住。

维护者决定不 fork x/net，采用字节扫描的保守估算。实现与验证：

- 开发期间差分断言连续发现并修掉了四处我自己的低估：元素自身的属性未计入插入点费用；属性名可以由 `<`、`!` 等字符开头，也可以在 `/` 之后重新开始；标签内的 `<` 即使位于引号中也必须成为切点（否则 `<xmp>` 内的伪标签会吞掉真实标记）；以及 `impliedElements` 按每个插入点收 4 会误省略密集的普通页面，改为只对表格标签收 2。
- 差分性质测试：`TestConversionCost_BoundsParsedWeight` 断言放行输入满足"估算 ≥ 实际权重"，种子含 P1–P3、opus 的漏洞 A/B、Codex 的反例与 Fable 6.4 节的回归向量（9 种 foreign raw-text 包装、MathML title、3 例注释吞并、breakout 后的注释吞并、单元格与 select 内被忽略的结束标签、属性复制、`x</p>`、隐式表格）。定稿代码另跑了 192 万个生成样本：0 违反，最紧样本的实际/估算比为 0.929。
- 放大向量全部输出省略标记；常见页面全部在预算内：真实页面费用 2,066–127,177（最高者 MDN 占上限 48%），复审者的文章/文档/表格页面 20,807–45,030，3,000 个链接 27,003，20 万单元密集段落 100,000，宽字符文本 16,000，`go_spec.html` 截到 100 KB 为 7,582（opus 方案下为 103,076）。
- 分配：放行输入实测 0.5–9.4 MB；恰好用满预算的最坏形态 7.4 MB，此前同一形态 159 MB。astra 的两个 100,000 单元可见/隐藏样本经 `formatFetch` 返回省略标记。
- 渲染器上限：1 MB 文本与 40 万个宽字符的输入都在 600,051 字节处停止并带省略标记，输出仍是有效 UTF-8；宽字符页面仍能交付完整的 200,000 单元输出预算。
- mutation：替换此前 8 项，现为 13 项，单独运行均被杀死——预算比较的三种变异（禁用、`>=`、`<`）、属性权重、元素自身权重、文本插入点、两处 adoption 计费、栈顶判定、标签切点、唯一签名、渲染上限与渲染停止点。
- `make check`（私有 `GOLANGCI_LINT_CACHE`）：通过；逐产品文件 coverage 100.0%，lint `0 issues`，清单中 229 个 mutation 全部 killed，build smoke 输出 `nano-harness dev`。

局限：上界论证依据 HTML 规范与 x/net/html v0.59.0 的源码（8 轮上限、重建路径、clone 的属性复制），没有形式化证明；升级 x/net 时必须重新核对，ADR-0011 已把它列入复审触发条件。签名各不相同的格式元素特别多的页面会被省略，这是已记录的取舍。

### 混合路线：HTML 内容精确、foreign 子树内保守（astra 四项 Blocker 与 Fable 复审）

astra 复审 `3339104` 报出四项，Fable 独立确认并扩展：字节扫描的扣减会被注释、伪注释、`</` 后接非字母、doctype、引号属性值与 raw-text 结束形式骗过（80 KB 可放大到约 552 MB）；自写的标签名与属性语法与 x/net 不一致；渲染预算在捕获层之间重复计费，且整页省略时 `truncated` 为 false（后者 `633844b` 就存在）。Fable 指出"可能不是标签"的区段方案要照 x/net 状态机实现并维护单调地平线，实现量接近自写 tokenizer，而逐标签新建 tokenizer 分配过多（2 万个标签约 88 MB）。

- 按协调者转达的 Fable 方案改为混合路线：HTML 内容用一个复用的精确 tokenizer，foreign 内容用字节扫描。第一版"第一个 svg/math 之后整页只增不减"在 11 个真实页面中误省略 4 个（MDN、Python 文档、GitHub、pkg.go.dev，均因靠前的小 SVG 图标），按要求停手回报；维护者选定只在 SVG/MathML 子树内按字节计价，恢复点必须保守。
- 恢复点：`foreignEnd` 在子树内对每个 `<` 求所有读法的最远终点（注释到 `-->`、CDATA 到 `]]>`、伪注释到 `>`、标签按引号值读到结尾、raw-text 内容到合法结束标签，script 含注释开启符或 plaintext 时到输入结尾），只有越过这些区段且 SVG、MathML 根都配平的结束标签才作为恢复点；开始标签不论位置都计入深度。breakout 只会让恢复更晚。
- 渲染：捕获受根层剩余预算约束（克隆长链接的形态由 23 MB 降到约 6.8 MB），捕获合计另有上限；`renderHTML` 返回是否丢弃，`formatFetch` 取或，整页省略时 `truncated` 为真。
- 证据：Fable 列出的 23 种结束形式、astra 的四项反例与放大版、子树内用注释/CDATA/引号值/raw text 藏 `</svg>`、嵌套 svg/math、breakout、未闭合 svg 全部 sound 或被省略；差分 fuzz 216 万个样本（含 svg 前缀、长尾、链接尾）0 违反，最紧比 0.995。常见页面：11 个真实页面费用 2,050–120,161 全部转换（数据与 SHA-256 前缀见测试策略）；SVG 图标后接 900 个链接 8,105；1,000 个小 svg 后接链接 114,374，扫描分配 1.1 MB。移植的标签读取器经 20,000 个生成标签对照 x/net 从不少计属性。
- mutation：现为 23 项，含预算比较三种变异、属性与元素权重、文本插入点、两处 adoption、栈顶、noscript、foreign 切换、子树权重不可释放、恢复点的地平线/raw text/深度/注释、子树数上限、子树内属性计数、渲染停止点、捕获合计上限、捕获剩余预算与丢弃即截断，单独运行均被杀死。
- `make check`（私有 `GOLANGCI_LINT_CACHE`）：通过；逐产品文件 coverage 100.0%，lint `0 issues`，清单中 239 个 mutation 全部 killed，build smoke 输出 `nano-harness dev`。期间修掉三处 lint（两处 token 类型 switch 补全 exhaustive、一处空循环体），并把随重构失配的三项 mutation 重新对准。

未获得的证据：没有对真实网页做批量差分；差分覆盖测试表、复审向量与生成语料。费用校准用 11 个页面快照（快照不入库，测试策略记录 SHA-256 前缀），没有大规模网页语料。上界论证依据 x/net/html v0.59.0 源码中的 5 处回馈，没有形式化证明；生成样本只覆盖所列标记组合，不构成对全部 HTML 输入的上界证明。

### 已知缺口与遗留决定

Fable 对 `02dafbf` 的复审（RB-H1、RB-H2、RB-H3）与 astra 的复审（五类低估、两项 Suggestion）由 opus 交叉核实，证伪了 ADR-0011 原有的三处论证：HTML 内容中 token 流与解析器一致、恢复点只会推迟、foreign 子树内的属性数与签名是保守的。维护者决定不再继续修补，把这些问题作为已知缺口遗留，遇到真实页面触发或内存问题报告时再修。

- ADR-0011 第 7 条改写为“估算是尽力而为的上界”，逐条列出五类低估的最小反例与量级：`<template><col>` 之后被解析器忽略的 noscript 仍回馈（RB-H1）、以 `/` 结尾的无引号属性值被判为自闭合（RB-H2）、integration point 中被忽略的根结束标签仍扣深度、属性中的 `<` 干扰根类型识别、在 `<` 处切开标签导致属性数与签名少算。前两项各有放大样本：113 KB 输入估算恒为 6，单次转换累计分配约 593 MB，预算被完全绕过。
- 同时记录三项已知限制：`flush` 的分隔符绕过预算计数；SVG title、style 或 noscript 内的 template 被丢弃时检测不到，`truncated` 漏报；捕获上限触发后整页只剩省略标记（RB-H3）。
- 考虑过但没有采用：在 fork 的 x/net 建树代码中计数，以及完整的并行解释地平线。两者都能消除这些低估，但前者影响依赖与供应链，后者实现量接近自写 tokenizer；真实场景出现时再比较二者。
- `docs/security.md` 写明 web_fetch 的 HTML 转换资源上限并不严格，免审批的 web_fetch 遇到恶意页面时可能消耗大量内存；`docs/testing.md` 写明差分 fuzz 没有覆盖这些情形；参考分析的 web 偏差表增加一行。没有新增会失败的测试，也没有修改代码。

验证：本节只改文档。上述反例与放大量级来自 Fable、astra 在仓库外副本中的复现，本 Note 没有重新运行。

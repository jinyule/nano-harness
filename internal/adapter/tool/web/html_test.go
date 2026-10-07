package web

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Expected semantics come from Turndown 7.2.4 with @joplin/turndown-plugin-gfm
// 1.0.67 and tool-web/src/fetch.ts at upstream 5badb15009ae. The upstream
// column records its exact output; want differs only in equivalent spacing
// and the horizontal-rule marker documented in ADR-0011.
func TestRenderHTML_MatchesUpstreamSemantics(t *testing.T) {
	for _, test := range []struct {
		name, source, upstream, want string
	}{
		{"del", "<del>old</del>", "~~old~~", "~~old~~"},
		{"s", "<s>old</s>", "~~old~~", "~~old~~"},
		{"strike", "<strike>old</strike>", "~~old~~", "~~old~~"},
		{"nested strike", "<p>old <del><b>unsafe</b></del> new</p>", "old ~~**unsafe**~~ new", "old ~~**unsafe**~~ new"},
		{"empty strike", "<del> </del>visible", "visible", "visible"},
		{"checked task", `<ul><li><input type="checkbox" checked>done</li></ul>`, "-   [x] done", "- [x] done"},
		{"unchecked task", `<ul><li><input type="checkbox">todo</li></ul>`, "-   [ ] todo", "- [ ] todo"},
		{"boolean checked", `<ul><li><input type="CHECKBOX" checked="false" disabled>done</li></ul>`, "-   [x] done", "- [x] done"},
		{"checkbox outside item", `<input type="checkbox" checked>outside`, "outside", "outside"},
		{"hidden task", `<ul><li><input type="checkbox" hidden checked>text</li></ul>`, "-   text", "- text"},
		{"language", "<pre><code class=language-go>a\n  b\n</code></pre>", "```go\na\n  b\n```", "```go\na\n  b\n```"},
		{"language class list", "<pre><code class=\"other language-go extra\">a\n</code></pre>", "```go\na\n```", "```go\na\n```"},
		{"fence in code", "<pre><code>```\nx\n```</code></pre>", "````\n```\nx\n```\n````", "````\n```\nx\n```\n````"},
		{"code literals", "<pre><code class=language-text>*x* [y] \\z</code></pre>", "```text\n*x* [y] \\z\n```", "```text\n*x* [y] \\z\n```"},
		{"inline code literals", "<code>*x* [y] \\z `tick`</code>", "`` *x* [y] \\z `tick` ``", "`` *x* [y] \\z `tick` ``"},
		{"markdown literals", "<p>literal *asterisk* and [x](y) _u_ `c` \\path</p>", "literal \\*asterisk\\* and \\[x\\](y) \\_u\\_ \\`c\\` \\\\path", "literal \\*asterisk\\* and \\[x\\](y) \\_u\\_ \\`c\\` \\\\path"},
		{"block literals", "<p># heading</p><p>- bullet</p><p>+ bullet</p><p>1. item</p><p>&gt; quote</p><p>---</p>", "\\# heading\n\n\\- bullet\n\n\\+ bullet\n\n1\\. item\n\n\\> quote\n\n\\---", "\\# heading\n\n\\- bullet\n\n\\+ bullet\n\n1\\. item\n\n\\> quote\n\n\\---"},
		{"tilde fence literal", "<p>~~~code</p>", `\~~~code`, `\~~~code`},
		{"setext literal", "<p>x<br>=</p>", "x  \n\\=", "x  \n\\="},
		{"escaped link label", `<a href="/x">[literal] *star*</a>`, `[\[literal\] \*star\*](/x)`, `[\[literal\] \*star\*](/x)`},
		{"hidden implicit p", "<p hidden>secret<p>visible", "visible", "visible"},
		{"hidden nested implicit p", "<p hidden><span>secret<p>visible", "visible", "visible"},
		{"hidden p before div", "<p hidden>secret<div>visible</div>tail", "visible\n\ntail", "visible\n\ntail"},
		{"implicit li", "<ul><li>one<li>two</ul>", "-   one\n-   two", "- one\n- two"},
		{"hidden implicit li", "<ul><li hidden>secret<li>visible</ul>", "-   visible", "- visible"},
		{"hidden implicit dt", "<dl><dt hidden>secret<dd>visible</dl>", "visible", "visible"},
		{"hidden implicit option", "<select><option hidden>secret<option>visible</select>", "visible", "visible"},
		{"hidden implicit optgroup", "<select><optgroup hidden label=secret><option>secret<optgroup label=visible><option>visible</select>", "visible", "visible"},
		{"hidden heading", "<h1 hidden>secret<h2>visible</h2>", "## visible", "## visible"},
		{"hidden p before hr", "<p hidden>secret<hr>visible", "* * *\n\nvisible", "---\n\nvisible"},
		{"nested item scope", "<ul><li>outer<ul><li>inner<li>next</ul><li>last</ul>", "-   outer\n    -   inner\n    -   next\n-   last", "- outer\n  - inner\n  - next\n- last"},
		{"image label literals", `<img src="/a" alt="[foo] *bar*">`, `![\[foo\] \*bar\*](/a)`, `![\[foo\] \*bar\*](/a)`},
		{"link destination parentheses", `<a href="/a(b)">[foo]</a>`, `[\[foo\]](/a\(b\))`, `[\[foo\]](/a\(b\))`},
		{"link destination spaces", `<a href="/a b">link</a>`, `[link](</a b>)`, `[link](</a b>)`},
		{"image destination delimiters", `<img src="/a&lt;b&gt;.png" alt="image">`, `![image](/a\<b\>.png)`, `![image](/a\<b\>.png)`},
		{"hard break", "<p>a<br>b</p>", "a  \nb", "a  \nb"},
		{"quoted hard break", "<blockquote><p>a<br>b</p></blockquote>", "> a  \n> b", "> a  \n> b"},
		{"code whitespace", "<pre><code class=language-go>  x  \n\n\n  y\t\n</code></pre>", "```go\n  x  \n\n\n  y\t\n```", "```go\n  x  \n\n\n  y\t\n```"},
		{"quoted code whitespace", "<blockquote><pre><code class=language-go>x  \n\ny\t\n</code></pre></blockquote>", "> ```go\n> x  \n> \n> y\t\n> ```", "> ```go\n> x  \n>\n> y\t\n> ```"},
		{"title keeps paragraph scope", "<p hidden>secret<title>still secret</title></p><p>visible", "visible", "visible"},
		{"button keeps paragraph scope", "<p hidden>secret<button><p>still secret</p></button></p><p>visible", "visible", "visible"},
		// A self-closing slash on a non-void HTML element is ignored, so the
		// element stays open and hides everything up to its end tag.
		{"self-closing hidden div", `<div hidden/>secret</div><p>visible</p>`, "visible", "visible"},
		{"self-closing aria-hidden", `<span aria-hidden="TRUE"/>secret</span><p>after</p>`, "after", "after"},
		{"self-closing visibility hidden", `<section style="visibility: hidden !important"/>secret</section><p>visible</p>`, "visible", "visible"},
		{"self-closing display none", `<p style="display:none"/>secret<p>visible</p>`, "visible", "visible"},
		{"self-closing hidden keeps children", `<div hidden/><p>still hidden</p></div><p>visible</p>`, "visible", "visible"},
		{"self-closing hidden inline", `<p>a<span hidden/>b</span>c</p>`, "ac", "ac"},
		{"self-closing hidden item", `<ul><li hidden/>gone<li>kept</ul>`, "-   kept", "- kept"},
		{"self-closing hidden before div", `<p hidden/>secret<div>visible</div>`, "visible", "visible"},
		{"self-closing hidden input", `<input type="hidden" value="v"/><p>visible</p>`, "visible", "visible"},
		{"self-closing display none span", `<span style="display:none"/>secret</span><p>visible</p>`, "visible", "visible"},
		{"self-closing display none uppercase", `<span style="DISPLAY: NONE"/>secret</span><p>visible</p>`, "visible", "visible"},
		{"self-closing hidden paragraph", `<p hidden/>secret</p><p>visible</p>`, "visible", "visible"},
		{"self-closing aria-hidden div", `<div aria-hidden="true"/>secret</div><p>visible</p>`, "visible", "visible"},
		{"self-closing template content", `<template/>tpl</template><p>visible</p>`, "visible", "visible"},
		{"self-closing object", `<object/>secret</object><p>visible</p>`, "visible", "visible"},
		{"self-closing strong", `<b/>bold</b> plain`, "**bold** plain", "**bold** plain"},
		{"self-closing link", `<p/>text<a href="x"/>link</a><br/><img src="/i.png"/>`, "text[link](x)  \n![](/i.png)", "text[link](x)  \n![](/i.png)"},
		{"self-closing noscript", `<noscript/>secret</noscript><p>visible</p>`, "visible", "visible"},
		{"self-closing template", `<template/>secret</template><p>visible</p>`, "visible", "visible"},
		{"self-closing script at end", `<p>before</p><script/>secret</script>`, "before", "before"},
		{"raw script markup", `<script>if (a</div>) leak()</script><p>visible</p>`, "visible", "visible"},
		{"raw style markup", `<div><style>p{}</div></style>still</div>`, "still", "still"},
		{"raw title entities", `<title>Guide &amp; more</title><p>body</p>`, "Guide & more\n\nbody", "Guide & more\n\nbody"},
		{"hidden raw text", `<div hidden><script>x</script>secret</div><p>visible</p>`, "visible", "visible"},
		// In SVG and MathML a self-closing tag closes the element, and raw-text
		// names are ordinary elements; integration points and breakout tags
		// return to HTML rules.
		{"foreign self-closing hidden", `<svg><g hidden/>visible</g></svg>`, "visible", "visible"},
		{"foreign self-closing", `<svg><rect/><text>label</text></svg><p>after</p>`, "label\n\nafter", "label\n\nafter"},
		{"foreign title is not raw", `<svg><title/>visible<desc>d</desc></svg>`, "visibled", "visibled"},
		{"foreign style self-closed", `<svg><style/>css</svg><p>after</p>`, "css\n\nafter", "css\n\nafter"},
		{"foreign hidden text", `<svg viewBox="0 0 1 1"><g/><text hidden/>t</svg><p>after</p>`, "t\n\nafter", "t\n\nafter"},
		{"foreignObject is html", `<svg><foreignObject><div hidden/>secret</div></foreignObject></svg><p>visible</p>`, "visible", "visible"},
		{"foreign breakout", `<svg><div hidden/>secret</div></svg><p>visible</p>`, "visible", "visible"},
		{"font color breakout", `<svg><font color="red" hidden/>secret</font></svg><p>visible</p>`, "visible", "visible"},
		{"font size breakout", `<svg><font size="2" hidden/>secret</font></svg><p>visible</p>`, "visible", "visible"},
		{"font face breakout", `<svg><font face="a" hidden/>secret</font></svg><p>visible</p>`, "visible", "visible"},
		{"plain font stays foreign", `<svg><font hidden/>x</svg><p>after</p>`, "x\n\nafter", "x\n\nafter"},
		{"mathml self-closing", `<math><mi hidden/>x</math><p>visible</p>`, "x\n\nvisible", "x\n\nvisible"},
		{"mathml text integration", `<math><mi><span hidden/>secret</span></mi></math><p>visible</p>`, "visible", "visible"},
		{"mathml mtext is html", `<math><mtext><b/>bold</b></mtext></math>`, "**bold**", "**bold**"},
		{"annotation-xml html", `<math><annotation-xml encoding="text/html"><div hidden/>secret</div></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"annotation-xml xhtml", `<math><annotation-xml encoding="Application/XHTML+XML"><p hidden/>secret</p></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"annotation-xml html unknown element", `<math><annotation-xml encoding="text/html"><g hidden/>x</g></annotation-xml></math><p>after</p>`, "after", "after"},
		{"annotation-xml without encoding", `<math><annotation-xml><g hidden/>x</annotation-xml></math><p>after</p>`, "x\n\nafter", "x\n\nafter"},
		// Foreign-content rules from the tree construction dispatcher: font breaks
		// out when color, face, or size is present at all; mglyph and malignmark
		// stay MathML below a text integration point; svg below annotation-xml
		// starts SVG; the annotation-xml encoding must match exactly; CDATA is
		// text only in foreign content.
		{"font empty color breakout", `<svg><font color="" hidden/>secret</font></svg><p>visible</p>`, "visible", "visible"},
		{"font valueless size breakout", `<svg><font size hidden/>secret</font></svg><p>visible</p>`, "visible", "visible"},
		{"font blank face breakout", `<svg><font face=" " hidden/>secret</font></svg><p>visible</p>`, "visible", "visible"},
		{"font other attribute stays foreign", `<svg><font class="x" hidden/>x</svg><p>after</p>`, "x\n\nafter", "x\n\nafter"},
		{"annotation-xml svg foreignObject", `<math><annotation-xml><svg><foreignObject><x hidden/>secret</x></foreignObject></svg></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"annotation-xml svg desc", `<math><annotation-xml><svg><desc><b hidden/>secret</b></desc></svg></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"annotation-xml svg self-closing", `<math><annotation-xml><svg><g hidden/>x</svg></annotation-xml></math><p>after</p>`, "x\n\nafter", "x\n\nafter"},
		{"annotation-xml padded encoding", `<math><annotation-xml encoding=" text/html"><g hidden/>x</annotation-xml></math><p>after</p>`, "x\n\nafter", "x\n\nafter"},
		{"annotation-xml uppercase encoding", `<math><annotation-xml encoding="TEXT/HTML"><g hidden/>secret</g></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"mtext mglyph stays mathml", `<math><mtext><mglyph><g hidden/>visible</g></mglyph></mtext></math>`, "visible", "visible"},
		{"mtext malignmark stays mathml", `<math><mtext><malignmark><g hidden/>visible</g></malignmark></mtext></math>`, "visible", "visible"},
		{"mi self-closing mglyph", `<math><mi><mglyph/>x</mi></math>`, "x", "x"},
		{"mglyph breakout stops at mo", `<math><mo><mglyph><b hidden/>secret</b></mglyph></mo></math><p>visible</p>`, "visible", "visible"},
		{"svg cdata", `<svg><![CDATA[visible]]></svg>`, "visible", "visible"},
		{"foreignObject cdata", `<svg><foreignObject><![CDATA[inside]]></foreignObject></svg>`, "inside", "inside"},
		{"html cdata is a comment", `<p><![CDATA[hidden]]>after</p>`, "after", "after"},
		{"self-closing svg", `<svg/>after`, "after", "after"},
		{"self-closing hidden math", `<math hidden/>after`, "after", "after"},
		{"svg inside svg title", `<svg><title><svg><g hidden/>x</g></svg></title></svg>`, "x", "x"},
		{"math inside svg is svg", `<svg><math><mi><b hidden/>x</b></mi></math></svg><p>after</p>`, "after", "after"},
		{"svg inside math is mathml", `<math><svg><foreignObject><b hidden/>x</b></foreignObject></svg></math><p>after</p>`, "after", "after"},
		// End tags: </br> acts as <br> and an unmatched </p> as an empty paragraph.
		{"end br", `a</br>b`, "a  \nb", "a  \nb"},
		{"unmatched end p", `a</p>b`, "a\n\nb", "a\n\nb"},
		{"orphan cells", "<td>loose</td><tr><td>row</td></tr>", "looserow", "looserow"},
		// Integration points with an element that is not a breakout tag, so the
		// result depends on the integration point alone.
		{"foreignObject non-breakout", `<svg><foreignObject><section hidden/>secret</section></foreignObject></svg><p>visible</p>`, "visible", "visible"},
		{"desc non-breakout", `<svg><desc><section hidden/>secret</section></desc></svg><p>visible</p>`, "visible", "visible"},
		{"title non-breakout", `<svg><title><section hidden/>secret</section></title></svg><p>visible</p>`, "visible", "visible"},
		{"mtext non-breakout", `<math><mtext><section hidden/>secret</section></mtext></math><p>visible</p>`, "visible", "visible"},
		{"mi non-breakout", `<math><mi><section hidden/>secret</section></mi></math><p>visible</p>`, "visible", "visible"},
		{"mo non-breakout", `<math><mo><section hidden/>secret</section></mo></math><p>visible</p>`, "visible", "visible"},
		{"mn non-breakout", `<math><mn><section hidden/>secret</section></mn></math><p>visible</p>`, "visible", "visible"},
		{"ms non-breakout", `<math><ms><section hidden/>secret</section></ms></math><p>visible</p>`, "visible", "visible"},
		{"annotation-xml html non-breakout", `<math><annotation-xml encoding="text/html"><section hidden/>secret</section></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"annotation-xml svg non-breakout", `<math><annotation-xml><svg><foreignObject><section hidden/>SECRET</section></foreignObject></svg></annotation-xml></math><p>visible</p>`, "visible", "visible"},
		{"foreign non-breakout self-closes", `<svg><g><section hidden/>x</g></svg><p>after</p>`, "x\n\nafter", "x\n\nafter"},
		// Misnested formatting: elements closed by an outer end tag or an implied
		// end are reconstructed with their attributes, and the adoption agency
		// keeps a hidden block open. Upstream renders the emptied wrappers as
		// ~~~~ and [](url); the converter omits empty wrappers.
		{"reconstructed hidden bold", `<i><b hidden>x</i>SECRET</b><p>visible</p>`, "visible", "visible"},
		{"reconstructed hidden bold to end", `<i><b hidden>x</i>SECRET<p>visible</p>`, "", ""},
		{"reconstructed display none emphasis", `<s><em style="display:none">x</s>SECRET<p>visible</p>`, "~~~~", ""},
		{"reconstructed aria-hidden strong", `<a href="/a"><strong aria-hidden="true">x</a>SECRET<p>visible</p>`, "[](/a)", ""},
		{"reconstructed across paragraphs", `<p><b hidden>x<p>SECRET</p><p>also</p>`, "", ""},
		{"adoption keeps hidden block", `<b>1<p hidden>2</b>3</p><p>visible</p>`, "**1**\n\nvisible", "**1**\n\nvisible"},
		{"adoption keeps hidden div", `<a href="x"><div hidden>y</a>SECRET</div><p>visible</p>`, "[](x)\n\nvisible", "visible"},
		{"adoption pops plain inline", `<b><p><span hidden>x</b>y</span></p>`, "y", "y"},
		{"reconstructed bold", `<p><b>bold<p>more</p>`, "**bold**\n\n**more**", "**bold**\n\n**more**"},
		{"reconstructed link", `<p><a href="x">link<p>next</p>`, "[link](x)\n\n[next](x)", "[link](x)\n\n[next](x)"},
		{"adoption splits bold", `<b>1<p>2</b>3</p>`, "**1**\n\n**2**3", "**1**\n\n**2**3"},
		{"noah's ark", `<b hidden><b><b><b>x</b></b></b></b>visible`, "visible", "visible"},
		// The reference parser runs with scripting disabled, so noscript holds
		// markup that implied end tags can close.
		{"noscript markup", `<noscript><p>hidden</p></noscript><p>visible</p>`, "visible", "visible"},
		{"noscript closed by paragraph", `<p>PRE</p><svg><p><noscript/><p>HID</p></noscript></p></svg><p>POST</p>`, "PRE\n\nHID\n\nPOST", "PRE\n\nHID\n\nPOST"},
		{"noscript text then paragraph", `<p>PRE</p><svg><p><noscript/>HID<p>VIS</p></noscript></p></svg><p>POST</p>`, "PRE\n\nVIS\n\nPOST", "PRE\n\nVIS\n\nPOST"},
		{"html end tag closes foreign", `<div><svg><g hidden></div>secret</g></svg></div><p>visible</p>`, "secret\n\nvisible", "secret\n\nvisible"},
		{"unmatched end tag in foreign", `<svg><g hidden></div>secret</g></svg><p>visible</p>`, "visible", "visible"},
		{"implicit table cells and rows", "<table><thead><tr><th>A<th>B<tbody><tr><td>1<td>2<tr><td>3<td>4</table>", "| A   | B   |\n| --- | --- |\n| 1   | 2   |\n| 3   | 4   |", "| A | B |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%q\nwant=%q\nupstream=%q", got, test.want, test.upstream)
			}
		})
	}
}

func TestRenderHTML_ConvertsVisibleContent(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{"document", "<!doctype html><html><head><title>Guide</title><meta charset=utf-8><link rel=x></head><body><h1>Intro  <i></i></h1><p>Hello&nbsp;&amp; <b>bold</b> <strong> </strong><em>soft</em>.</p></body></html>", "Guide\n\n# Intro\n\nHello & **bold** _soft_."},
		{"whitespace", "<p>  one\n\t two  </p>  <div>three</div>", "one two\n\nthree"},
		{"headings", "<h2>Two</h2><h6>Six</h6>", "## Two\n\n###### Six"},
		{"links", `<p>See <a href="https://go.dev"> the <b>docs</b> </a>, <a>plain</a>, <a href="javascript:alert(1)">script</a> and <a href="/x"></a>.</p>`, "See [the **docs**](https://go.dev), plain, script and ."},
		{"images", `<img src="/a.png" alt=" A  B "><img src="data:image/png;base64,AAAA" alt="inline"><img alt="">`, "![A B](/a.png)inline"},
		{"code", "<p>Run <code>go test</code> or <kbd></kbd></p><pre><code>line 1\n  line 2</code></pre><pre>tail\n</pre>", "Run `go test` or\n\n```\nline 1\n  line 2\n```\n\n```\ntail\n```"},
		{"breaks", "<p>a<br>b</p><hr><pre>x<br>y</pre>", "a  \nb\n\n---\n\n```\nx\ny\n```"},
		{"lists", `<ul><li>one</li><li><p>two</p><ol start="3"><li>three</li><li>four</li></ol></li></ul><ol><li>first</li></ol><li>orphan</li>`, "- one\n- two\n\n  3. three\n  4. four\n\n1. first\n\n- orphan"},
		{"empty item", "<ul><li></li><li>b</li></ul>", "-\n- b"},
		{"quote", "<blockquote><p>quoted</p><p>more</p></blockquote>after", "> quoted\n>\n> more\n\nafter"},
		{"table", "<table><thead><tr><th>A|B</th><th>C</th></tr></thead><tbody><tr><td>1</td><td><b>2</b></td></tr><tr></tr></tbody></table>", "| A\\|B | C |\n| --- | --- |\n| 1 | **2** |"},
		{"table without header", "<table><tr><td>x</td><th>y</th></tr></table>", "| x | y |"},
		{"hidden", `<p>shown</p><div hidden>no</div><span aria-hidden="TRUE">no</span><span aria-hidden="false">yes</span><p style="color:red; display : none !important">no</p><p style="visibility:collapse">no</p><p style="visibility:hidden">no</p><p style="display">kept</p><input type="hidden" value="x"><div hidden><img src="/x.png"><br></div>`, "shown\n\nyes\n\nkept"},
		{"removed", "<script>alert('x')</script><style>p{}</style><noscript>n</noscript><template><p>t</p></template><iframe src=x>f</iframe><object>o</object><embed src=x><p>visible</p>", "visible"},
		{"malformed", "<div><p>open<span>nested</div></b>after<p>last", "opennested\n\nafter\n\nlast"},
		{"comments", "a<!-- hidden -->b", "ab"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%q\nwant=%q", got, test.want)
			}
		})
	}
}

// Where Turndown's domino 2.2.0 parser departs from the current HTML standard,
// the converter follows the standard. domino does not record a self-closing
// raw-text tag as the last start tag, so its raw text ends at the wrong end
// tag: it swallows the rest of the page, emits literal end tags, or ends at an
// ancestor's end tag and leaks script text. It also predates the rule that
// </p> and </br> in SVG or MathML end the foreign content; browsers render the
// following text outside the hidden element. The upstream column records the
// observed reference output.
func TestRenderHTML_FollowsHTMLStandardWhereReferenceDiverges(t *testing.T) {
	for _, test := range []struct {
		name, source, upstream, want string
	}{
		{"script", `<script/>alert(1)</script><p>visible</p>`, "", "visible"},
		{"style", `<style/>body{color:red}</style><p>visible</p>`, "", "visible"},
		{"iframe", `<iframe/>secret</iframe><p>visible</p>`, "", "visible"},
		{"script inside ancestor", `<div><script/>"</div>"; leaked</script></div><p>visible</p>`, "\"; leaked\n\nvisible", "visible"},
		{"textarea", `<textarea/>typed &amp; kept</textarea>`, "typed & kept</textarea></x-turndown>", "typed & kept"},
		{"title", `<title/>Guide</title><p>body</p>`, "Guide</title><p>body</p></x-turndown>", "Guide\n\nbody"},
		{"foreign end p", `<svg><g hidden></p>visible</g></svg>`, "", "visible"},
		{"foreign end br", `<svg><g hidden></br>visible</svg>`, "", "visible"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%q\nwant=%q\nupstream=%q", got, test.want, test.upstream)
			}
		})
	}
}

// x/net/html ignores the rest of the input at a template start tag processed
// while SVG or MathML is open, a divergence its source documents. The
// converter keeps what was parsed and appends the omission marker when the
// lexical template count exceeds the parsed one. A template inside an SVG
// title or style is read as raw text by the plain tokenizer and goes unmarked.
func TestRenderHTML_MarksTemplateDroppedInForeignContent(t *testing.T) {
	for _, test := range []struct {
		name, source, upstream, want string
	}{
		{"foreignObject", `<p>before</p><svg><foreignObject><template>HID</template></foreignObject></svg><p>POST</p>`, "before\n\nPOST", "before\n\n" + omittedHTML},
		{"mathml text integration point", `<p>before</p><math><mi><template>x</template></mi></math><p>POST</p>`, "before\n\nPOST", "before\n\n" + omittedHTML},
		{"nothing parsed", `<svg><foreignObject><template>a</template></foreignObject></svg><p>POST</p>`, "POST", omittedHTML},
		{"html template", `<template>x</template><p>after</p>`, "after", "after"},
		{"svg title is not detected", `<p>before</p><svg><title><template>x</template></title></svg><p>POST</p>`, "before\n\nPOST", "before"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%q\nwant=%q\nupstream=%q", got, test.want, test.upstream)
			}
		})
	}
}

// The reference removal rule compares upper-case HTML node names, so it keeps
// script and style text inside SVG and MathML. The converter removes them in
// every namespace, which only drops code and style sheets from the output.
func TestRenderHTML_RemovesForeignScriptAndStyle(t *testing.T) {
	for _, test := range []struct {
		name, source, upstream, want string
	}{
		{"svg style", `<svg><style>css</style></svg><p>after</p>`, "css\n\nafter", "after"},
		{"svg script", `<svg><script>run()</script></svg><p>after</p>`, "run()\n\nafter", "after"},
		{"mathml style", `<math><style>m</style></math><p>after</p>`, "m\n\nafter", "after"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%q\nwant=%q\nupstream=%q", got, test.want, test.upstream)
			}
		})
	}
}

// amplifiedHTML opens formatting elements whose attributes all differ, so the
// Noah's Ark clause keeps every one of them in the active formatting list and
// the parser reconstructs all of them in each following paragraph. closer ends
// the opener without closing the formatting run.
func amplifiedHTML(opener, closer string, formatting, paragraphs int) string {
	var source strings.Builder
	source.WriteString(opener)
	for index := range formatting {
		source.WriteString(`<b x="` + strconv.Itoa(index) + `">`)
	}
	source.WriteString("x" + closer)
	source.WriteString(strings.Repeat("<p>x</p>", paragraphs))
	return source.String()
}

// Tree construction runs before hidden content is removed and before the output
// budget applies, so only a pre-parse bound keeps a small page from expanding
// into millions of nodes.
func TestRenderHTML_OmitsAmplifiedTreeConstruction(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{"unclosed formatting run", amplifiedHTML("<p><b hidden>", "", 499, 1000), omittedHTML},
		{"formatting run left open by its paragraph", amplifiedHTML("<p>", "</p>", 500, 1000), omittedHTML},
		{"scripting disabled keeps noscript markup", amplifiedHTML("<noscript>", "</noscript>", 500, 1000), omittedHTML},
		{"self-closing formatting elements stay open", strings.ReplaceAll(amplifiedHTML("<p>", "</p>", 500, 1000), `">`, `"/>`), omittedHTML},
		{"start tags beyond the budget", strings.Repeat("<p>x</p>", maxConversionCost+1), omittedHTML},
		{"start tags at the budget convert", strings.Repeat("<p>x</p>", maxConversionCost), strings.TrimSuffix(strings.Repeat("x\n\n", maxConversionCost), "\n\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%.80q… (%d bytes)\nwant=%.80q… (%d bytes)", got, len(got), test.want, len(test.want))
			}
		})
	}
}

// The budget bounds allocation, not elapsed time: the reviewer's 8 KB sample
// allocated about 80 MB building its 502,502-node tree before the bound existed.
func TestRenderHTML_BoundsAmplifiedAllocation(t *testing.T) {
	const maxBytes = 4 << 20
	source := amplifiedHTML("<p><b hidden>", "", 499, 1000)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	text := renderHTML(source)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > maxBytes {
		t.Fatalf("converting %d bytes allocated %d bytes, want at most %d", len(source), allocated, maxBytes)
	}
	if text != omittedHTML {
		t.Fatalf("got=%q want=%q", text, omittedHTML)
	}
}

// The scan models tree construction, not markup: comments and raw text hold no
// elements, and a matching end tag releases the reconstruction pressure.
func TestExceedsConversionCost_ScansLikeTreeConstruction(t *testing.T) {
	for _, test := range []struct {
		name, source string
		want         bool
	}{
		{"empty", "", false},
		{"comments hold no elements", strings.Repeat("<!-- <b x=1> -->", maxConversionCost), false},
		{"raw text holds no elements", "<style>" + strings.Repeat("<b x=1>", maxConversionCost) + "</style>", false},
		{"void elements never nest", strings.Repeat("<br>", maxConversionCost), false},
		{"closed formatting releases pressure", strings.Repeat("<b x=1>y</b>", 1000) + strings.Repeat("<p>x</p>", 1000), false},
		{"unclosed formatting multiplies paragraphs", amplifiedHTML("<p>", "</p>", 500, 1000), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := exceedsConversionCost(test.source); got != test.want {
				t.Fatalf("got=%v want=%v", got, test.want)
			}
		})
	}
}

// x/net/html rejects input whose open-element stack exceeds 512 elements; the
// fragment root counts as one, so 511 nested content elements still convert.
func TestRenderHTML_OmitsPathologicalNesting(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{"at limit", strings.Repeat("<span>", 511) + "x", "x"},
		{"beyond limit", strings.Repeat("<span>", 512) + "x", omittedHTML},
		{"self-closing html elements stay open", strings.Repeat("<div/>", 512) + "x", omittedHTML},
		{"implied paragraph", strings.Repeat("<span>", 511) + "</p>x", omittedHTML},
		{"foreign self-closing at limit", strings.Repeat("<span>", 509) + "<svg><g/>x", "x"},
		{"foreign self-closing beyond limit", strings.Repeat("<span>", 510) + "<svg><g/>x", omittedHTML},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderHTML(test.source); got != test.want {
				t.Fatalf("got=%q want=%q", got, test.want)
			}
		})
	}
}

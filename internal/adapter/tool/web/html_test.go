package web

import (
	"math/rand"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	appWeb "github.com/jinyule/nano-harness/internal/app/web"
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
// Noah's Ark clause keeps every one of them in the list of active formatting
// elements and the parser clones all of them, attributes included, at each
// following insertion point. closer ends the opener without closing the run.
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

// parsedWeight is what conversionCost bounds: the weight of the tree the
// production parser builds, where an element weighs one plus its attributes
// because a clone copies the whole attribute slice, and any other node weighs
// one. It returns false for input the parser rejects, which never reaches the
// renderer.
func parsedWeight(source string) (int, bool) {
	roots, err := html.ParseFragmentWithOptions(strings.NewReader(source), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}, html.ParseOptionEnableScripting(false))
	if err != nil {
		return 0, false
	}
	weight := 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			weight += 1 + len(node.Attr)
		} else {
			weight++
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return weight, true
}

// reviewVectors are the amplifications four review rounds found. Each stays a
// few KB and expands to hundreds of thousands of elements, or to hundreds of MB
// of copied attributes, unless the scan rejects it before the parse.
func reviewVectors() map[string]string {
	vectors := map[string]string{
		"unclosed formatting run":                    amplifiedHTML("<p><b hidden>", "", 499, 1000),
		"unclosed paragraphs":                        strings.ReplaceAll(amplifiedHTML("<p><b hidden>", "", 499, 1000), "</p>", ""),
		"formatting run left open by its paragraph":  amplifiedHTML("<p>", "</p>", 500, 1000),
		"scripting disabled keeps noscript markup":   amplifiedHTML("<noscript>", "</noscript>", 500, 1000),
		"self-closing formatting elements stay open": strings.ReplaceAll(amplifiedHTML("<p>", "</p>", 500, 1000), `">`, `"/>`),
		"end tags ignored inside a cell":             amplifiedHTML("<p>", "", 500, 0) + "<table><tr><td>" + strings.Repeat("</b>", 500) + "</td>" + strings.Repeat("<p>x</p>", 1000),
		"end tags ignored inside select":             amplifiedHTML("<p>", "", 500, 0) + "<select>" + strings.Repeat("</b>", 500) + "</select>" + strings.Repeat("<p>x</p>", 1000),
	}
	// A comment opened inside raw text is text to the parser, so a scan that
	// honoured comments would stop counting here.
	for _, wrapper := range []string{"script", "style", "title", "textarea"} {
		vectors["comment inside "+wrapper] = "<" + wrapper + "><!--</" + wrapper + ">" + amplifiedHTML("<p><b hidden>", "", 100, 1000)
	}
	vectors["comment inside script after a breakout"] = "<svg><p></p><script>var s='<!--';</script>" + amplifiedHTML("<p><b hidden>", "", 100, 1000)
	// Raw text in a foreign namespace is markup to the parser, so a scan that
	// honoured raw text would stop counting here.
	for _, wrapper := range []string{"title", "style", "script", "xmp", "iframe", "noembed", "noframes", "textarea", "plaintext"} {
		vectors["svg "+wrapper] = "<svg><" + wrapper + ">" + amplifiedHTML("<p><b hidden>", "", 100, 1000) + "</" + wrapper + "></svg>"
	}
	vectors["mathml title"] = "<math><title>" + amplifiedHTML("<p><b hidden>", "", 100, 1000) + "</title></math>"
	// The adoption agency stops after eight iterations and leaves its clone in
	// the list, so the element keeps being reconstructed. <a> start tags run it
	// too, and the clone outlives the pointer the parser removes.
	var limit, anchors strings.Builder
	for index := range 50 {
		limit.WriteString(`<b x="` + strconv.Itoa(index) + `">` + strings.Repeat("<div>", 9) + "</b>")
		anchors.WriteString(`<a href="/` + strconv.Itoa(index) + `">` + strings.Repeat("<div>", 9))
	}
	tail := strings.Repeat("</div>", 450) + strings.Repeat("<p>x</p>", 1000)
	vectors["adoption agency iteration limit"] = limit.String() + tail
	vectors["anchor clones outlive their pointer"] = anchors.String() + tail
	// Cloning copies every attribute, so few elements can still copy megabytes.
	var attributes strings.Builder
	attributes.WriteString("<p><b")
	for index := range 8000 {
		attributes.WriteString(" a" + strconv.Itoa(index))
	}
	attributes.WriteString(">x</p>" + strings.Repeat("<p>x</p>", 1000))
	vectors["attribute copies"] = attributes.String()
	// Each of these tags hides a "<" inside its value, so the scan cannot
	// compare them faithfully and each has to count as its own element.
	var cut strings.Builder
	cut.WriteString("<p>")
	for index := range 500 {
		cut.WriteString(`<b x="<` + strconv.Itoa(index) + `">`)
	}
	cut.WriteString("x</p>" + strings.Repeat("<p>x</p>", 1000))
	vectors["markup inside distinct attribute values"] = cut.String()
	return vectors
}

// ordinaryPages must keep converting: the scan is conservative, so its headroom
// on real markup is the evidence that it does not reject what it should keep.
func ordinaryPages() map[string]string {
	return map[string]string{
		"links":          "<div>" + strings.Repeat(`<a href="/x">link</a> `, 3000) + "</div>",
		"article":        "<article><h1>A</h1>" + strings.Repeat("<section><h2>H</h2><p>This is <strong>important</strong> content with <a href='/ref'>a reference</a>.</p></section>", 800) + "</article>",
		"documentation":  "<main>" + strings.Repeat("<section><h2>API</h2><p>Use <code>run()</code> to start.</p><pre><code class='language-go'>run(ctx)\n</code></pre><ul><li>First</li><li>Second</li></ul></section>", 600) + "</main>",
		"table":          "<table><thead><tr><th>N</th><th>V</th></tr></thead><tbody>" + strings.Repeat("<tr><td><a href='/item'>Item</a></td><td>42</td></tr>", 1800) + "</tbody></table>",
		"dense markup":   strings.Repeat("<p>x</p>", 25000),
		"wide text":      strings.Repeat("<p>\u8fd9\u662f\u4e00\u6bb5\u4e2d\u6587\u5185\u5bb9\uff0c\u7528\u4e8e\u68c0\u67e5\u6e32\u67d3\u4e0a\u9650\u3002</p>", 4000),
		"stray end tags": strings.Repeat("x</p>", 1000),
		"implied tables": strings.Repeat("<table><td>x</table>", 200),
		"quoted markup":  strings.Repeat(`<div title="<b x=1>">text</div>`, 1000),
	}
}

// Tree construction runs before hidden content is removed and before the output
// budget applies, so only a pre-parse bound keeps a few KB from expanding into
// millions of elements or hundreds of MB of copied attributes.
func TestRenderHTML_OmitsAmplifiedTreeConstruction(t *testing.T) {
	for name, source := range reviewVectors() {
		t.Run(name, func(t *testing.T) {
			if got := renderHTML(source); got != omittedHTML {
				t.Fatalf("%d bytes scored %d and converted to %.60q…", len(source), conversionCost(source), got)
			}
		})
	}
	for name, source := range ordinaryPages() {
		t.Run("ordinary/"+name, func(t *testing.T) {
			if cost := conversionCost(source); cost > maxConversionCost {
				t.Fatalf("%d bytes scored %d, over the %d budget", len(source), cost, maxConversionCost)
			}
		})
	}
	// Text and end tags cost one each with nothing open, so this is the exact
	// comparison boundary.
	t.Run("at the budget", func(t *testing.T) {
		source := strings.Repeat("<p>x", maxConversionCost/2)
		if cost := conversionCost(source); cost != maxConversionCost {
			t.Fatalf("got=%d want=%d", cost, maxConversionCost)
		}
		if got := renderHTML(source); got == omittedHTML {
			t.Fatal("input at the budget must convert")
		}
	})
	t.Run("one insertion point past the budget", func(t *testing.T) {
		source := strings.Repeat("<p>x", maxConversionCost/2) + "<p>"
		if got := renderHTML(source); got != omittedHTML {
			t.Fatalf("got=%.60q… want the omission marker", got)
		}
	})
}

// The budget bounds allocation, not elapsed time: the reviewer's 8 KB sample
// allocated about 80 MB building its 502,502-node tree before the bound existed,
// and its visible variant about 18.9 GiB through the renderer.
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

// Nested visible formatting makes every level copy the level below it, so the
// renderer stops at the output budget instead of formatting everything and
// truncating afterwards.
func TestRenderHTML_StopsAtTheOutputBudget(t *testing.T) {
	for _, test := range []struct{ name, source string }{
		{"ascii", "<p>" + strings.Repeat("x", 4*maxRenderBytes) + "</p>"},
		{"closing markup after the budget", "<p><b>" + strings.Repeat("x", 4*maxRenderBytes) + "</b></p>"},
		{"a cut between bytes of one rune", "<p>x" + strings.Repeat("\u4e2d", maxRenderBytes) + "</p>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := renderHTML(test.source)
			if len(text) > maxRenderBytes+len(omittedHTML)+2 {
				t.Fatalf("rendered %d bytes, want at most %d", len(text), maxRenderBytes)
			}
			if !strings.HasSuffix(text, omittedHTML) {
				t.Fatalf("got=%.40q…, want the omission marker at the end", text)
			}
			if !utf8.ValidString(text) {
				t.Fatal("rendered text is not valid UTF-8")
			}
		})
	}
	// Once the budget is spent the walk stops: later siblings are not rendered
	// and nothing more is written.
	t.Run("drops what follows", func(t *testing.T) {
		source := "<p>" + strings.Repeat("x", 2*maxRenderBytes) + "</p>" + strings.Repeat("<p>tail</p>", 3)
		text := renderHTML(source)
		if strings.Contains(text, "tail") {
			t.Fatal("rendering continued past the output budget")
		}
		if !strings.HasSuffix(text, omittedHTML) {
			t.Fatalf("got=%.40q…, want the omission marker at the end", text)
		}
	})

	// The budget is counted in bytes, so the widest text still fills the whole
	// UTF-16 output budget.
	t.Run("keeps the whole output budget", func(t *testing.T) {
		source := "<p>" + strings.Repeat("\u4e2d", maxFetchOutputUnits) + "</p>"
		text, truncated := formatFetch(appWeb.FetchResult{URL: "https://example.test/", StatusCode: 200, Kind: appWeb.FetchHTML, Content: source}, maxFetchOutputUnits)
		if !truncated {
			t.Fatal("want truncated")
		}
		if units := len(utf16.Encode([]rune(text))); units < maxFetchOutputUnits-len(fetchFooter) {
			t.Fatalf("delivered %d units, want the output budget", units)
		}
	})
}

// The scan has to over-approximate the weight the parser creates for every
// input, in every context. The fixed samples are the amplifications four
// review rounds found; the generated ones mix formatting elements, their
// attributes and the contexts that change how their tags are read.
func TestConversionCost_BoundsParsedWeight(t *testing.T) {
	assert := func(t *testing.T, source string) {
		t.Helper()
		cost := conversionCost(source)
		if cost > maxConversionCost {
			// Rejected input is never parsed, so there is no tree to bound.
			return
		}
		weight, parsed := parsedWeight(source)
		if parsed && weight > cost {
			t.Fatalf("%.120q…: parsed weight %d, scored %d", source, weight, cost)
		}
	}
	for name, source := range reviewVectors() {
		t.Run(name, func(t *testing.T) { assert(t, source) })
	}
	for name, source := range ordinaryPages() {
		t.Run("ordinary/"+name, func(t *testing.T) { assert(t, source) })
	}
	for name, source := range map[string]string{
		"quoted greater-than in a signature": "<p>" + strings.Repeat(`<b x=">" y=k>`, 500) + "x</p>" + strings.Repeat("<p>x</p>", 1000),
		"unterminated quote":                 `<p><b x='` + strings.Repeat("<p>x</p>", 100),
		"unterminated quote inside raw text": `<xmp><b x='</xmp><em><b x="1">` + strings.Repeat("<p>x</p>", 100),
		"markup inside an attribute value":   `<div title="<b x=1>">` + strings.Repeat("<p>x</p>", 100),
		"bare attribute names":               "<b a b c d e>x",
		"solidus between attributes":         "<b x='v'</div>x",
		"unterminated tag":                   "<b x",
		"not a tag":                          "<3 < <!",
	} {
		t.Run(name, func(t *testing.T) { assert(t, source) })
	}
	pieces := []string{
		`<b x="1">`, `<b x="2">`, `<b x=">" y=k>`, "<b a b c d e>", `<b x='`, "</b>", `<i x="1">`, "</i>",
		`<a href="/1">`, `<a href="/2">`, "</a>", "<nobr>", "</nobr>", `<font color="">`, "</font>", "<em>", "</em>",
		"<code>", "</code>", "<u>", "</u>", "<s>", "</s>", "<p>", "</p>", "</br>", "<div>", "</div>",
		"x", " ", "y", "<!-- c -->", "<!--", "-->", "<br>", "<hr>", "<img src=x>", `<div title="<b x=1>">`,
		"<table>", "</table>", "<tr>", "<td>", "</td>", "<th>", "</th>", "<caption>", "</caption>", "<tbody>", "<col>",
		"<template>", "</template>", "<object>", "</object>", "<applet>", "</applet>", "<marquee>", "</marquee>",
		"<select>", "</select>", "<option>", "<svg>", "</svg>", "<math>", "</math>", "<title>", "</title>",
		"<style>", "</style>", "<script>", "</script>", "<textarea>", "</textarea>", "<noscript>", "</noscript>",
		"<plaintext>", "<xmp>", "</xmp>", "<iframe>", "</iframe>", "<noembed>", "<foreignObject>", "</foreignObject>",
		"<mi>", "</mi>", `<annotation-xml encoding="text/html">`, "<![CDATA[z]]>", "<li>", "<ul>", "</ul>",
		"<h1>", "</h1>", "<b/>", "<svg/>", "<", "<!", "</>", "<3",
	}
	// Samples only need to be varied and reproducible, not unpredictable.
	random := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test input, not a security decision
	for sample := range 4000 {
		var source strings.Builder
		for range 1 + random.Intn(60) {
			source.WriteString(pieces[random.Intn(len(pieces))])
		}
		t.Run("generated/"+strconv.Itoa(sample), func(t *testing.T) { assert(t, source.String()) })
	}
}

// The model charges each insertion point the weight the parser may clone there,
// each element its own attributes, and each run of the adoption agency its
// clones. Raw text, comments and quoting never hide markup from it.
func TestConversionCost_ChargesWeightedFormatting(t *testing.T) {
	for _, test := range []struct {
		name, source string
		want         int
	}{
		{"empty", "", 0},
		{"text alone", "x", 1},
		{"an element weighs one plus its attributes", `<b x="1" y="2">`, 3},
		{"paragraphs cost one each", strings.Repeat("<p>x</p>", 10), 30},
		{"a cell implies tbody and tr", "<td>", 3},
		{"an open element is charged at every later point", `<b x="1">` + strings.Repeat("<p>x</p>", 10), 92},
		{"a closed element stops being charged", `<b x="1">y</b>` + strings.Repeat("<p>x</p>", 10), 38},
		{"Noah's Ark caps identical elements", strings.Repeat(`<b x="1">`, 10) + "y", 75},
		{"raw text is scanned", "<style>" + strings.Repeat(`<b x="1">`, 3) + "</style>y", 27},
		{"an end tag off the stack top runs the adoption agency", `<b x="1"><p></b>`, 104},
		{"a repeated anchor runs it too", `<a href="/1"><a href="/2">`, 102},
		{"stray end tags never go negative", strings.Repeat("</b>", 10) + "x", 331},
	} {
		t.Run(test.name, func(t *testing.T) {
			if cost := conversionCost(test.source); cost != test.want {
				t.Fatalf("got=%d want=%d", cost, test.want)
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

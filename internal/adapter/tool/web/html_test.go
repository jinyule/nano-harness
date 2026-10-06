package web

import (
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
		{"orphan cells", "<td>loose</td><tr><td>row</td></tr>", "| row |"},
		{"hidden", `<p>shown</p><div hidden>no</div><span aria-hidden="TRUE">no</span><span aria-hidden="false">yes</span><p style="color:red; display : none !important">no</p><p style="visibility:collapse">no</p><p style="visibility:hidden">no</p><p style="display">kept</p><input type="hidden" value="x"><div hidden><img src="/x.png"><br></div>`, "shown\n\nyes\n\nkept"},
		{"removed", "<script>alert('x')</script><style>p{}</style><noscript>n</noscript><template><p>t</p></template><iframe src=x>f</iframe><object>o</object><embed src=x><p>visible</p>", "visible"},
		{"self closing", `<p/>text<a href="x"/><br/><img src="/i.png"/>`, "text  \n![](/i.png)"},
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

func TestRenderHTML_OmitsPathologicalNesting(t *testing.T) {
	deep := strings.Repeat("<div>", maxHTMLDepth+1) + "x"
	if got := renderHTML(deep); got != omittedHTML {
		t.Fatalf("deep nesting=%q", got)
	}
	limit := strings.Repeat("<span>", maxHTMLDepth) + "x"
	if got := renderHTML(limit); got != "x" {
		t.Fatalf("nesting at limit=%q", got)
	}
	selfClosing := strings.Repeat("<span>", maxHTMLDepth) + "<div/>x"
	if got := renderHTML(selfClosing); got != omittedHTML {
		t.Fatalf("self-closing beyond limit=%q", got)
	}
}

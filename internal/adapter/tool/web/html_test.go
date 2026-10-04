package web

import (
	"strings"
	"testing"
)

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
		{"breaks", "<p>a<br>b</p><hr><pre>x<br>y</pre>", "a\nb\n\n---\n\n```\nx\ny\n```"},
		{"lists", `<ul><li>one</li><li><p>two</p><ol start="3"><li>three</li><li>four</li></ol></li></ul><ol><li>first</li></ol><li>orphan</li>`, "- one\n- two\n\n  3. three\n  4. four\n\n1. first\n\n- orphan"},
		{"empty item", "<ul><li></li><li>b</li></ul>", "-\n- b"},
		{"quote", "<blockquote><p>quoted</p><p>more</p></blockquote>after", "> quoted\n>\n> more\n\nafter"},
		{"table", "<table><thead><tr><th>A|B</th><th>C</th></tr></thead><tbody><tr><td>1</td><td><b>2</b></td></tr><tr></tr></tbody></table>", "| A\\|B | C |\n| --- | --- |\n| 1 | **2** |"},
		{"table without header", "<table><tr><td>x</td><th>y</th></tr></table>", "| x | y |"},
		{"orphan cells", "<td>loose</td><tr><td>row</td></tr>", "| row |"},
		{"hidden", `<p>shown</p><div hidden>no</div><span aria-hidden="TRUE">no</span><span aria-hidden="false">yes</span><p style="color:red; display : none !important">no</p><p style="visibility:collapse">no</p><p style="visibility:hidden">no</p><p style="display">kept</p><input type="hidden" value="x"><div hidden><img src="/x.png"><br></div>`, "shown\n\nyes\n\nkept"},
		{"removed", "<script>alert('x')</script><style>p{}</style><noscript>n</noscript><template><p>t</p></template><iframe src=x>f</iframe><object>o</object><embed src=x><p>visible</p>", "visible"},
		{"self closing", `<p/>text<a href="x"/><br/><img src="/i.png"/>`, "text\n![](/i.png)"},
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

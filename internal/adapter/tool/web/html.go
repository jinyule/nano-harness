package web

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

const (
	// maxHTMLDepth bounds the open-element stack. Real pages nest a few dozen
	// levels; deeper input is omitted instead of converted.
	maxHTMLDepth = 512
	omittedHTML  = "[HTML content omitted: unable to convert safely.]"
)

// Elements removed with their content, matching the reference converter.
var removedElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "iframe": true, "object": true, "embed": true,
}

// Elements that never take a closing tag and so never enter the stack.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "body": true, "center": true, "dd": true, "details": true,
	"div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "header": true, "html": true, "main": true, "nav": true, "p": true, "section": true,
	"summary": true, "title": true,
}

type frameKind int

const (
	frameNone frameKind = iota
	frameBlock
	frameHeading
	frameLink
	frameStrong
	frameEmphasis
	frameStrike
	frameCode
	frameQuote
	frameList
	frameListItem
	framePre
	frameTable
	frameRow
	frameCell
)

type frame struct {
	tag      string
	kind     frameKind
	hidden   bool
	href     string
	language string
	// namespace is "svg" or "math" for foreign elements and empty for HTML.
	namespace string
	// integration marks an SVG or MathML element whose children follow HTML rules.
	integration bool
}

// HTML start tags that end SVG or MathML content (the HTML standard's rules
// for "any other start tag" in foreign content); font breaks out only with a
// color, face, or size attribute.
var foreignBreakout = map[string]bool{
	"b": true, "big": true, "blockquote": true, "body": true, "br": true, "center": true, "code": true, "dd": true,
	"div": true, "dl": true, "dt": true, "em": true, "embed": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "head": true, "hr": true, "i": true, "img": true, "li": true, "listing": true, "menu": true,
	"meta": true, "nobr": true, "ol": true, "p": true, "pre": true, "ruby": true, "s": true, "small": true, "span": true,
	"strong": true, "strike": true, "sub": true, "sup": true, "table": true, "tt": true, "u": true, "ul": true, "var": true,
}

type list struct {
	ordered bool
	next    int
}

type row struct {
	cells  []string
	header bool
}

// buffer is one capture level; last remembers the final written byte.
type buffer struct {
	text bytes.Buffer
	last byte
}

func (current *buffer) trimLineSpace() {
	content := current.text.Bytes()
	end := len(content)
	for end > 0 && (content[end-1] == ' ' || content[end-1] == '\t') {
		end--
	}
	current.text.Truncate(end)
	current.last = 0
	if end > 0 {
		current.last = content[end-1]
	}
}

func (current *buffer) write(value string) {
	if value != "" {
		current.text.WriteString(value)
		current.last = value[len(value)-1]
	}
}

// renderer converts a token stream to Markdown-like text in one linear pass.
type renderer struct {
	captures []*buffer
	stack    []frame
	lists    []list
	tables   []int
	rows     []*row
	hidden   int
	pre      int
	code     int
	newlines int
	space    bool
	marker   bool
}

// renderHTML converts decoded HTML to model-facing Markdown. It removes
// non-visible content and returns a fixed omission marker for pathological nesting.
func renderHTML(source string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	state := &renderer{captures: []*buffer{{}}}
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			// The source is in memory and unbounded by the tokenizer, so the
			// only error is the end of input; close what remains open.
			for len(state.stack) > 0 {
				state.pop()
			}
			return strings.TrimSpace(state.captures[0].text.String())
		case html.TextToken:
			state.text(string(tokenizer.Text()))
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			namespace := state.namespaceFor(token)
			switch {
			case namespace != "":
				// Foreign elements have no raw text, and their self-closing
				// slash closes them.
				tokenizer.NextIsNotRawText()
				if !state.open(token, namespace) {
					return omittedHTML
				}
				if token.Type == html.SelfClosingTagToken {
					state.pop()
				}
			case voidElements[token.Data]:
				state.void(token)
			case !state.open(token, ""):
				// An HTML element's self-closing slash is ignored: the element
				// stays open, and raw-text elements read raw text to their end tag.
				return omittedHTML
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			state.close(string(name))
		case html.CommentToken, html.DoctypeToken:
		}
	}
}

func (state *renderer) current() *buffer { return state.captures[len(state.captures)-1] }

// inForeign reports whether the current element is SVG or MathML content
// that is not an HTML integration point.
func (state *renderer) inForeign() bool {
	if len(state.stack) == 0 {
		return false
	}
	top := state.stack[len(state.stack)-1]
	return top.namespace != "" && !top.integration
}

// namespaceFor returns the namespace of the element a start tag creates. A
// breakout tag first closes the foreign elements above the nearest HTML element
// or integration point, and is then an HTML element.
func (state *renderer) namespaceFor(token html.Token) string {
	if state.inForeign() && (foreignBreakout[token.Data] || token.Data == "font" && (attribute(token, "color") != "" || attribute(token, "face") != "" || attribute(token, "size") != "")) {
		for state.inForeign() {
			state.pop()
		}
	}
	if state.inForeign() {
		return state.stack[len(state.stack)-1].namespace
	}
	switch token.Data {
	case "svg", "math":
		return token.Data
	default:
		return ""
	}
}

// integrationPoint reports whether children of a foreign element follow HTML rules.
func integrationPoint(namespace string, token html.Token) bool {
	switch {
	case namespace == "svg":
		return token.Data == "foreignobject" || token.Data == "desc" || token.Data == "title"
	case token.Data == "annotation-xml":
		encoding := strings.ToLower(attribute(token, "encoding"))
		return encoding == "text/html" || encoding == "application/xhtml+xml"
	default:
		return token.Data == "mi" || token.Data == "mo" || token.Data == "mn" || token.Data == "ms" || token.Data == "mtext"
	}
}

// flush writes pending separators before visible output. A buffer never starts
// with a separator, and list markers absorb the separator that follows them.
func (state *renderer) flush() {
	current := state.current()
	switch {
	case current.text.Len() == 0 || state.marker:
	case state.newlines > 0:
		if state.pre == 0 {
			current.trimLineSpace()
		}
		current.write(strings.Repeat("\n", state.newlines))
	case state.space && current.last != ' ' && current.last != '\n':
		current.write(" ")
	}
	state.newlines, state.space, state.marker = 0, false, false
}

func (state *renderer) emit(value string) {
	state.flush()
	state.current().write(value)
}

func (state *renderer) breakLines(count int) {
	if !state.marker {
		state.newlines, state.space = max(state.newlines, count), false
	}
}

func (state *renderer) text(value string) {
	if state.hidden > 0 {
		return
	}
	if state.pre > 0 {
		state.emit(value)
		return
	}
	if state.code == 0 {
		value = escapeMarkdown(value)
	}
	start := -1
	for index, char := range value {
		if strings.ContainsRune(" \t\n\r\f", char) {
			if start >= 0 {
				state.emit(value[start:index])
				start = -1
			}
			state.space = true
			continue
		}
		if start < 0 {
			start = index
		}
	}
	if start >= 0 {
		state.emit(value[start:])
	}
}

func hiddenElement(token html.Token) bool {
	for _, attribute := range token.Attr {
		value := strings.ToLower(strings.TrimSpace(attribute.Val))
		switch attribute.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if value == "true" {
				return true
			}
		case "style":
			for declaration := range strings.SplitSeq(value, ";") {
				property, setting, ok := strings.Cut(declaration, ":")
				setting = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(setting), "!important"))
				property = strings.TrimSpace(property)
				if ok && (property == "display" && setting == "none" || property == "visibility" && (setting == "hidden" || setting == "collapse")) {
					return true
				}
			}
		}
	}
	return false
}

func attribute(token html.Token, key string) string {
	for _, candidate := range token.Attr {
		if candidate.Key == key {
			return strings.TrimSpace(candidate.Val)
		}
	}
	return ""
}

func (state *renderer) void(token html.Token) {
	if token.Data == "hr" {
		state.closeInScope([]string{"p"}, paragraphScope)
	}
	if state.hidden > 0 || hiddenElement(token) {
		return
	}
	switch token.Data {
	case "br":
		if state.pre > 0 {
			state.emit("\n")
		} else {
			state.flush()
			state.current().trimLineSpace()
			state.current().write("  \n")
		}
	case "hr":
		state.breakLines(2)
		state.emit("---")
		state.breakLines(2)
	case "img":
		alt, source := attribute(token, "alt"), attribute(token, "src")
		if source != "" && !strings.HasPrefix(strings.ToLower(source), "data:") {
			state.emit("![" + escapeMarkdown(collapse(alt)) + "](" + escapeDestination(source) + ")")
		} else if alt != "" {
			state.emit(escapeMarkdown(collapse(alt)))
		}
	case "input":
		if len(state.stack) > 0 && state.stack[len(state.stack)-1].tag == "li" && strings.EqualFold(attribute(token, "type"), "checkbox") {
			marker := "[ ] "
			for _, attr := range token.Attr {
				if attr.Key == "checked" {
					marker = "[x] "
				}
			}
			state.emit(marker)
		}
	}
}

// open pushes one element in namespace and applies its opening effect. It
// reports false when the nesting limit is exceeded. Foreign elements only
// carry visibility; their text renders as plain inline content.
func (state *renderer) open(token html.Token, namespace string) bool {
	if namespace == "" {
		state.closeImplied(token.Data)
	}
	if len(state.stack) >= maxHTMLDepth {
		return false
	}
	current := frame{tag: token.Data, namespace: namespace}
	if namespace != "" {
		current.integration = integrationPoint(namespace, token)
	}
	if state.hidden > 0 || removedElements[token.Data] || hiddenElement(token) {
		current.hidden = true
		state.hidden++
		state.stack = append(state.stack, current)
		return true
	}
	if namespace != "" {
		state.stack = append(state.stack, current)
		return true
	}
	switch token.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		current.kind = frameHeading
		state.breakLines(2)
		state.capture()
	case "a":
		current.kind, current.href = frameLink, attribute(token, "href")
		state.capture()
	case "strong", "b":
		current.kind = frameStrong
		state.capture()
	case "em", "i":
		current.kind = frameEmphasis
		state.capture()
	case "del", "s", "strike":
		current.kind = frameStrike
		state.capture()
	case "code", "kbd", "samp":
		if state.pre == 0 {
			current.kind = frameCode
			state.code++
			state.capture()
		} else if token.Data == "code" {
			if match := codeLanguage.FindStringSubmatch(attribute(token, "class")); len(match) > 1 {
				state.stack[len(state.stack)-1].language = match[1]
			}
		}
	case "blockquote":
		current.kind = frameQuote
		state.breakLines(2)
		state.capture()
	case "ul", "ol":
		current.kind = frameList
		state.breakLines(2 - min(len(state.lists), 1))
		start, _ := strconv.Atoi(attribute(token, "start"))
		state.lists = append(state.lists, list{ordered: token.Data == "ol", next: max(start, 1)})
	case "li":
		current.kind = frameListItem
		state.listMarker()
	case "pre":
		current.kind = framePre
		state.breakLines(2)
		state.capture()
		state.pre++
	case "table":
		current.kind = frameTable
		state.breakLines(2)
		state.tables = append(state.tables, 0)
	case "tr":
		current.kind = frameRow
		state.rows = append(state.rows, &row{header: true})
	case "th", "td":
		current.kind = frameCell
		if len(state.rows) == 0 {
			state.rows = append(state.rows, &row{header: true})
		}
		state.rows[len(state.rows)-1].header = state.rows[len(state.rows)-1].header && token.Data == "th"
		state.capture()
	default:
		if blockElements[token.Data] {
			current.kind = frameBlock
			state.breakLines(2)
		}
	}
	state.stack = append(state.stack, current)
	return true
}

// HTML start tags close optional end tags before visibility is inherited.
// Scope boundaries keep a nested list, table or template from closing an
// ancestor's item or paragraph.
func (state *renderer) closeImplied(tag string) {
	switch tag {
	case "address", "article", "aside", "blockquote", "center", "details", "dialog", "dir", "div", "dl",
		"fieldset", "figcaption", "figure", "footer", "form", "header", "hgroup", "main", "menu", "nav",
		"ol", "p", "search", "section", "summary", "ul", "h1", "h2", "h3", "h4", "h5", "h6",
		"li", "dt", "dd", "pre", "listing", "table":
		state.closeInScope([]string{"p"}, paragraphScope)
	}
	switch tag {
	case "li":
		state.closeInScope([]string{"li"}, []string{"ul", "ol", "template"})
	case "dt", "dd":
		state.closeInScope([]string{"dt", "dd"}, []string{"dl", "template"})
	case "h1", "h2", "h3", "h4", "h5", "h6":
		state.closeInScope([]string{"h1", "h2", "h3", "h4", "h5", "h6"}, paragraphScope)
	case "tr":
		state.closeInScope([]string{"tr"}, []string{"table", "template"})
	case "td", "th":
		state.closeInScope([]string{"td", "th"}, []string{"tr", "table", "template"})
	case "thead", "tbody", "tfoot":
		state.closeInScope([]string{"thead", "tbody", "tfoot"}, []string{"table", "template"})
	case "option", "optgroup":
		state.closeInScope([]string{"option"}, []string{"select", "datalist", "template"})
		if tag == "optgroup" {
			state.closeInScope([]string{"optgroup"}, []string{"select", "template"})
		}
	}
}

var paragraphScope = []string{"applet", "button", "caption", "html", "table", "td", "th", "marquee", "object", "template"}

func (state *renderer) closeInScope(tags, boundaries []string) {
	for index, current := range slices.Backward(state.stack) {
		if slices.Contains(tags, current.tag) {
			for len(state.stack) > index {
				state.pop()
			}
			return
		}
		if slices.Contains(boundaries, current.tag) {
			return
		}
	}
}

func (state *renderer) capture() {
	state.flush()
	state.captures = append(state.captures, &buffer{})
}

func (state *renderer) release() string {
	captured := state.current().text.String()
	state.captures = state.captures[:len(state.captures)-1]
	return captured
}

func (state *renderer) listMarker() {
	// An empty preceding item must not absorb this item's line break.
	state.marker = false
	state.breakLines(1)
	if len(state.lists) == 0 {
		state.emit("- ")
	} else {
		current := &state.lists[len(state.lists)-1]
		marker := "- "
		if current.ordered {
			marker = strconv.Itoa(current.next) + ". "
			current.next++
		}
		state.emit(strings.Repeat("  ", len(state.lists)-1) + marker)
	}
	state.marker = true
}

// close pops through the nearest matching open element; an end tag with no
// matching element is ignored, as browsers do.
func (state *renderer) close(name string) {
	for index, open := range slices.Backward(state.stack) {
		if open.tag == name {
			for len(state.stack) > index {
				state.pop()
			}
			return
		}
	}
}

func (state *renderer) pop() {
	current := state.stack[len(state.stack)-1]
	state.stack = state.stack[:len(state.stack)-1]
	if current.hidden {
		state.hidden--
		return
	}
	switch current.kind {
	case frameHeading:
		level, _ := strconv.Atoi(current.tag[1:])
		state.wrap(strings.Repeat("#", level)+" ", "")
		state.breakLines(2)
	case frameLink:
		content := collapse(state.release())
		switch {
		case content == "":
		case current.href == "" || strings.HasPrefix(strings.ToLower(current.href), "javascript:"):
			state.emit(content)
		default:
			state.emit("[" + content + "](" + escapeDestination(current.href) + ")")
		}
	case frameStrong:
		state.wrap("**", "**")
	case frameEmphasis:
		state.wrap("_", "_")
	case frameStrike:
		state.wrap("~~", "~~")
	case frameCode:
		state.code--
		content := collapse(state.release())
		if content != "" {
			fence := codeFence(content, 1)
			padding := ""
			if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
				padding = " "
			}
			state.emit(fence + padding + content + padding + fence)
		}
	case frameQuote:
		lines := strings.Split(strings.TrimSpace(state.release()), "\n")
		for index, line := range lines {
			lines[index] = ">"
			if line != "" {
				lines[index] += " " + line
			}
		}
		state.emit(strings.Join(lines, "\n"))
		state.breakLines(2)
	case frameList:
		state.lists = state.lists[:len(state.lists)-1]
		state.breakLines(2 - min(len(state.lists), 1))
	case framePre:
		state.pre--
		content := strings.TrimSuffix(state.release(), "\n")
		fence := codeFence(content, 3)
		state.emit(fence + current.language + "\n" + content + "\n" + fence)
		state.breakLines(2)
	case frameTable:
		state.tables = state.tables[:len(state.tables)-1]
		state.breakLines(2)
	case frameRow:
		state.endRow()
	case frameCell:
		content := strings.ReplaceAll(collapse(state.release()), "|", `\|`)
		current := state.rows[len(state.rows)-1]
		current.cells = append(current.cells, content)
	case frameBlock:
		state.breakLines(2)
	case frameNone, frameListItem:
	}
}

// wrap replaces the current capture with its collapsed content between markers;
// empty content produces nothing.
func (state *renderer) wrap(prefix, suffix string) {
	if content := collapse(state.release()); content != "" {
		state.emit(prefix + content + suffix)
	}
}

// endRow emits one GFM table row and, for a leading all-header row, its separator.
func (state *renderer) endRow() {
	current := state.rows[len(state.rows)-1]
	state.rows = state.rows[:len(state.rows)-1]
	if len(current.cells) == 0 {
		return
	}
	state.breakLines(1)
	state.emit("| " + strings.Join(current.cells, " | ") + " |")
	if len(state.tables) == 0 {
		return
	}
	index := len(state.tables) - 1
	if state.tables[index] == 0 && current.header {
		state.breakLines(1)
		state.emit("|" + strings.Repeat(" --- |", len(current.cells)))
	}
	state.tables[index]++
}

func collapse(value string) string { return strings.Join(strings.Fields(value), " ") }

var (
	codeLanguage        = regexp.MustCompile(`\blanguage-(\S+)`)
	backticks           = regexp.MustCompile("`+")
	markdownLiteral     = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`)
	markdownLineStart   = regexp.MustCompile(`(?m)^(#{1,6} |[-+] |>|-{3,}|=+|~~~)`)
	markdownNumber      = regexp.MustCompile(`(?m)^(\s*\d+)\. `)
	markdownDestination = strings.NewReplacer("(", `\(`, ")", `\)`, "<", `\<`, ">", `\>`)
)

func escapeMarkdown(text string) string {
	text = markdownLiteral.Replace(text)
	text = markdownLineStart.ReplaceAllString(text, `\$1`)
	return markdownNumber.ReplaceAllString(text, `$1\. `)
}

func escapeDestination(destination string) string {
	text := markdownDestination.Replace(destination)
	if strings.Contains(text, " ") {
		return "<" + text + ">"
	}
	return text
}

func codeFence(content string, minimum int) string {
	for _, run := range backticks.FindAllString(content, -1) {
		minimum = max(minimum, len(run)+1)
	}
	return strings.Repeat("`", minimum)
}

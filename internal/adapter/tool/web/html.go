package web

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// omittedHTML replaces HTML the parser refuses: x/net/html rejects input whose
// open-element stack exceeds 512 elements, which bounds tree construction.
const omittedHTML = "[HTML content omitted: unable to convert safely.]"

// Elements removed with their content, matching the reference converter.
var removedElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "iframe": true, "object": true, "embed": true,
}

// HTML elements that never have children.
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

// renderHTML converts decoded HTML to model-facing Markdown. The x/net/html
// parser builds the tree with full HTML tree construction (implied end tags,
// formatting-element reconstruction and adoption, foreign content), in the
// reference converter's body context and with scripting disabled as in its
// parser; the renderer then removes non-visible content while walking it.
func renderHTML(source string) string {
	// The reader is in memory, so the only parse error is the depth limit.
	nodes, err := html.ParseFragmentWithOptions(strings.NewReader(source), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}, html.ParseOptionEnableScripting(false))
	if err != nil {
		return omittedHTML
	}
	state := &renderer{captures: []*buffer{{}}}
	parsed := 0
	for _, node := range nodes {
		state.walk(node)
		parsed += countTemplates(node)
	}
	text := strings.TrimSpace(state.captures[0].text.String())
	if templateStartTags(source) > parsed {
		// x/net/html ignores the rest of the input at a template start tag
		// processed while SVG or MathML is open; mark what was dropped.
		return strings.TrimSpace(text + "\n\n" + omittedHTML)
	}
	return text
}

// walk renders one parsed node and its subtree.
func (state *renderer) walk(node *html.Node) {
	switch node.Type {
	case html.TextNode:
		state.text(node.Data)
	case html.ElementNode:
		token := html.Token{Type: html.StartTagToken, Data: node.Data, Attr: node.Attr}
		if node.Namespace == "" && voidElements[node.Data] {
			state.void(token)
			return
		}
		state.open(token, node.Namespace)
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			state.walk(child)
		}
		state.pop()
	case html.ErrorNode, html.DocumentNode, html.CommentNode, html.DoctypeNode, html.RawNode:
	}
}

// countTemplates counts template elements, in any namespace, in a subtree.
func countTemplates(node *html.Node) int {
	count := 0
	if node.Type == html.ElementNode && node.Data == "template" {
		count++
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		count += countTemplates(child)
	}
	return count
}

// templateStartTags counts template start tags outside comments and raw text.
// The plain tokenizer reads SVG title and style as raw text, so a template
// there is not counted and its dropped remainder is not marked.
func templateStartTags(source string) int {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	count := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return count
		case html.StartTagToken, html.SelfClosingTagToken:
			if name, _ := tokenizer.TagName(); string(name) == "template" {
				count++
			}
		case html.TextToken, html.EndTagToken, html.CommentToken, html.DoctypeToken:
		}
	}
}

func (state *renderer) current() *buffer { return state.captures[len(state.captures)-1] }

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

// open pushes one element in namespace and applies its opening effect. Foreign
// elements only carry visibility; their text renders as plain inline content.
func (state *renderer) open(token html.Token, namespace string) {
	current := frame{tag: token.Data, namespace: namespace}
	if state.hidden > 0 || removedElements[token.Data] || hiddenElement(token) {
		current.hidden = true
		state.hidden++
		state.stack = append(state.stack, current)
		return
	}
	if namespace != "" {
		state.stack = append(state.stack, current)
		return
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
		// The parser places every visible cell in a row of a table.
		current.kind = frameCell
		state.rows[len(state.rows)-1].header = state.rows[len(state.rows)-1].header && token.Data == "th"
		state.capture()
	default:
		if blockElements[token.Data] {
			current.kind = frameBlock
			state.breakLines(2)
		}
	}
	state.stack = append(state.stack, current)
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

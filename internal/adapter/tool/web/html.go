package web

import (
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

var (
	trailingSpace    = regexp.MustCompile(`[ \t]+\n`)
	excessBlankLines = regexp.MustCompile(`\n{3,}`)
)

type frameKind int

const (
	frameNone frameKind = iota
	frameBlock
	frameHeading
	frameLink
	frameStrong
	frameEmphasis
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
	tag    string
	kind   frameKind
	hidden bool
	href   string
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
	text strings.Builder
	last byte
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
			text := trailingSpace.ReplaceAllString(state.captures[0].text.String(), "\n")
			return strings.TrimSpace(excessBlankLines.ReplaceAllString(text, "\n\n"))
		case html.TextToken:
			state.text(string(tokenizer.Text()))
		case html.StartTagToken:
			token := tokenizer.Token()
			if voidElements[token.Data] {
				state.void(token)
			} else if !state.open(token) {
				return omittedHTML
			}
		case html.SelfClosingTagToken:
			token := tokenizer.Token()
			switch {
			case voidElements[token.Data]:
				state.void(token)
			case !state.open(token):
				return omittedHTML
			default:
				state.close(token.Data)
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			state.close(string(name))
		case html.CommentToken, html.DoctypeToken:
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
			state.breakLines(1)
		}
	case "hr":
		state.breakLines(2)
		state.emit("---")
		state.breakLines(2)
	case "img":
		alt, source := attribute(token, "alt"), attribute(token, "src")
		if source != "" && !strings.HasPrefix(strings.ToLower(source), "data:") {
			state.emit("![" + collapse(alt) + "](" + source + ")")
		} else if alt != "" {
			state.emit(collapse(alt))
		}
	}
}

// open pushes one element and applies its opening effect. It reports false when
// the nesting limit is exceeded.
func (state *renderer) open(token html.Token) bool {
	if len(state.stack) >= maxHTMLDepth {
		return false
	}
	current := frame{tag: token.Data}
	if state.hidden > 0 || removedElements[token.Data] || hiddenElement(token) {
		current.hidden = true
		state.hidden++
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
	case "code", "kbd", "samp":
		if state.pre == 0 {
			current.kind = frameCode
			state.capture()
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
		state.emit("```\n")
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
			state.emit("[" + content + "](" + current.href + ")")
		}
	case frameStrong:
		state.wrap("**", "**")
	case frameEmphasis:
		state.wrap("_", "_")
	case frameCode:
		state.wrap("`", "`")
	case frameQuote:
		lines := strings.Split(strings.TrimSpace(state.release()), "\n")
		for index, line := range lines {
			lines[index] = strings.TrimRight("> "+line, " ")
		}
		state.emit(strings.Join(lines, "\n"))
		state.breakLines(2)
	case frameList:
		state.lists = state.lists[:len(state.lists)-1]
		state.breakLines(2 - min(len(state.lists), 1))
	case framePre:
		state.pre--
		if state.current().last != '\n' {
			state.current().write("\n")
		}
		state.current().write("```")
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

package web

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// omittedHTML replaces HTML whose tree construction is unbounded: input over
// the conversion cost budget, or input the parser refuses because its
// open-element stack would exceed 512 elements.
const omittedHTML = "[HTML content omitted: unable to convert safely.]"

// maxConversionCost bounds the weight tree construction may create, where an
// element weighs one plus its attributes and a text node weighs one. The
// parser's depth limit bounds nesting, not cumulative work: formatting elements
// stay in the list of active formatting elements and are cloned at every later
// insertion point, attributes included, so a few KB can expand to millions of
// elements or hundreds of MB of copied attributes.
const maxConversionCost = 1 << 18

// impliedElements bounds the elements a table tag creates besides its own: a
// cell implies tbody and tr, a column implies colgroup. Charging it only for
// those tags keeps a page of many short tags, whose tree the conversion input
// cap already bounds, from spending the budget on elements it never creates.
const impliedElements = 2

// adoptionClones bounds the elements one run of the adoption agency creates:
// its outer loop runs at most eight times, each iteration creates one element
// and clones at most three more.
const adoptionClones = 32

// Elements whose start tag the parser may precede with implied table elements.
var tableElements = map[string]bool{
	"caption": true, "col": true, "colgroup": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "tr": true,
}

// noahsArkLimit is how many identical formatting elements the parser keeps in
// the list of active formatting elements.
const noahsArkLimit = 3

// maxTagScan bounds how far scanTag reads for one tag, so a tag with an
// unterminated quote cannot make the scan quadratic. Past it the remaining
// input bounds the attributes the tag could still carry.
const maxTagScan = 4096

// Elements the parser keeps in the list of active formatting elements.
var formattingElements = map[string]bool{
	"a": true, "b": true, "big": true, "code": true, "em": true, "font": true, "i": true, "nobr": true,
	"s": true, "small": true, "strike": true, "strong": true, "tt": true, "u": true,
}

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

// maxRenderBytes stops the walk once the text written, captured content
// included, cannot fit the fetch output budget. Nested visible formatting makes
// each level copy the level below it, so rendering has to stop at the budget
// rather than format everything and truncate afterwards. One UTF-16 unit is at
// most three UTF-8 bytes, so this never cuts output the budget would keep.
const maxRenderBytes = 3 * maxFetchOutputUnits

// write records the text, stopping at maxRenderBytes. Captured content is
// counted again when its level writes it out, so nested formatting cannot copy
// without bound.
func (state *renderer) write(value string) {
	remaining := maxRenderBytes - state.written
	if remaining <= 0 {
		state.written = maxRenderBytes + 1
		return
	}
	if len(value) > remaining {
		// Never split a rune: the result has to stay valid UTF-8.
		for remaining > 0 && value[remaining]&0xC0 == 0x80 {
			remaining--
		}
		value = value[:remaining]
		state.written = maxRenderBytes + 1
	}
	state.written += len(value)
	state.current().write(value)
}

// renderer converts a token stream to Markdown-like text in one linear pass.
type renderer struct {
	captures []*buffer
	stack    []frame
	lists    []list
	tables   []int
	rows     []*row
	hidden   int
	written  int
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
	if conversionCost(source) > maxConversionCost {
		return omittedHTML
	}
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
	if state.written > maxRenderBytes {
		// Rendering stopped at the output budget; mark what it dropped.
		return strings.TrimSpace(text + "\n\n" + omittedHTML)
	}
	if templateStartTags(source) > parsed {
		// x/net/html ignores the rest of the input at a template start tag
		// processed while SVG or MathML is open; mark what was dropped.
		return strings.TrimSpace(text + "\n\n" + omittedHTML)
	}
	return text
}

// walk renders one parsed node and its subtree, stopping at the output budget.
func (state *renderer) walk(node *html.Node) {
	if state.written > maxRenderBytes {
		return
	}
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

// conversionCost reports an upper bound on the weight tree construction may
// create: every element counts one plus its attributes, because x/net/html
// copies the whole attribute slice whenever it clones a node, and every text
// node counts one.
//
// The scan reads bytes only. It never interprets comments, CDATA sections, raw
// text or foreign content, because the parser's context decides where those
// begin and end: a comment opened inside script is text to the parser, while
// the same bytes hide markup from a tokenizer. Every `<name` and `</name` is an
// insertion point, as is every run of other bytes, and the parser's own tag
// tokens must start the same way, so the scan never sees fewer insertion points
// than the parser.
//
// Each insertion point costs the active formatting weight plus impliedElements,
// which covers reconstruction before inserting a node, the node itself and the
// elements it implies. `</br>` is an insertion point like the `<br>` the parser
// substitutes, and text insertion reconstructs before inserting, so both are
// covered. Every end tag of a formatting element, and every `<a>` or `<nobr>`
// start tag, also costs adoptionClones times the heaviest formatting weight,
// which bounds one run of the adoption agency.
//
// The active formatting weight only grows, except where an end tag matches the
// top of the lexical element stack. That exception is conservative: the parser
// removes the entry when it closes an element that is both in scope and
// innermost, and an end tag the parser ignores or reads as text is not on top
// of this stack either. Identical start tags stop adding weight after
// noahsArkLimit, matching the clause that keeps at most three of them.
func conversionCost(source string) int {
	var open []openElement
	signatures := map[string]int{}
	cost, weight, heaviest, anchors := 0, 0, 0, 0
	for index := 0; index < len(source); {
		start := strings.IndexByte(source[index:], '<')
		if start != 0 {
			// One run of text inserts one text node.
			cost += weight + 1
			if start < 0 {
				return cost
			}
			index += start
		}
		closing := index+1 < len(source) && source[index+1] == '/'
		nameStart := index + 1
		if closing {
			nameStart++
		}
		name, nameEnd := scanTagName(source, nameStart)
		if name == "" {
			// Not a tag: the parser reads "<" as text.
			cost += weight + 1
			index++
			continue
		}
		index = nameEnd
		if closing {
			// An end tag inserts nothing of its own; one element covers the
			// paragraph an unmatched `</p>` opens and the `<br>` `</br>` becomes.
			cost += weight + 1
			_, _, length := scanTag(source[index:], false)
			index += length
			if formattingElements[name] && !matchesTop(open, name) {
				// A formatting end tag the parser does not close directly runs
				// the adoption agency, which clones.
				cost += adoptionClones * (heaviest + 1)
			}
			popMatching(&open, name, &weight, signatures, &anchors)
			continue
		}
		signature, attributes, length := scanTag(source[index:], formattingElements[name])
		if signature == "" {
			// A tag whose bytes the parser may read differently counts as a
			// formatting element of its own.
			signature = "\x01" + strconv.Itoa(index)
		} else {
			signature = name + "\x00" + signature
		}
		index += length
		// The element itself weighs one plus its attributes, because a clone
		// copies the whole attribute slice.
		cost += weight + 1 + attributes
		if tableElements[name] {
			cost += impliedElements
		}
		switch {
		case !formattingElements[name]:
		case voidElements[name]:
		default:
			if (name == "a" || name == "nobr") && anchors > 0 {
				// An a or nobr already in the list makes the start tag run the
				// adoption agency before the new element joins it.
				cost += adoptionClones * (heaviest + 1)
			}
			element := openElement{tag: name, signature: signature}
			if signatures[signature] < noahsArkLimit {
				element.weight = 1 + attributes
				signatures[signature]++
				weight += element.weight
				heaviest = max(heaviest, element.weight)
				if name == "a" || name == "nobr" {
					anchors++
				}
			}
			open = append(open, element)
			continue
		}
		if !voidElements[name] {
			open = append(open, openElement{tag: name})
		}
		if cost > maxConversionCost {
			return cost
		}
	}
	return cost
}

// openElement is one entry on conversionCost's lexical element stack.
type openElement struct {
	tag       string
	signature string
	weight    int
}

func matchesTop(open []openElement, name string) bool {
	return len(open) > 0 && open[len(open)-1].tag == name
}

// popMatching releases an element only when the end tag closes the innermost
// open element. The parser removes an entry from the list of active formatting
// elements when it closes an element that is both innermost and in scope; an
// end tag it ignores, or reads as text inside raw text or foreign content, does
// not match here either, so the entry keeps its weight.
func popMatching(open *[]openElement, name string, weight *int, signatures map[string]int, anchors *int) {
	if !matchesTop(*open, name) {
		return
	}
	last := len(*open) - 1
	element := (*open)[last]
	*open = (*open)[:last]
	if element.weight == 0 {
		return
	}
	*weight -= element.weight
	signatures[element.signature]--
	if element.tag == "a" || element.tag == "nobr" {
		*anchors--
	}
}

// scanTagName reads the tag name starting at index and returns it lower-cased
// with the offset after it, or "" when no name follows.
func scanTagName(source string, index int) (name string, end int) {
	if index >= len(source) || !isASCIILetter(source[index]) {
		return "", index
	}
	end = index
	for end < len(source) && (isASCIILetter(source[end]) || isASCIIDigit(source[end]) || source[end] == '-') {
		end++
	}
	return strings.ToLower(source[index:end]), end
}

func isASCIILetter(char byte) bool {
	return 'a' <= char|0x20 && char|0x20 <= 'z'
}

func isASCIIDigit(char byte) bool {
	return '0' <= char && char <= '9'
}

// scanTag reads the rest of one start tag, whose name already ended at source's
// start, and reports an upper bound on its attributes, the signature the Noah's
// Ark clause compares and how far the scan may consume.
//
// A quoted ">" belongs to the tag, so the scan tracks quotes when looking for
// the tag's end. It never consumes past a "<" even so: whether these bytes are
// a tag at all depends on the parser's context, and inside raw text the same
// bytes are text followed by real tags. Attributes are still counted across the
// whole tag, so a tag the parser does parse is never under-counted. A signature
// is only faithful when the tag ended at its own ">" with no "<" inside and
// within maxTagScan; otherwise the element counts as unique, which only adds
// weight, and an over-long tag is bounded by the input it could still cover.
func scanTag(source string, formatting bool) (signature string, attributes, consumed int) {
	quote, boundary, cut, length := byte(0), true, -1, 0
	for length < len(source) {
		if length >= maxTagScan {
			return "", max(len(source)/2, 1), max(cut, 1)
		}
		char := source[length]
		length++
		if char == '<' && cut < 0 {
			// Quoting cannot protect this: the parser may read the bytes before
			// it as text and these as a tag of its own.
			cut = length - 1
		}
		switch {
		case quote != 0:
			if char == quote {
				quote, boundary = 0, true
			}
		case char == '"' || char == '\'':
			quote, boundary = char, false
		case char == '>':
			if cut >= 0 {
				return "", attributes, cut
			}
			if !formatting {
				return "", attributes, length
			}
			return source[:length], attributes, length
		case char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == '\f' || char == '/':
			// Whitespace, a solidus or a closed value ends the previous name.
			boundary = true
		case char == '=':
			boundary = false
		default:
			// Any other character starts an attribute name. Duplicate names
			// collapse in the parser, so this can only over-count.
			if boundary {
				attributes++
			}
			boundary = false
		}
	}
	// An unterminated tag: the parser reads the rest of the input as this tag
	// and drops it at EOF, so the bytes scanned bound anything it could create.
	if cut >= 0 {
		return "", attributes, cut
	}
	return "", attributes, length
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
		state.written += state.newlines
		current.write(strings.Repeat("\n", state.newlines))
	case state.space && current.last != ' ' && current.last != '\n':
		state.written++
		current.write(" ")
	}
	state.newlines, state.space, state.marker = 0, false, false
}

func (state *renderer) emit(value string) {
	state.flush()
	state.write(value)
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
			state.write("  \n")
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

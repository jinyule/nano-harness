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

// noahsArkLimit is how many identical formatting elements the parser keeps in
// the list of active formatting elements.
const noahsArkLimit = 3

// Elements the parser keeps in the list of active formatting elements.
var formattingElements = map[string]bool{
	"a": true, "b": true, "big": true, "code": true, "em": true, "font": true, "i": true, "nobr": true,
	"s": true, "small": true, "strike": true, "strong": true, "tt": true, "u": true,
}

// Elements whose start tag the parser may precede with implied table elements.
var tableElements = map[string]bool{
	"caption": true, "col": true, "colgroup": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "tr": true,
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

// maxRenderBytes stops the walk once the output cannot fit the fetch output
// budget. Rendering has to stop there rather than format everything and
// truncate afterwards. One UTF-16 unit is at most three UTF-8 bytes, so this
// never cuts output the budget would keep.
const maxRenderBytes = 3 * maxFetchOutputUnits

// maxCapturedBytes bounds the text held in capture levels. A capture does not
// spend the output budget, because its content is written out again at the
// level above and must stay available in full; but nested visible formatting
// makes each level copy the level below it, so the total is bounded too. Pages
// that fit the output budget stay far below this: the calibration pages hold at
// most 58 KB in captures, and a body wrapped in eight levels of visible
// formatting still fits.
const maxCapturedBytes = 8 * maxRenderBytes

// write records value in the current level, stopping at the output budget.
// The root level holds what the model receives and spends the budget once. A
// capture level does not spend it, because its content is written out again
// at the level above; but no capture may grow past what the root can still
// take, since that excess could never be shown, and all captures together stay
// within maxCapturedBytes, because nested visible formatting makes each level
// copy the level below it. state.stopped carries the result.
func (state *renderer) write(value string) {
	remaining := maxRenderBytes - state.written
	if len(state.captures) > 1 {
		remaining -= state.current().text.Len()
		state.captured += len(value)
		if state.captured > maxCapturedBytes {
			state.stopped = true
			return
		}
	}
	if remaining <= 0 {
		state.stopped = true
		return
	}
	if len(value) > remaining {
		value = cutUTF8(value, remaining)
		state.stopped = true
	}
	if len(state.captures) == 1 {
		state.written += len(value)
	}
	state.current().write(value)
}

// cutUTF8 returns the longest prefix of value within limit bytes that does not
// split a rune, so the result stays valid UTF-8.
func cutUTF8(value string, limit int) string {
	for limit > 0 && value[limit]&0xC0 == 0x80 {
		limit--
	}
	return value[:limit]
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
	captured int
	stopped  bool
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
// dropped reports that the omission marker replaced the input or part of it,
// which the fetch output reports as truncation.
func renderHTML(source string) (text string, dropped bool) {
	if conversionCost(source) > maxConversionCost {
		return omittedHTML, true
	}
	// The reader is in memory, so the only parse error is the depth limit.
	nodes, err := html.ParseFragmentWithOptions(strings.NewReader(source), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}, html.ParseOptionEnableScripting(false))
	if err != nil {
		return omittedHTML, true
	}
	state := &renderer{captures: []*buffer{{}}}
	parsed := 0
	for _, node := range nodes {
		state.walk(node)
		parsed += countTemplates(node)
	}
	text = strings.TrimSpace(state.captures[0].text.String())
	// Rendering stopped at the output budget, or x/net/html ignored the rest of
	// the input at a template start tag processed while SVG or MathML was open;
	// mark what was dropped.
	if state.stopped || templateStartTags(source) > parsed {
		return strings.TrimSpace(text + "\n\n" + omittedHTML), true
	}
	return text, false
}

// walk renders one parsed node and its subtree, stopping at the output budget.
func (state *renderer) walk(node *html.Node) {
	if state.stopped {
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

// conversionCost estimates the weight tree construction may create: every
// element counts one plus its attributes, because x/net/html copies the whole
// attribute slice whenever it clones a node, and every text node counts one.
// Above maxConversionCost the caller omits the input. The estimate is a
// best-effort upper bound, not a strict one: ADR-0011 lists inputs it is known
// to undercount, by enough that the budget can be bypassed.
//
// HTML content is priced from an x/net tokenizer. The parser changes the
// tokenizer only through NextIsNotRawText (parse.go:645, 748 and 1096 for
// noscript, and 2122 when it inserts a foreign element) and AllowCDATA
// (parse.go:2233, foreign content only); this calls NextIsNotRawText after
// every noscript start tag because scripting is disabled. The parser skips
// that call when it ignores the noscript tag, so the token streams can
// diverge. Weight is released only when an end tag closes the innermost open
// element. Inside an SVG or MathML subtree the streams diverge by design, so
// that stretch is priced by bytes and its weight is never released; see
// foreignEnd for where the tokenizer resumes.
func conversionCost(source string) int {
	state := &costState{signatures: map[string]int{}}
	for start, regions := 0, 0; start < len(source) && state.cost <= maxConversionCost; regions++ {
		begin := state.scanHTML(source, start)
		if begin >= len(source) {
			break
		}
		resume := len(source)
		if regions < maxForeignRegions {
			resume = foreignEnd(source, begin)
		}
		state.scanForeign(source[begin:resume])
		start = resume
	}
	return state.cost
}

// maxForeignRegions bounds how many SVG or MathML subtrees get their own
// resumed tokenizer, each of which allocates a read buffer. Past it the rest of
// the input is priced by bytes, which only adds.
const maxForeignRegions = 256

// scanHTML prices HTML content from start with one tokenizer and returns the
// offset just after the first svg or math start tag that opens foreign content,
// or the input length. A self-closing svg or math closes at once, so the
// parser and tokenizer stay in step past it.
func (state *costState) scanHTML(source string, start int) int {
	tokenizer := html.NewTokenizer(strings.NewReader(source[start:]))
	offset := start
	for state.cost <= maxConversionCost {
		kind := tokenizer.Next()
		raw := tokenizer.Raw()
		offset += len(raw)
		switch kind {
		case html.ErrorToken:
			return len(source)
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data == "noscript" {
				tokenizer.NextIsNotRawText()
			}
			state.startTag(token.Data, len(token.Attr), string(raw))
			if (token.Data == "svg" || token.Data == "math") && kind == html.StartTagToken {
				return offset
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			state.endTag(string(name))
		case html.TextToken, html.CommentToken, html.DoctypeToken:
			state.text()
		}
	}
	return len(source)
}

// costState accumulates conversionCost's running weight and total.
type costState struct {
	open       []openElement
	signatures map[string]int
	cost       int
	weight     int
	heaviest   int
	anchors    int
	// foreign is set while a foreign subtree is priced by bytes.
	foreign bool
}

// openElement is one entry on costState's lexical element stack. sticky marks
// an element opened inside a foreign subtree: its weight is never released,
// because nothing there is certain to be a tag.
type openElement struct {
	tag       string
	signature string
	weight    int
	sticky    bool
}

func (state *costState) text() { state.cost += state.weight + 1 }

// startTag charges the element and the clones its start may trigger, and tracks
// the formatting weight it adds.
func (state *costState) startTag(name string, attributes int, raw string) {
	state.cost += state.weight + 1 + attributes
	if tableElements[name] {
		state.cost += impliedElements
	}
	element := openElement{tag: name, sticky: state.foreign}
	if formattingElements[name] {
		if (name == "a" || name == "nobr") && state.anchors > 0 {
			// An a or nobr already in the list makes the start tag run the
			// adoption agency before the new element joins it.
			state.cost += adoptionClones * (state.heaviest + 1)
		}
		signature := name + "\x00" + raw
		if state.signatures[signature] < noahsArkLimit {
			element.signature = signature
			element.weight = 1 + attributes
			state.signatures[signature]++
			state.weight += element.weight
			state.heaviest = max(state.heaviest, element.weight)
			if name == "a" || name == "nobr" {
				state.anchors++
			}
		}
	}
	// HTML ignores a self-closing slash on non-void elements, so only void
	// elements leave nothing open.
	if !voidElements[name] {
		state.open = append(state.open, element)
	}
}

// endTag charges the node and any adoption-agency clones. It releases weight
// only in HTML content, only for an element opened there, and only when the tag
// closes the innermost open element, the condition under which the parser
// removes that entry from the list of active formatting elements when its
// token stream matches this one.
func (state *costState) endTag(name string) {
	state.cost += state.weight + 1
	top := len(state.open) > 0 && state.open[len(state.open)-1].tag == name
	if formattingElements[name] && !top {
		state.cost += adoptionClones * (state.heaviest + 1)
	}
	if !top {
		return
	}
	element := state.open[len(state.open)-1]
	state.open = state.open[:len(state.open)-1]
	if state.foreign || element.sticky || element.weight == 0 {
		return
	}
	state.weight -= element.weight
	state.signatures[element.signature]--
	if element.tag == "a" || element.tag == "nobr" {
		state.anchors--
	}
}

// scanForeign prices a foreign subtree by bytes: every "<name"/"</name" is a
// tag and every other run is text, and weight added here is never released.
// The scan never consumes past a nested "<", so markup a quoted value might hide
// is still read.
func (state *costState) scanForeign(source string) {
	state.foreign = true
	defer func() { state.foreign = false }()
	for index := 0; index < len(source) && state.cost <= maxConversionCost; {
		if next := strings.IndexByte(source[index:], '<'); next != 0 {
			state.text()
			if next < 0 {
				return
			}
			index += next
		}
		name, attributes, consumed := scanForeignTag(source[index:])
		switch {
		case name == "":
			state.text()
		case name[0] == '/':
			state.endTag(name[1:])
		default:
			state.startTag(name, attributes, source[index:index+consumed])
		}
		index += consumed
	}
}

// foreignEnd returns where the tokenizer resumes after the foreign subtree
// whose root start tag ends at from: just past the end tag that closes it, or
// the input length. Resuming early undercounts, because the parser may still be
// in foreign content, where CDATA exists and raw text does not; resuming late
// only prices more by bytes. So the end tag only counts when it lies beyond
// every stretch some reading takes as non-markup: comment and CDATA interiors,
// bogus comments, quoted attribute values and raw-text content, each to its
// farthest possible end. SVG and MathML roots are counted apart, any start tag
// counts toward the depth wherever it lies, and an end tag only counts outside
// those stretches. A breakout tag makes the parser leave earlier, which only
// delays resumption here. Resumption can still come too early: the root type is
// inferred from the last "<", a root end tag the parser ignores inside an
// integration point still counts, and readTagExtent's self-closing test is
// looser than x/net's (ADR-0011 lists these known undercounts).
func foreignEnd(source string, from int) int {
	depth := map[string]int{"svg": 0, "math": 0}
	root := strings.ToLower(source[strings.LastIndexByte(source[:from], '<')+1 : from])
	if strings.HasPrefix(root, "math") {
		depth["math"] = 1
	} else {
		depth["svg"] = 1
	}
	horizon := from
	for index := from; index < len(source); index++ {
		next := strings.IndexByte(source[index:], '<')
		if next < 0 {
			break
		}
		index += next
		rest := source[index:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			horizon = max(horizon, farthestEnd(source, index+len("<!--"), "-->"))
		case strings.HasPrefix(rest, "<![CDATA["):
			horizon = max(horizon, farthestEnd(source, index+len("<![CDATA["), "]]>"))
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			horizon = max(horizon, farthestEnd(source, index+2, ">"))
		case len(rest) > 2 && rest[1] == '/' && isASCIILetter(rest[2]), len(rest) > 1 && isASCIILetter(rest[1]):
			name, closing, selfClosing, end := readTagExtent(source, index)
			if closing && index >= horizon && depth[name] > 0 {
				depth[name]--
				if depth["svg"] == 0 && depth["math"] == 0 {
					return end
				}
			}
			horizon = max(horizon, end)
			if !closing {
				if _, root := depth[name]; root && !selfClosing {
					depth[name]++
				}
				if rawTextNames[name] {
					horizon = max(horizon, rawTextExtent(source, name, end))
				}
			}
		case len(rest) > 1 && rest[1] == '/':
			// "</" not followed by a letter opens a bogus comment.
			horizon = max(horizon, farthestEnd(source, index+2, ">"))
		}
	}
	return len(source)
}

// Elements whose content x/net's tokenizer reads as raw text or RCDATA.
var rawTextNames = map[string]bool{
	"iframe": true, "noembed": true, "noframes": true, "noscript": true, "plaintext": true,
	"script": true, "style": true, "textarea": true, "title": true, "xmp": true,
}

// farthestEnd returns the offset just past terminator after from, or the input
// length when it never appears.
func farthestEnd(source string, from int, terminator string) int {
	if end := strings.Index(source[from:], terminator); end >= 0 {
		return from + end + len(terminator)
	}
	return len(source)
}

// readTagExtent reads the tag at index with x/net's grammar, quoted values
// included, and returns its lower-cased name, whether it is an end tag, whether
// the byte before ">" is "/", and the offset just past it (the input length
// when it never closes). x/net additionally refuses self-closing when that "/"
// ends an unquoted attribute value, so this reports some open tags as
// self-closing (a known undercount in ADR-0011).
func readTagExtent(source string, index int) (name string, closing, selfClosing bool, end int) {
	position := index + 1
	if closing = source[position] == '/'; closing {
		position++
	}
	start := position
	for position < len(source) && !isASCIISpace(source[position]) && source[position] != '/' && source[position] != '>' {
		position++
	}
	name = strings.ToLower(source[start:position])
	quote := byte(0)
	for ; position < len(source); position++ {
		char := source[position]
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case (char == '"' || char == '\'') && afterEquals(source, start, position):
			quote = char
		case char == '>':
			return name, closing, source[position-1] == '/' && position-1 > index, position + 1
		}
	}
	return name, closing, false, len(source)
}

// afterEquals reports whether the quote at position opens an attribute value:
// x/net only treats a quote as a delimiter right after "=" and optional
// whitespace.
func afterEquals(source string, start, position int) bool {
	position--
	for position > start && isASCIISpace(source[position]) {
		position--
	}
	return position > start && source[position] == '='
}

// rawTextExtent returns where the raw-text content of name, which starts at
// from, may end: just past the earliest end tag x/net accepts, "</name"
// followed by whitespace, "/" or ">". script can also stay open past such a tag
// in its double-escaped state, so script content holding a comment opener is
// taken to run to the end of the input; plaintext has no end.
func rawTextExtent(source, name string, from int) int {
	if name == "plaintext" {
		return len(source)
	}
	lower := strings.ToLower(source[from:])
	for offset := 0; ; {
		found := strings.Index(lower[offset:], "</"+name)
		if found < 0 {
			return len(source)
		}
		position := offset + found + len("</"+name)
		if position >= len(lower) || isASCIISpace(lower[position]) || lower[position] == '/' || lower[position] == '>' {
			if name == "script" && strings.Contains(lower[:offset+found], "<!--") {
				return len(source)
			}
			_, _, _, end := readTagExtent(source, from+offset+found)
			return end
		}
		offset = position
	}
}

// scanForeignTag reads one tag at the start of source with x/net's tag grammar,
// stopping at any nested "<". It returns the lower-cased name (with a leading
// "/" for an end tag, empty when the bytes are not a tag), the attribute count
// and the bytes consumed. The count never falls below the tokenizer's for a tag
// without a nested "<"; stopping at one undercounts the attributes after it.
func scanForeignTag(source string) (name string, attributes, consumed int) {
	index := 1
	closing := index < len(source) && source[index] == '/'
	if closing {
		index++
	}
	if index >= len(source) || !isASCIILetter(source[index]) {
		return "", 0, 1
	}
	start := index
	for index < len(source) && !isTagNameEnd(source[index]) {
		index++
	}
	name = strings.ToLower(source[start:index])
	for index < len(source) && source[index] != '>' && source[index] != '<' {
		for index < len(source) && isASCIISpace(source[index]) {
			index++
		}
		if index >= len(source) || source[index] == '>' || source[index] == '<' {
			break
		}
		if source[index] == '/' {
			index++
			continue
		}
		attributes++
		index = skipAttribute(source, index)
	}
	if index < len(source) && source[index] == '>' {
		index++
	}
	if closing {
		name = "/" + name
	}
	return name, attributes, index
}

// skipAttribute advances past one attribute's name and optional value, matching
// x/net's reader but never consuming past a nested "<".
func skipAttribute(source string, index int) int {
	// Attribute name: an "=" that opens the name is part of it; otherwise the
	// name ends at "=", whitespace, "/" or ">".
	if source[index] == '=' {
		index++
	}
	for index < len(source) {
		c := source[index]
		if c == '=' || c == '>' || c == '<' || c == '/' || isASCIISpace(c) {
			break
		}
		index++
	}
	for index < len(source) && isASCIISpace(source[index]) {
		index++
	}
	if index >= len(source) || source[index] != '=' {
		return index
	}
	index++
	for index < len(source) && isASCIISpace(source[index]) {
		index++
	}
	if index >= len(source) {
		return index
	}
	switch source[index] {
	case '>', '<':
		return index
	case '\'', '"':
		quote := source[index]
		index++
		for index < len(source) {
			if source[index] == quote {
				return index + 1
			}
			if source[index] == '<' {
				return index
			}
			index++
		}
		return index
	default:
		for index < len(source) && source[index] != '>' && source[index] != '<' && !isASCIISpace(source[index]) {
			index++
		}
		return index
	}
}

func isTagNameEnd(char byte) bool {
	return isASCIISpace(char) || char == '/' || char == '>' || char == '<'
}

func isASCIISpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == '\f'
}

func isASCIILetter(char byte) bool {
	return 'a' <= char|0x20 && char|0x20 <= 'z'
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
		state.count(state.newlines)
	case state.space && current.last != ' ' && current.last != '\n':
		current.write(" ")
		state.count(1)
	}
	state.newlines, state.space, state.marker = 0, false, false
}

// count charges separators written straight to the current level.
func (state *renderer) count(bytes int) {
	if len(state.captures) > 1 {
		state.captured += bytes
		state.stopped = state.stopped || state.captured > maxCapturedBytes
		return
	}
	state.written += bytes
	state.stopped = state.stopped || state.written > maxRenderBytes
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

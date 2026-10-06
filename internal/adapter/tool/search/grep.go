package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// grepMaxMatches is the inline match cap of one grep call.
	grepMaxMatches = 250
	// grepMaxLineBytes bounds one matched-line preview.
	grepMaxLineBytes = 2000
)

type grepArgs struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
	Include *string `json:"include"`
}

type grepMatch struct {
	path string
	line int
	text string
}

// grepRecord is the subset of one `rg --json` line grep consumes.
type grepRecord struct {
	Type string `json:"type"`
	Data *struct {
		Path *struct {
			Text *string `json:"text"`
		} `json:"path"`
		LineNumber *int `json:"line_number"`
		Lines      *struct {
			Text  *string `json:"text"`
			Bytes *string `json:"bytes"`
		} `json:"lines"`
	} `json:"data"`
}

func (provider *Provider) grepTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[grepArgs]{
		Name: "grep",
		Description: "Search file contents with a ripgrep regular expression. Returns matching lines with line numbers, grouped by file. " +
			fmt.Sprintf("Returns up to %d matches; a larger result reports where the complete match list was saved.", grepMaxMatches),
		Parameters: appTool.Parameters{
			appTool.Required("pattern", appTool.String("Regular expression to search for (ripgrep syntax).")),
			appTool.Optional("path", appTool.String("File or directory to search. Defaults to the session workspace; a relative path resolves against it.")),
			appTool.Optional("include", appTool.String("One glob filter for which files to search (e.g. \"*.ts\", \"*.{js,jsx}\"). Not a list; negation is not supported.")),
		},
		Guidance: appTool.Guidance{Order: appTool.OrderGrep, Text: func(visible func(string) bool) string {
			text := "Use the grep tool — not shell grep or rg — to search file contents."
			if visible("read") {
				text += " Use read on a matched file when you need surrounding context."
			}
			return text
		}},
		Check:      checkGrep,
		Concurrent: func(grepArgs) bool { return true },
		Execute:    provider.grep,
	})
}

func checkGrep(_ appTool.Invocation, arguments grepArgs) error {
	if arguments.Pattern == "" {
		return errors.New("pattern must be a non-empty string")
	}
	if arguments.Path != nil && appTool.IsBlank(*arguments.Path) {
		return errors.New("path must be a non-empty string when given")
	}
	if arguments.Include == nil {
		return nil
	}
	include := *arguments.Include
	if appTool.IsBlank(include) {
		return errors.New("include must be a non-empty glob when given")
	}
	if strings.HasPrefix(include, "!") {
		return errors.New(`include must be a positive glob filter; negated patterns ("!…") are not supported`)
	}
	depth := 0
	for _, char := range include {
		switch {
		case char == '{':
			depth++
		case char == '}':
			depth = max(0, depth-1)
		case char == ',' && depth == 0:
			return errors.New("include must be one glob, not a comma-separated list (use {a,b} alternation instead)")
		}
	}
	return nil
}

// grep runs `rg --json --regexp=<pattern> [--glob=<include>]` and groups the
// first matches by file in ripgrep's output order.
func (provider *Provider) grep(ctx context.Context, invocation appTool.Invocation, arguments grepArgs) (appTool.Result, error) {
	start, err := provider.locate(ctx, invocation, "grep", arguments.Path, true)
	if err != nil {
		return appTool.Result{}, err
	}
	// ripgrep would block reading a FIFO or device named explicitly.
	if !start.info.IsDir() && !start.info.Mode().IsRegular() {
		return appTool.Result{}, fmt.Errorf("grep search failed: %q is not a regular file or directory", start.relative)
	}
	command := []string{"--json", "--regexp=" + arguments.Pattern}
	if arguments.Include != nil {
		command = append(command, "--glob="+*arguments.Include)
	}
	stdout, empty, err := provider.run(ctx, "grep", append(command, start.arguments()...))
	if err != nil {
		return appTool.Result{}, err
	}
	var matches []grepMatch
	if !empty {
		if matches, err = parseMatches(stdout); err != nil {
			return appTool.Result{}, err
		}
	}
	if len(matches) <= grepMaxMatches {
		return appTool.Text(renderGrep(matches, nil)), nil
	}
	// The artifact holds every match with its line preview, so it is the
	// complete search rather than a longer page.
	complete := fmt.Sprintf("Found %d matches\n\n%s", len(matches), groupMatches(matches))
	ref, err := invocation.SaveText(ctx, "grep-results.txt", complete)
	if err != nil {
		// Like upstream, an unsaved complete result changes the footer, not
		// the outcome of the search.
		return appTool.Text(renderGrep(matches, nil)), nil
	}
	return appTool.Text(renderGrep(matches, &ref)), nil
}

// parseMatches reads every match record from complete `rg --json` output.
// Other record types are framing; a malformed match fails the search rather
// than returning a partial result.
func parseMatches(stdout string) ([]grepMatch, error) {
	var matches []grepMatch
	for line := range strings.SplitSeq(stdout, "\n") {
		if line == "" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			return nil, malformed("a line is not JSON")
		}
		switch header := parsed.(type) {
		case map[string]any:
			if header["type"] != "match" {
				continue
			}
		case []any:
			// JavaScript arrays are objects without a match type.
			continue
		default:
			return nil, malformed("a record is not an object")
		}
		var record grepRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, malformed("a record is not an object")
		}
		data := record.Data
		switch {
		case data == nil:
			return nil, malformed("a match record has no data")
		case data.Path == nil || data.Path.Text == nil:
			return nil, malformed("a match record has no path text")
		case data.LineNumber == nil:
			return nil, malformed("a match record has no line number")
		case data.Lines == nil:
			return nil, malformed("a match record has no line content")
		case data.Lines.Text != nil:
			text := *data.Lines.Text
			if trimmed, ok := strings.CutSuffix(text, "\n"); ok {
				text = strings.TrimSuffix(trimmed, "\r")
			}
			matches = append(matches, grepMatch{path: *data.Path.Text, line: *data.LineNumber, text: text})
		case data.Lines.Bytes != nil:
			matches = append(matches, grepMatch{path: *data.Path.Text, line: *data.LineNumber, text: "(line is not valid UTF-8)"})
		default:
			return nil, malformed("a match record has neither line text nor bytes")
		}
	}
	return matches, nil
}

func malformed(detail string) error {
	return fmt.Errorf("grep received malformed ripgrep --json output (%s)", detail)
}

// previewLine bounds one matched line to grepMaxLineBytes at a rune boundary.
func previewLine(line string) string {
	if len(line) <= grepMaxLineBytes {
		return line
	}
	cut := grepMaxLineBytes
	for !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + " (line truncated)"
}

// renderGrep keeps the first grepMaxMatches matches grouped by file, like
// upstream; a capped result points at the saved complete result, or says it
// could not be saved.
func renderGrep(matches []grepMatch, ref *appTool.SpillRef) string {
	if len(matches) == 0 {
		return "No matches found"
	}
	header := fmt.Sprintf("Found %d matches", len(matches))
	if len(matches) == 1 {
		header = "Found 1 match"
	}
	if len(matches) <= grepMaxMatches {
		return header + "\n\n" + groupMatches(matches)
	}
	recovery := "The complete result could not be saved; narrow pattern, path, or include to see more."
	if ref != nil {
		recovery = fmt.Sprintf("Full grep result stored at: %s. %s", ref.Locator, ref.Hint)
	}
	return fmt.Sprintf("Found %d of %d matches\n\n%s\n\n(%s)", grepMaxMatches, len(matches), groupMatches(matches[:grepMaxMatches]), recovery)
}

// groupMatches groups matches by file in first-seen order with bounded line
// previews.
func groupMatches(matches []grepMatch) string {
	var order []string
	groups := map[string][]string{}
	for _, match := range matches {
		if _, seen := groups[match.path]; !seen {
			order = append(order, match.path)
		}
		groups[match.path] = append(groups[match.path], fmt.Sprintf("Line %d: %s", match.line, previewLine(match.text)))
	}
	sections := make([]string, len(order))
	for index, path := range order {
		sections[index] = path + "\n" + strings.Join(groups[path], "\n")
	}
	return strings.Join(sections, "\n\n")
}

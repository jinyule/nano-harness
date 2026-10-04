package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"unicode/utf8"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// grepMaxMatches is the inline match cap of one grep call.
	grepMaxMatches = 250
	// grepMaxLineBytes bounds one matched-line preview.
	grepMaxLineBytes = 2000
	// maxScanLineBytes is the longest line grep holds in memory; scanning
	// stops at a longer line in that file.
	maxScanLineBytes = 8 << 20
	// binaryPeekBytes is the prefix that marks a traversed file as binary.
	binaryPeekBytes = 64 << 10
)

type grepArgs struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
	Include *string `json:"include"`
}

type grepMatch struct {
	display string
	line    int
	text    string
}

// grepResult retains the first matches and counts the rest.
type grepResult struct {
	expression *regexp.Regexp
	matches    []grepMatch
	total      int
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

func checkGrep(arguments grepArgs) error {
	if arguments.Pattern == "" {
		return errors.New("pattern must be a non-empty string")
	}
	if arguments.Path != nil && strings.TrimSpace(*arguments.Path) == "" {
		return errors.New("path must be a non-empty string when given")
	}
	if arguments.Include == nil {
		return nil
	}
	include := *arguments.Include
	if strings.TrimSpace(include) == "" {
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

// grep searches like `rg --json --regexp=<pattern> [--glob=<include>]`:
// traversal skips hidden entries and ignore-file matches unless the include
// glob whitelists a file, explicit file paths are always searched, and
// matches keep traversal order grouped by file.
func (provider *Provider) grep(ctx context.Context, _ appTool.Invocation, arguments grepArgs) (appTool.Result, error) {
	expression, err := regexp.Compile(arguments.Pattern)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("grep pattern rejected: %w", err)
	}
	var include *pattern
	if arguments.Include != nil {
		compiled, err := compilePattern(*arguments.Include)
		if err != nil {
			return appTool.Result{}, fmt.Errorf("grep include rejected: %w", err)
		}
		include = &compiled
	}
	start, err := provider.locate("grep", arguments.Path)
	if err != nil {
		return appTool.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	result := &grepResult{expression: expression}
	switch {
	case start.info.IsDir():
		walk := walker{ctx: ctx, filtered: true, include: include, visit: func(resolved, display string, _ fs.DirEntry) error {
			return result.scan(ctx, resolved, display, false)
		}}
		err = walk.start(provider.root.Path(), start.resolved, start.display)
	case start.info.Mode().IsRegular():
		err = result.scan(ctx, start.resolved, start.display, true)
	default:
		err = fmt.Errorf("%q is not a regular file or directory", start.display)
	}
	if err != nil {
		return appTool.Result{}, aborted("grep", err)
	}
	return appTool.Text(result.render()), nil
}

// scan matches each line of one file. A traversed file with a NUL byte in
// its leading block is binary and skipped; a later NUL stops the scan. An
// explicit file is searched with NUL bytes treated as line breaks.
func (result *grepResult) scan(ctx context.Context, resolved, display string, explicit bool) error {
	file, err := openFile(resolved)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }() // read-only; close cannot lose data
	reader := bufio.NewReaderSize(file, binaryPeekBytes)
	if !explicit {
		peek, err := reader.Peek(binaryPeekBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if bytes.IndexByte(peek, 0) >= 0 {
			return nil
		}
	}
	number := 0
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		piece, readErr := reader.ReadSlice('\n')
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		line = append(line, piece...)
		if len(line) > maxScanLineBytes {
			return nil
		}
		if bytes.HasSuffix(line, []byte("\n")) || errors.Is(readErr, io.EOF) && len(line) > 0 {
			content := bytes.TrimSuffix(line, []byte("\n"))
			if !explicit && bytes.IndexByte(content, 0) >= 0 {
				return nil
			}
			for segment := range bytes.SplitSeq(content, []byte{0}) {
				number++
				result.record(display, number, segment)
			}
			line = line[:0]
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func (result *grepResult) record(display string, number int, line []byte) {
	if !result.expression.Match(line) {
		return
	}
	result.total++
	if len(result.matches) < grepMaxMatches {
		result.matches = append(result.matches, grepMatch{display: display, line: number, text: previewLine(line)})
	}
}

// previewLine drops a trailing CR and bounds the line to grepMaxLineBytes
// at a rune boundary, as ripgrep's JSON text is rendered upstream.
func previewLine(line []byte) string {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if !utf8.Valid(line) {
		return "(line is not valid UTF-8)"
	}
	if len(line) <= grepMaxLineBytes {
		return string(line)
	}
	cut := grepMaxLineBytes
	for !utf8.RuneStart(line[cut]) {
		cut--
	}
	return string(line[:cut]) + " (line truncated)"
}

func (result *grepResult) render() string {
	if result.total == 0 {
		return "No matches found"
	}
	header := fmt.Sprintf("Found %d matches", result.total)
	switch {
	case result.total == 1:
		header = "Found 1 match"
	case result.total > len(result.matches):
		header = fmt.Sprintf("Found %d of %d matches", len(result.matches), result.total)
	}
	var body strings.Builder
	for index, match := range result.matches {
		if index == 0 || match.display != result.matches[index-1].display {
			if index > 0 {
				body.WriteString("\n\n")
			}
			body.WriteString(match.display)
		}
		fmt.Fprintf(&body, "\nLine %d: %s", match.line, match.text)
	}
	text := header + "\n\n" + body.String()
	if result.total > len(result.matches) {
		text += "\n\n(The complete result could not be saved; narrow pattern, path, or include to see more.)"
	}
	return text
}

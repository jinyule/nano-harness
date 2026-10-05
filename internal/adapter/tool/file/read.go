package file

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// readLimit is the default and maximum line count of one read.
	readLimit = 2000
	// readMaxLineLength bounds one returned line in runes.
	readMaxLineLength = 2000
	// readMaxBytes bounds the selected lines of one read.
	readMaxBytes = 50 * 1024
	// binarySampleBytes is the prefix searched for NUL bytes.
	binarySampleBytes = 8192
	readBufferBytes   = 64 << 10
	// lineBufferBytes always holds readMaxLineLength+1 complete runes, so a
	// capped line still proves it needs truncation.
	lineBufferBytes = (readMaxLineLength + 1) * utf8.UTFMax
	// maxOffset keeps line arithmetic exact for absurd offsets.
	maxOffset = 1 << 53
)

var (
	errBinary  = errors.New("binary file")
	errNotText = errors.New("invalid UTF-8 text")
	utf8BOM    = []byte{0xef, 0xbb, 0xbf}
)

type readArgs struct {
	FilePath string   `json:"file_path"`
	Offset   *float64 `json:"offset"`
	Limit    *float64 `json:"limit"`
}

func (provider *Provider) readTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[readArgs]{
		Name:        "read",
		Description: "Read a UTF-8 text file and return line-numbered content.",
		Parameters: appTool.Parameters{
			appTool.Required("file_path", appTool.String("Path to read, resolved by the filesystem backend.")),
			appTool.Optional("offset", appTool.Number("1-based first line to return. Defaults to 1.")),
			appTool.Optional("limit", appTool.Number(fmt.Sprintf("Maximum number of lines to return. Defaults to %d.", readLimit))),
		},
		Guidance: appTool.StaticGuidance(appTool.OrderRead, "Use the read tool — not shell commands like cat — to inspect text files. Use offset and limit to continue reading large files."),
		Check:    checkRead,
		// Observation races fail closed: a guarded mutation re-checks the
		// version under its lock and reports a stale read.
		Concurrent: func(readArgs) bool { return true },
		// Reading a spilled artifact must not spill again.
		KeepInline: true,
		Execute:    provider.read,
	})
}

func checkRead(_ appTool.Invocation, arguments readArgs) error {
	if strings.TrimSpace(arguments.FilePath) == "" {
		return errors.New("file_path must be a non-empty string")
	}
	if arguments.Offset != nil && !positiveInteger(*arguments.Offset) {
		return errors.New("offset must be a positive integer")
	}
	if arguments.Limit != nil && !positiveInteger(*arguments.Limit) {
		return errors.New("limit must be a positive integer")
	}
	if arguments.Limit != nil && *arguments.Limit > readLimit {
		return fmt.Errorf("limit must be less than or equal to %d", readLimit)
	}
	return nil
}

func positiveInteger(value float64) bool { return value >= 1 && value == math.Trunc(value) }

// read streams one window and records what the session observed: absence
// for a missing path, or the digest of every byte read on success.
func (provider *Provider) read(ctx context.Context, invocation appTool.Invocation, arguments readArgs) (appTool.Result, error) {
	display, path, err := provider.root.Readable(arguments.FilePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		provider.observed.record(invocation.SessionID, display, observation{})
		return appTool.Result{}, fmt.Errorf("cannot read %q: not found", display)
	case err != nil:
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", arguments.FilePath, err)
	}
	info, err := statFile(path)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", display, err)
	}
	if !info.Mode().IsRegular() {
		return appTool.Result{}, fmt.Errorf("cannot read %q: not a regular file", display)
	}
	offset, limit := int64(1), readLimit
	if arguments.Offset != nil {
		offset = int64(min(*arguments.Offset, maxOffset))
	}
	if arguments.Limit != nil {
		limit = int(*arguments.Limit)
	}
	reader, err := openFile(path)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", display, err)
	}
	defer func() { _ = reader.Close() }() // read-only; close cannot lose data
	hasher := sha256.New()
	window, err := readWindow(ctx, io.TeeReader(reader, hasher), offset, limit)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", display, err)
	}
	if !window.capped && offset > window.total && (window.total != 0 || offset != 1) {
		return appTool.Result{}, fmt.Errorf("offset %d is out of range for %q (%d lines)", offset, display, window.total)
	}
	provider.observed.record(invocation.SessionID, path, observation{present: true, version: version(hasher.Sum(nil))})
	return appTool.Text(formatRead(display, offset, window)), nil
}

type numberedLine struct {
	number int64
	text   string
}

// window is the selected lines plus the exact total line count.
type window struct {
	lines  []numberedLine
	total  int64
	capped bool
}

// readWindow streams a UTF-8 file once: it rejects a NUL in the leading
// sample and any invalid UTF-8, strips a leading BOM, counts every line, and
// retains only the requested window within the line, rune, and byte caps.
// Memory stays bounded by the read buffer and one capped line prefix.
func readWindow(ctx context.Context, source io.Reader, offset int64, limit int) (window, error) {
	reader := bufio.NewReaderSize(source, readBufferBytes)
	sample, err := reader.Peek(binarySampleBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return window{}, err
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return window{}, errBinary
	}
	if bytes.HasPrefix(sample, utf8BOM) {
		_, _ = reader.Discard(len(utf8BOM)) // the peeked bytes are buffered
	}
	builder := windowBuilder{offset: offset, limit: limit}
	var (
		line     []byte
		overflow bool
		carry    []byte
	)
	for {
		if err := ctx.Err(); err != nil {
			return window{}, fmt.Errorf("read aborted: %w", err)
		}
		piece, readErr := reader.ReadSlice('\n')
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) && !errors.Is(readErr, io.EOF) {
			return window{}, readErr
		}
		if carry, err = validateUTF8(carry, piece); err != nil {
			return window{}, err
		}
		complete := bytes.HasSuffix(piece, []byte("\n"))
		content := bytes.TrimSuffix(piece, []byte("\n"))
		if room := lineBufferBytes - len(line); len(content) > room {
			line, overflow = append(line, content[:room]...), true
		} else {
			line = append(line, content...)
		}
		if complete || errors.Is(readErr, io.EOF) && (len(line) > 0 || overflow) {
			builder.consume(line, overflow)
			line, overflow = line[:0], false
		}
		if errors.Is(readErr, io.EOF) {
			if len(carry) > 0 {
				return window{}, errNotText
			}
			return builder.result, nil
		}
	}
}

// windowBuilder counts every line and keeps the requested ones within the
// line-count and byte caps.
type windowBuilder struct {
	offset      int64
	limit       int
	result      window
	outputBytes int
}

func (builder *windowBuilder) consume(line []byte, overflow bool) {
	builder.result.total++
	if builder.result.capped || builder.result.total < builder.offset || len(builder.result.lines) >= builder.limit {
		return
	}
	text := renderLine(line, overflow)
	size := len(text)
	if len(builder.result.lines) > 0 {
		size++
	}
	if builder.outputBytes+size > readMaxBytes {
		builder.result.capped = true
		return
	}
	builder.outputBytes += size
	builder.result.lines = append(builder.result.lines, numberedLine{number: builder.result.total, text: text})
}

// validateUTF8 checks carry+piece up to its last complete rune and returns
// the incomplete suffix, which may continue in the next piece.
func validateUTF8(carry, piece []byte) ([]byte, error) {
	data := piece
	if len(carry) > 0 {
		data = slices.Concat(carry, piece)
	}
	cut := len(data)
	for back := 1; back < utf8.UTFMax && back <= len(data); back++ {
		if utf8.RuneStart(data[len(data)-back]) {
			if !utf8.FullRune(data[len(data)-back:]) {
				cut = len(data) - back
			}
			break
		}
	}
	if !utf8.Valid(data[:cut]) {
		return nil, errNotText
	}
	return append([]byte(nil), data[cut:]...), nil
}

func renderLine(line []byte, overflow bool) string {
	if !overflow {
		line = bytes.TrimSuffix(line, []byte("\r"))
	}
	text := string(line)
	if utf8.RuneCountInString(text) <= readMaxLineLength {
		return text
	}
	cut, runes := 0, 0
	for runes < readMaxLineLength {
		_, size := utf8.DecodeRuneInString(text[cut:])
		cut += size
		runes++
	}
	return fmt.Sprintf("%s... (line truncated to %d chars)", text[:cut], readMaxLineLength)
}

// formatRead renders the upstream read envelope and continuation footer.
func formatRead(display string, offset int64, window window) string {
	end := max(0, offset-1)
	if len(window.lines) > 0 {
		end = window.lines[len(window.lines)-1].number
	}
	var footer string
	switch {
	case window.capped:
		footer = fmt.Sprintf("(Output capped. Showing lines %d-%d. Use offset=%d to continue.)", offset, end, end+1)
	case end < window.total:
		footer = fmt.Sprintf("(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, end, window.total, end+1)
	default:
		footer = fmt.Sprintf("(End of file - total %d lines)", window.total)
	}
	var body strings.Builder
	for _, line := range window.lines {
		fmt.Fprintf(&body, "%d: %s\n", line.number, line.text)
	}
	if len(window.lines) > 0 {
		body.WriteString("\n")
	}
	body.WriteString(footer)
	return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s\n</content>", display, body.String())
}

package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

const (
	// spillInlineTokens is the Base composition's estimated-token budget for
	// one inline tool result; larger results become a preview and a locator.
	spillInlineTokens = 12500
	// charsPerToken and blockOverhead are upstream's fixed-density text
	// estimate: ceil(UTF-16 code units / 4) plus 4 tokens of framing.
	charsPerToken = 4
	blockOverhead = 4
	// spillGap marks the omitted interval between the retained head and tail.
	spillGap = "\n\n[...]\n\n"
)

// ErrSpillUnavailable reports that no spill store is registered or the call
// has no owning session, so complete output cannot be saved.
var ErrSpillUnavailable = errors.New("spill storage is unavailable")

// SpillStore opens private artifacts that keep complete tool output outside
// the model context. Name is a caller-chosen file-name hint such as
// "grep-results.txt"; the store derives an unpredictable name from it.
type SpillStore interface {
	Create(ctx context.Context, sessionID, name string) (SpillFile, error)
}

// SpillFile is one open artifact. Write appends content; exactly one of
// Commit or Discard must follow. Commit makes the content durable and returns
// its locator; Discard removes it. A store may reject writes beyond its size
// limit, after which the caller must Discard. Locator is known from creation
// so a producer can advertise a growing artifact, as upstream does for a
// running command's output, before it commits.
type SpillFile interface {
	io.Writer
	Locator() string
	Commit() (SpillRef, error)
	Discard() error
}

// SpillRef locates one committed artifact. Locator is an opaque model-facing
// handle, Bytes the exact UTF-8 size, and Hint the retrieval guidance shown
// next to the locator.
type SpillRef struct {
	Locator string
	Bytes   int
	Hint    string
}

// CreateSpill opens an artifact owned by the call's session. It fails with
// ErrSpillUnavailable when no store is registered or the call has no session.
func (invocation Invocation) CreateSpill(ctx context.Context, name string) (SpillFile, error) {
	if invocation.spill == nil || invocation.SessionID == "" {
		return nil, ErrSpillUnavailable
	}
	return invocation.spill.Create(ctx, invocation.SessionID, name)
}

// SaveText stores complete text owned by the call's session in one artifact.
func (invocation Invocation) SaveText(ctx context.Context, name, content string) (SpillRef, error) {
	file, err := invocation.CreateSpill(ctx, name)
	if err != nil {
		return SpillRef{}, err
	}
	if _, err := io.WriteString(file, content); err != nil {
		return SpillRef{}, errors.Join(err, file.Discard())
	}
	return file.Commit()
}

// UseSpill makes store available to tool calls and the inline-result policy
// for exactly the caller's scope lifetime. Only one store may be in use.
func (runtime *Runtime) UseSpill(store SpillStore, scope *plugin.Scope) error {
	if store == nil || scope == nil {
		return ErrInvalidTool
	}
	runtime.mu.Lock()
	if !runtime.active {
		runtime.mu.Unlock()
		return ErrNotRunning
	}
	if runtime.spill != nil {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: a spill store is already in use", ErrInvalidTool)
	}
	runtime.spill = store
	runtime.mu.Unlock()
	release := func(context.Context) error {
		runtime.mu.Lock()
		if runtime.spill == store {
			runtime.spill = nil
		}
		runtime.mu.Unlock()
		return nil
	}
	if err := scope.Defer(release); err != nil {
		_ = release(context.Background()) // undo the publication made above
		return err
	}
	return nil
}

// retainInline applies upstream's spill policy to one successful result: text
// within the estimated-token budget passes through; larger text is saved and
// replaced by an ordered head/tail preview plus a notice naming the omitted
// bytes and the locator. A failed save, a missing store or session, or a
// notice that cannot fit keeps the original text.
func retainInline(ctx context.Context, invocation Invocation, tool, text string) string {
	if estimateTokens(text) <= spillInlineTokens {
		return text
	}
	ref, err := invocation.SaveText(ctx, tool+".txt", text)
	if err != nil {
		return text
	}
	preview, ok := spillPreview(text, ref, spillInlineTokens)
	if !ok {
		return text
	}
	return preview
}

// spillPreview keeps the largest head and tail that fit budget after
// reserving room for the gap marker and the worst-case notice. Each end
// receives half of the remaining budget.
func spillPreview(text string, ref SpillRef, budget int) (string, bool) {
	worst := spillNotice(len(text), ref)
	if estimateTokens(worst) > budget {
		return "", false
	}
	available := max(0, budget-estimateTokens(spillGap)-estimateTokens("\n\n"+worst))
	head := fitText(text, (available+1)/2, false)
	tail := fitText(text[len(head):], available/2, true)
	notice := spillNotice(len(text)-len(head)-len(tail), ref)
	if head == "" && tail == "" {
		return notice, true
	}
	return head + spillGap + tail + "\n\n" + notice, true
}

// spillNotice renders upstream's recovery notice for omitted bytes.
func spillNotice(omitted int, ref SpillRef) string {
	return fmt.Sprintf("(Omitted %d bytes. Full formatted result stored at: %s. %s)", omitted, ref.Locator, ref.Hint)
}

// estimateTokens prices one text block like upstream's token meter.
func estimateTokens(text string) int {
	return (utf16Length(text)+charsPerToken-1)/charsPerToken + blockOverhead
}

// fitText returns the longest prefix (or suffix when tail is set) whose
// estimate fits budget. Cuts fall on rune boundaries, which is where
// upstream's UTF-16 slicing lands after it refuses to split a surrogate pair.
func fitText(text string, budget int, tail bool) string {
	limit := max(0, (budget-blockOverhead)*charsPerToken)
	if !tail {
		units := 0
		for index, char := range text {
			units += utf16.RuneLen(char)
			if units > limit {
				return text[:index]
			}
		}
		return text
	}
	units, cut := 0, len(text)
	for cut > 0 {
		char, size := utf8.DecodeLastRuneInString(text[:cut])
		if units+utf16.RuneLen(char) > limit {
			break
		}
		units += utf16.RuneLen(char)
		cut -= size
	}
	return text[cut:]
}

func utf16Length(text string) int {
	units := 0
	for _, char := range text {
		units += utf16.RuneLen(char)
	}
	return units
}

package shell

import (
	"context"
	"io"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

// spillThreshold is the process runner's retained tail per stream: a stream
// gets a complete-output file exactly when its rendered text is truncated.
const spillThreshold = 64_000

// streamSpill tees one output stream into a complete-output spill file, like
// upstream's output collector: bytes are buffered until the stream outgrows
// spillThreshold, then the file is created, the buffer written, and every
// later byte appended. A failed create or write, including the store's size
// limit, discards the file for the rest of the stream; the in-memory tail is
// unaffected. Write is called from one goroutine at a time and never fails.
type streamSpill struct {
	next      io.Writer
	open      func() (appTool.SpillFile, error)
	advertise func(locator string)

	buffered []byte
	file     appTool.SpillFile
	disabled bool
}

// newStreamSpill wraps next. open creates the artifact; advertise publishes
// its locator once created and withdraws it with "" when discarded. next and
// advertise may be nil.
func newStreamSpill(next io.Writer, open func() (appTool.SpillFile, error), advertise func(string)) *streamSpill {
	if advertise == nil {
		advertise = func(string) {}
	}
	return &streamSpill{next: next, open: open, advertise: advertise}
}

// openSpill opens artifacts for one stream through the call's session.
func openSpill(ctx context.Context, invocation appTool.Invocation, name string) func() (appTool.SpillFile, error) {
	return func() (appTool.SpillFile, error) { return invocation.CreateSpill(ctx, name) }
}

func (spill *streamSpill) Write(data []byte) (int, error) {
	if spill.next != nil {
		_, _ = spill.next.Write(data) // job and tail writers never fail
	}
	switch {
	case spill.disabled:
	case spill.file != nil:
		if _, err := spill.file.Write(data); err != nil {
			spill.discard()
		}
	case len(spill.buffered)+len(data) <= spillThreshold:
		spill.buffered = append(spill.buffered, data...)
	default:
		spill.start(data)
	}
	return len(data), nil
}

// start creates the artifact on the first overflow and writes everything
// seen so far.
func (spill *streamSpill) start(data []byte) {
	file, err := spill.open()
	if err != nil {
		spill.disabled, spill.buffered = true, nil
		return
	}
	spill.file = file
	_, err = file.Write(spill.buffered)
	if err == nil {
		_, err = file.Write(data)
	}
	spill.buffered = nil
	if err != nil {
		spill.discard()
		return
	}
	spill.advertise(file.Locator())
}

func (spill *streamSpill) discard() {
	_ = spill.file.Discard() // the artifact is abandoned; removal failure leaves a bounded private file
	spill.file, spill.disabled = nil, true
	spill.advertise("")
}

// finish commits the artifact after the stream closed and returns its
// locator, or "" when the stream never overflowed or its file was lost.
func (spill *streamSpill) finish() string {
	if spill.file == nil {
		return ""
	}
	ref, err := spill.file.Commit()
	spill.file = nil
	if err != nil {
		spill.disabled = true
		spill.advertise("")
		return ""
	}
	return ref.Locator
}

// finishAll commits both streams of one command.
func finishAll(streams [2]*streamSpill) [2]string {
	return [2]string{streams[0].finish(), streams[1].finish()}
}

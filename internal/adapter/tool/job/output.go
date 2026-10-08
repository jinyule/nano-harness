package job

import (
	"strings"
	"unicode/utf8"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// renderOutput follows upstream's order: the streamed output, the loss
// notice that ends upstream's delta, the value result, then the status line.
// The status and loss notice are bounded metadata that always survive, even
// without a spill store; the stream is cut before the notice, and the value
// takes what remains. Metadata is bounded independently of value results.
func renderOutput(read appJob.Read) string {
	const (
		metadataBytes = 4096
		truncated     = "\n[output truncated]"
	)
	status := boundedText(read.Job.StatusLine(), metadataBytes, "…]")
	loss := strings.ToValidUTF8(appJob.Read{Lossy: read.Lossy, Spills: read.Spills}.Delta(), "�")
	if len(loss) > metadataBytes {
		loss = appJob.Read{Lossy: true}.Delta()
	}
	limit := session.MaxTextBytes - len(status) - 1
	streamLimit := limit
	if loss != "" {
		streamLimit -= len(loss) + 1
	}
	text := boundedText(appJob.Read{Stdout: read.Stdout, Stderr: read.Stderr}.Delta(), streamLimit, truncated)
	if loss != "" {
		text = withNewline(text) + loss
	}
	if read.Result != "" {
		separated := withNewline(text)
		// A stream that filled the budget already carries the cut marker.
		if room := limit - len(separated); room > len(truncated) {
			text = separated + boundedText(read.Result, room, truncated)
		}
	}
	if text == "" {
		text = "(no new output)"
	}
	return withNewline(text) + status
}

func boundedText(text string, limit int, suffix string) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= limit {
		return text
	}
	cut := limit - len(suffix)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + suffix
}

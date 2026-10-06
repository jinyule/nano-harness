package job

import (
	"strings"
	"unicode/utf8"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// renderOutput reserves the final envelope before budgeting the consuming
// read. Even without a spill store, the durable result keeps its status and
// loss notice. Metadata is bounded independently of producer value results.
func renderOutput(read appJob.Read) string {
	const metadataBytes = 4096
	status := boundedText(read.Job.StatusLine(), metadataBytes, "…]")
	loss := strings.ToValidUTF8(appJob.Read{Lossy: read.Lossy, Spills: read.Spills}.Delta(), "�")
	if len(loss) > metadataBytes {
		loss = appJob.Read{Lossy: true}.Delta()
	}
	envelope := withNewline(loss) + status
	body := appJob.Read{Stdout: read.Stdout, Stderr: read.Stderr}.Delta()
	if read.Result != "" {
		body = withNewline(body) + read.Result
	}
	if body == "" {
		body = "(no new output)"
	}
	body = boundedText(body, session.MaxTextBytes-len(envelope)-1, "\n[output truncated]")
	return withNewline(body) + envelope
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

package job

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// validNotice reports whether message is a user/message the agent accepts.
func validNotice(message session.Message) bool {
	return (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}).Validate() == nil
}

func TestNotice_FitsOneTextBlock(t *testing.T) {
	const (
		prefix = "background job bash-1 (bash: "
		suffix = ") finished [status: completed, exit code: 0]. Read its output with job_output."
		marker = "\n[notice truncated]\nDone; job_output."
	)
	view := func(label string) View {
		return View{ID: "bash-1", Kind: "bash", Label: label, Status: StatusCompleted, Detail: "exit code: 0"}
	}
	// A notice of exactly the block limit is delivered whole.
	exact := strings.Repeat("x", session.MaxTextBytes-len(prefix)-len(suffix))
	if text := session.Text(Notice(view(exact))); text != prefix+exact+suffix || !validNotice(Notice(view(exact))) {
		t.Fatalf("exact notice changed: %d bytes", len(text))
	}
	for name, label := range map[string]string{
		"one byte over": exact + "x",
		// Both parities of a two-byte rune against the cut point.
		"even runes":    strings.Repeat("é", 200<<10),
		"shifted runes": "x" + strings.Repeat("é", 200<<10),
	} {
		t.Run(name, func(t *testing.T) {
			message := Notice(view(label))
			text := session.Text(message)
			if !validNotice(message) || len(text) > session.MaxTextBytes || len(text) < session.MaxTextBytes-utf8.UTFMax || !utf8.ValidString(text) {
				t.Fatalf("notice is %d bytes, valid UTF-8 %v, accepted %v", len(text), utf8.ValidString(text), validNotice(message))
			}
			if !strings.HasPrefix(text, prefix+label[:16]) || !strings.HasSuffix(text, marker) {
				t.Fatalf("notice = %q...%q", text[:64], text[len(text)-64:])
			}
		})
	}
}

func TestService_UndeliveredNoticeIsVisibleInDetail(t *testing.T) {
	service, notifier, _ := startService(t)
	notifier.mu.Lock()
	notifier.err = errors.New("journal unavailable")
	notifier.mu.Unlock()
	producer := newGate()
	id, _ := launch(t, service, "root", producer)
	producer.release <- Outcome{Status: StatusCompleted, Detail: "exit code: 0"}
	<-notifier.sent
	want := "[status: completed, exit code: 0; completion notice not delivered: journal unavailable]"
	for deadline := time.Now().Add(10 * time.Second); ; {
		view, err := service.Get("root", id)
		if err != nil {
			t.Fatal(err)
		}
		if view.StatusLine() == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want %q", view.StatusLine(), want)
		}
		time.Sleep(time.Millisecond)
	}
	if texts := notifier.texts(); len(texts) != 1 || !strings.Contains(texts[0], "finished [status: completed, exit code: 0]. Read") {
		t.Fatalf("notices = %q", texts)
	}
}

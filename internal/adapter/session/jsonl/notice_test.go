package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

func jobNotice(id, text string) *coresession.Message {
	return &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "tool-jobs", NoticeID: id}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: text}}}
}

func queued(message *coresession.Message) coresession.Record {
	return coresession.Record{Type: coresession.RecordNoticeQueued, Message: message}
}

func delivered(turn uint64, message *coresession.Message) coresession.Record {
	return coresession.Record{Type: coresession.RecordUserMessage, Turn: turn, Message: message}
}

func owedIDs(events []coresession.Event) []string {
	var ids []string
	for _, message := range coresession.PendingNotices(events) {
		ids = append(ids, message.Source.NoticeID)
	}
	return ids
}

const (
	firstNoticeText  = "background job bash-1 (bash: make) finished [status: completed, exit code: 0]. Read its output with job_output."
	secondNoticeText = "background job bash-2 (bash: test) finished [status: failed, workspace sandbox is unavailable]. Read its output with job_output."
)

func TestSessionV2Notice_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-notice.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 14 || events[5].Record.Type != coresession.RecordNoticeQueued || coresession.NoticeID(events) != "notice-3" {
		t.Fatalf("events = %v", events)
	}
	if owed := owedIDs(events); len(owed) != 1 || owed[0] != "notice-2" {
		t.Fatalf("owed = %v", owed)
	}
	// Only the delivery is model-visible; a queued notice is not.
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 4 || surface[2].Message.Source.NoticeID != "notice-1" {
		t.Fatalf("surface = %+v, %v", surface, err)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatal("read and closed resume changed a committed notice fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header, nil); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("run it")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("waiting")},
		queued(jobNotice("notice-1", firstNoticeText)),
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		delivered(1, jobNotice("notice-1", firstNoticeText)),
		{Type: coresession.RecordStepStart, Turn: 1, Step: 2},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 2, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 2, Message: assistantMessage("noted")},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
		queued(jobNotice("notice-2", secondNoticeText)),
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 notice fixture:\n%s", actual)
	}
}

func TestSessionV2Notice_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-notice.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	const deliveredSource = `"source":{"kind":"tool-jobs","notice_id":"notice-1"}}}}` + "\n" + `{"seq":9`
	for _, test := range []struct{ name, from, to string }{
		{"unknown-source-field", `"notice_id":"notice-1"}}}}` + "\n" + `{"seq":7`, `"notice_id":"notice-1","job":"bash-1"}}}}` + "\n" + `{"seq":7`},
		{"queued-in-turn", `{"type":"notice/queued","message"`, `{"type":"notice/queued","turn":1,"message"`},
		{"queued-without-id", `"source":{"kind":"tool-jobs","notice_id":"notice-2"}`, `"source":{"kind":"tool-jobs"}`},
		{"duplicate-id", `"notice_id":"notice-2"`, `"notice_id":"notice-1"`},
		{"unowed-delivery", deliveredSource, `"source":{"kind":"tool-jobs","notice_id":"notice-7"}}}}` + "\n" + `{"seq":9`},
		{"changed-delivery", `"text":"background job bash-1 (bash: make) finished [status: completed, exit code: 0]. Read its output with job_output."}],"source":{"kind":"tool-jobs","notice_id":"notice-1"}}}}` + "\n" + `{"seq":9`, `"text":"background job bash-1 finished"}],"source":{"kind":"tool-jobs","notice_id":"notice-1"}}}}` + "\n" + `{"seq":9`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatalf("replacement %q did not change the fixture", test.from)
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("got %v, want %v", err, ErrCorruptSession)
			}
		})
	}
}

func TestValidateOrder_DeliversEachQueuedNoticeOnce(t *testing.T) {
	notice := jobNotice("notice-1", "done")
	open := addOrder(orderPrefix()[:2], queued(notice))
	for name, events := range map[string][]coresession.Event{
		"never queued":    addOrder(orderPrefix()[:2], delivered(1, notice)),
		"delivered twice": addOrder(addOrder(open, delivered(1, notice)), delivered(1, notice)),
		"different text":  addOrder(open, delivered(1, jobNotice("notice-1", "other"))),
		"different kind":  addOrder(open, delivered(1, &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "user", NoticeID: "notice-1"}, Content: notice.Content})),
		"queued twice":    addOrder(addOrder(open, delivered(1, notice)), queued(jobNotice("notice-1", "again"))),
	} {
		if _, err := validateOrder(events, false); !errors.Is(err, ErrCorruptSession) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// A notice may be queued inside a step or between turns, and delivered
	// in any later turn.
	inStep := addOrder(withAssistant(orderPrefix()), queued(notice))
	closed := addOrder(addOrder(inStep, coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 1}), coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCanceled})
	between := addOrder(closed, queued(jobNotice("notice-2", "later")))
	next := addOrder(addOrder(between, coresession.Record{Type: coresession.RecordTurnStart, Turn: 2}), delivered(2, jobNotice("notice-2", "later")))
	next = addOrder(next, delivered(2, notice))
	if _, err := validateOrder(next, false); err != nil {
		t.Fatal(err)
	}
	if owed := owedIDs(next); len(owed) != 0 {
		t.Fatalf("owed after delivery = %v", owed)
	}
}

func TestLog_OwedNoticeSurvivesInterruptedRepair(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "notice", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("work")})
	appendRecord(t, log, queued(jobNotice("notice-1", "done")))
	if _, err := log.Append(t.Context(), delivered(1, jobNotice("notice-2", "missing"))); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("unowed delivery = %v", err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The crash interrupted the turn before delivery: repair closes the
	// turn and the notice is still owed.
	resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: "notice", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	events, err := resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if last := events[len(events)-1].Record; last.Outcome != coresession.OutcomeInterrupted {
		t.Fatalf("repaired tail = %#v", last)
	}
	if owed := owedIDs(events); len(owed) != 1 || owed[0] != "notice-1" {
		t.Fatalf("owed after repair = %v", owed)
	}
	appendRecord(t, resumed, coresession.Record{Type: coresession.RecordTurnStart, Turn: 2})
	appendRecord(t, resumed, delivered(2, jobNotice("notice-1", "done")))
	if err := resumed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, events, err = manager.Inspect(t.Context(), "notice")
	if err != nil {
		t.Fatal(err)
	}
	if owed := owedIDs(events); len(owed) != 0 {
		t.Fatalf("owed after delivery = %v", owed)
	}
}

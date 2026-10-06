package session

import (
	"errors"
	"slices"
	"testing"
)

func noticeMessage(id, text string) *Message {
	return &Message{Role: RoleUser, Source: MessageSource{Kind: "tool-jobs", NoticeID: id}, Content: []ContentBlock{{Type: ContentText, Text: text}}}
}

func TestRecord_ValidatesQueuedNotices(t *testing.T) {
	valid := Record{Type: RecordNoticeQueued, Message: noticeMessage("notice-1", "job finished")}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid notice: %v", err)
	}
	assistant := noticeMessage("notice-1", "x")
	assistant.Role = RoleAssistant
	untrimmed := noticeMessage(" notice-1", "x")
	empty := noticeMessage("notice-1", "x")
	empty.Content = nil
	for name, record := range map[string]Record{
		"in a turn":       {Type: RecordNoticeQueued, Turn: 1, Message: noticeMessage("notice-1", "x")},
		"in a step":       {Type: RecordNoticeQueued, Step: 1, Message: noticeMessage("notice-1", "x")},
		"no message":      {Type: RecordNoticeQueued},
		"assistant":       {Type: RecordNoticeQueued, Message: assistant},
		"no notice ID":    {Type: RecordNoticeQueued, Message: noticeMessage("", "x")},
		"untrimmed ID":    {Type: RecordNoticeQueued, Message: untrimmed},
		"empty content":   {Type: RecordNoticeQueued, Message: empty},
		"unrelated field": {Type: RecordNoticeQueued, Message: noticeMessage("notice-1", "x"), Outcome: OutcomeCompleted},
	} {
		if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// A delivery is an ordinary user message that names its notice; only
	// user messages may.
	if err := (Record{Type: RecordUserMessage, Turn: 1, Message: noticeMessage("notice-1", "x")}).Validate(); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if err := (Record{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: assistant}).Validate(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("assistant notice ID = %v", err)
	}
}

func TestNotices_NumberAndFoldOwedNotices(t *testing.T) {
	events := []Event{
		{Sequence: 1, Record: Record{Type: RecordNoticeQueued, Message: noticeMessage("notice-1", "first")}},
		{Sequence: 2, Record: Record{Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Source: MessageSource{Kind: "user"}, Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}}},
		{Sequence: 3, Record: Record{Type: RecordNoticeQueued, Message: noticeMessage("notice-2", "second")}},
		{Sequence: 4, Record: Record{Type: RecordNoticeQueued, Message: noticeMessage("notice-3", "third")}},
		{Sequence: 5, Record: Record{Type: RecordUserMessage, Turn: 1, Message: noticeMessage("notice-2", "second")}},
		{Sequence: 6, Record: Record{Type: RecordUserMessage, Turn: 1, Message: noticeMessage("notice-9", "unknown")}},
	}
	if id := NoticeID(nil); id != "notice-1" {
		t.Fatalf("first ID = %q", id)
	}
	if id := NoticeID(events); id != "notice-4" {
		t.Fatalf("next ID = %q", id)
	}
	pending := PendingNotices(events)
	ids := make([]string, len(pending))
	for index, message := range pending {
		ids[index] = message.Source.NoticeID
	}
	if !slices.Equal(ids, []string{"notice-1", "notice-3"}) || Text(pending[1]) != "third" {
		t.Fatalf("pending = %#v", pending)
	}
	pending[0].Content[0].Text = "changed"
	if Text(*events[0].Record.Message) != "first" {
		t.Fatal("pending notices alias the log")
	}
	if pending := PendingNotices(events[:2]); len(pending) != 1 {
		t.Fatalf("prefix pending = %#v", pending)
	}
}

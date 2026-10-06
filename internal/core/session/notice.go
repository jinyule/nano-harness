package session

import "strconv"

// requireNotice checks a notice/queued record: a session-level fact outside
// any turn that carries the complete user message to deliver, identified
// by its notice ID.
func (record Record) requireNotice() error {
	if record.Turn != 0 || record.Step != 0 || record.Message == nil || record.Message.Role != RoleUser || record.Message.Source.NoticeID == "" || record.hasExtras("message") {
		return invalid("notice/queued shape is invalid")
	}
	return validateMessage(*record.Message, true)
}

// NoticeID returns the identity of the next notice a session queues after
// events: notices are numbered in queue order across the whole log, so a
// forked child continues after the notices it inherited.
func NoticeID(events []Event) string {
	queued := 0
	for _, event := range events {
		if event.Record.Type == RecordNoticeQueued {
			queued++
		}
	}
	return "notice-" + strconv.Itoa(queued+1)
}

// PendingNotices returns the notices events queued and did not deliver, in
// queue order. A user/message carrying a notice's ID delivers it. Callers
// pass the session's own events, so a fork never owes its parent's notices.
func PendingNotices(events []Event) []Message {
	var pending []Message
	for _, event := range events {
		record := event.Record
		switch {
		case record.Type == RecordNoticeQueued:
			pending = append(pending, *cloneMessage(record.Message))
		case record.Type == RecordUserMessage && record.Message.Source.NoticeID != "":
			for index := range pending {
				if pending[index].Source.NoticeID == record.Message.Source.NoticeID {
					pending = append(pending[:index], pending[index+1:]...)
					break
				}
			}
		}
	}
	return pending
}

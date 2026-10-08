package jsonl

import coresession "github.com/jinyule/nano-harness/internal/core/session"

// Fork seeds retain their original ownership. Only the child's own suffix
// is subject to its delegated policy; human switches belong to root sessions.
func validateSandboxOwner(header coresession.Header, events []coresession.Event) error {
	for _, event := range coresession.OwnEvents(events) {
		if event.Record.Sandbox != nil && (event.Record.Sandbox.Source == "delegation") != (header.ParentSessionID != "") {
			return orderError("sandbox/mode source does not match session ownership")
		}
	}
	return nil
}

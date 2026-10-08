package file

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type sandboxJournal struct {
	nopJournal
	events []session.Event
	err    error
}

func (journal *sandboxJournal) Events(context.Context) ([]session.Event, error) {
	return journal.events, journal.err
}
func (journal *sandboxJournal) set(t *testing.T, mode string) {
	t.Helper()
	var record session.Record
	if err := json.Unmarshal([]byte(`{"type":"sandbox/mode","sandbox":{"mode":"`+mode+`"}}`), &record); err != nil {
		t.Fatal(err)
	}
	journal.events = append(journal.events, session.Event{Sequence: uint64(len(journal.events) + 1), Record: record})
}

func TestProvider_SandboxApprovalMatrix(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		for _, standing := range []string{"read-only", "workspace-write", "danger-full-access"} {
			for _, requested := range []string{"", "workspace-write", "danger-full-access"} {
				for _, allowed := range []bool{false, true} {
					t.Run(tool+"/"+standing+"/"+requested+"/"+map[bool]string{false: "deny", true: "allow"}[allowed], func(t *testing.T) {
						h := newHarness(t)
						writeFixture(t, h.path("target"), "old")
						h.read(t, "target")
						journal := &sandboxJournal{}
						journal.set(t, standing)
						args := map[string]any{"file_path": "target", "content": "new"}
						if tool == "edit" {
							args = map[string]any{"file_path": "target", "old_string": "old", "new_string": "new"}
						}
						if requested != "" {
							args["sandbox_permissions"], args["justification"] = requested, "needs this operation"
						}
						if !allowed {
							h.approver.outcome = session.ApprovalRejected
						}
						encoded, _ := json.Marshal(args)
						result := h.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "session", Journal: journal, Calls: []session.ToolCall{{ID: "call", Name: tool, Arguments: encoded}}})[0]
						policyAllows := (standing != "read-only" || requested != "") && (standing != "danger-full-access" || requested != "workspace-write")
						want := allowed && policyAllows
						if result.IsError == want || (readFixture(t, h.path("target")) == "new") != want {
							t.Fatalf("result=%+v contents=%s", result, readFixture(t, h.path("target")))
						}
						if (len(h.approver.reasons) == 1) != policyAllows {
							t.Fatalf("approval count=%d policy=%v", len(h.approver.reasons), policyAllows)
						}
						if session.EffectiveSandbox(journal.events) != session.SandboxMode(standing) {
							t.Fatal("one-shot grant changed standing mode")
						}
					})
				}
			}
		}
	}
}

func TestProvider_ModeChangeDuringApprovalAndDirectDenial(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("target"), "old")
			h.read(t, "target")
			journal := &sandboxJournal{}
			journal.set(t, "workspace-write")
			h.approver.during = func() { journal.set(t, "read-only") }
			args := map[string]any{"file_path": "target", "content": "new"}
			if tool == "edit" {
				args = map[string]any{"file_path": "target", "old_string": "old", "new_string": "new"}
			}
			encoded, _ := json.Marshal(args)
			result := h.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "session", Journal: journal, Calls: []session.ToolCall{{ID: "call", Name: tool, Arguments: encoded}}})[0]
			if !result.IsError || !strings.Contains(result.Output, "read-only") || readFixture(t, h.path("target")) != "old" {
				t.Fatalf("changed during approval: %+v", result)
			}
			// Calling the executor directly with a grant does not bypass the
			// read-only mode the journal now holds; delegated refusal has its
			// own test, TestFileMutations_RefuseDelegatedCallsAtExecution.
			inv := appTool.Invocation{SessionID: "session", Approved: true, Journal: journal}
			var err error
			if tool == "write" {
				_, err = h.provider.write(t.Context(), inv, writeArgs{FilePath: "target", Content: "new"})
			} else {
				_, err = h.provider.edit(t.Context(), inv, editArgs{FilePath: "target", OldString: "old", NewString: "new"})
			}
			if err == nil || !strings.Contains(err.Error(), "file access denied under read-only") || readFixture(t, h.path("target")) != "old" {
				t.Fatalf("direct %s under read-only = %v", tool, err)
			}
		})
	}
	h := newHarness(t)
	failure := errors.New("journal failed")
	_, err := h.provider.write(t.Context(), appTool.Invocation{Approved: true, Journal: &sandboxJournal{err: failure}}, writeArgs{FilePath: "target"})
	if !errors.Is(err, failure) {
		t.Fatalf("journal failure lost: %v", err)
	}
	_, err = h.provider.write(t.Context(), appTool.Invocation{Approved: true}, writeArgs{FilePath: "target"})
	if err == nil {
		t.Fatal("missing journal authorized")
	}
}

func TestProvider_ReadOnlyEnforcedAtMutation(t *testing.T) {
	h := newHarness(t)
	journal := &sandboxJournal{}
	journal.set(t, "read-only")
	invocation := appTool.Invocation{SessionID: "root", Approved: true, Journal: journal}
	result, err := h.provider.write(t.Context(), invocation, writeArgs{FilePath: "blocked", Content: "x"})
	if err == nil || !strings.Contains(err.Error(), "file access denied under read-only") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(h.path("blocked")); !os.IsNotExist(err) {
		t.Fatalf("read-only created a file: %v", err)
	}
}

// TestFileMutations_RefuseDelegatedCallsAtExecution proves the execution
// point refuses delegated write and edit even with every other condition
// met: a session, a workspace-write journal, an approval grant, and a target
// the session has read. Nothing changes on disk and no observation moves.
func TestFileMutations_RefuseDelegatedCallsAtExecution(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("target"), "old")
			h.read(t, "target")
			prior, seen := h.provider.observed.lookup("session", h.path("target"))
			if !seen {
				t.Fatal("the read recorded no observation")
			}
			journal := &sandboxJournal{}
			journal.set(t, "workspace-write")
			invocation := appTool.Invocation{SessionID: "session", Approved: true, Delegated: true, Journal: journal}
			var errs []error
			if tool == "write" {
				for _, path := range []string{"target", "fresh"} {
					_, err := h.provider.write(t.Context(), invocation, writeArgs{FilePath: path, Content: "new"})
					errs = append(errs, err)
				}
			} else {
				_, err := h.provider.edit(t.Context(), invocation, editArgs{FilePath: "target", OldString: "old", NewString: "new"})
				errs = append(errs, err)
			}
			for _, err := range errs {
				if err == nil || err.Error() != "subagents cannot obtain file approval" {
					t.Fatalf("delegated %s = %v", tool, err)
				}
			}
			if readFixture(t, h.path("target")) != "old" {
				t.Fatal("a delegated mutation changed the file")
			}
			if _, err := os.Stat(h.path("fresh")); !os.IsNotExist(err) {
				t.Fatalf("a delegated write created a file: %v", err)
			}
			if after, _ := h.provider.observed.lookup("session", h.path("target")); after != prior {
				t.Fatal("a delegated mutation moved the observation")
			}
			if _, recorded := h.provider.observed.lookup("session", h.path("fresh")); recorded {
				t.Fatal("a delegated write recorded an observation")
			}
		})
	}
}

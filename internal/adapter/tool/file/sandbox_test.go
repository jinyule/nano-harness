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
			inv := appTool.Invocation{Approved: true, Delegated: true, Journal: journal}
			if tool == "write" {
				_, err := h.provider.write(t.Context(), inv, writeArgs{FilePath: "target"})
				if err == nil {
					t.Fatal("delegated write authorized directly")
				}
			} else {
				_, err := h.provider.edit(t.Context(), inv, editArgs{FilePath: "target"})
				if err == nil {
					t.Fatal("delegated edit authorized directly")
				}
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

package shell

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

func TestOutcome_ReadOnlyRunnerFailureUsesLaunchMode(t *testing.T) {
	for _, code := range []int{0, 1} {
		got := outcome(platformProcess.Result{RunnerFailed: true, ExitCode: code, SandboxMode: platformProcess.ModeReadOnly}, platformProcess.ErrSandboxUnavailable)
		base := "killed before exit; "
		if code > 0 {
			base = "exit code: 1; "
		}
		if got.Status != appJob.StatusFailed || got.Detail != base+strings.Replace(runnerFailedNote, "workspace-write", "read-only", 1) {
			t.Fatalf("read-only runner failure: %+v", got)
		}
	}
}

type modeJournal struct {
	nopJournal
	events []session.Event
	err    error
}

func (journal *modeJournal) Events(context.Context) ([]session.Event, error) {
	return journal.events, journal.err
}
func (journal *modeJournal) set(mode session.SandboxMode) {
	journal.events = append(journal.events, session.Event{Record: session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: mode}}})
}

func TestBash_SandboxApprovalMatrix(t *testing.T) {
	for _, standing := range []session.SandboxMode{session.SandboxReadOnly, session.SandboxWorkspaceWrite, session.SandboxDangerFullAccess} {
		for _, target := range []string{"", "workspace-write", "danger-full-access"} {
			for _, allowed := range []bool{false, true} {
				t.Run(string(standing)+"/"+target+"/"+map[bool]string{true: "allow", false: "deny"}[allowed], func(t *testing.T) {
					runner := &fakeRunner{}
					h := newHarness(t, runner)
					journal := &modeJournal{}
					journal.set(standing)
					args := map[string]any{"description": "List files", "command": "ls"}
					if target != "" {
						args["sandbox_permissions"], args["justification"] = target, "needs this command"
					}
					if !allowed {
						h.approver.outcome = session.ApprovalRejected
					}
					encoded, _ := json.Marshal(args)
					result := h.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "session", Journal: journal, Calls: []session.ToolCall{{ID: "call", Name: "bash", Arguments: encoded}}})[0]
					policyAllows := standing != session.SandboxDangerFullAccess || target != "workspace-write"
					want := policyAllows && allowed
					if result.IsError == want || (runner.count() == 1) != want {
						t.Fatalf("result=%+v executions=%d", result, runner.count())
					}
					if want {
						mode := standing
						if target != "" {
							mode = session.SandboxMode(target)
						}
						profile := map[session.SandboxMode]platformProcess.Mode{session.SandboxReadOnly: platformProcess.ModeReadOnly, session.SandboxWorkspaceWrite: platformProcess.ModeWorkspace, session.SandboxDangerFullAccess: platformProcess.ModeHost}[mode]
						if runner.last().Mode != profile {
							t.Fatalf("profile=%s want=%s", runner.last().Mode, profile)
						}
					}
					if (len(h.approver.reasons) == 1) != policyAllows {
						t.Fatalf("approvals=%v", h.approver.reasons)
					}
				})
			}
		}
	}
}

func TestBash_ExecutionPolicyAndReadOnlyMarkers(t *testing.T) {
	runner := &fakeRunner{result: platformProcess.Result{SandboxDenied: true, ExitCode: 1}}
	h := newHarness(t, runner)
	journal := &modeJournal{}
	journal.set(session.SandboxReadOnly)
	inv := appTool.Invocation{SessionID: "session", Approved: true, Journal: journal}
	args := bashArgs{Description: "List files", Command: "ls"}
	result, err := h.provider.bash(t.Context(), inv, args)
	if err != nil || !strings.Contains(result.Text, "denied under read-only mode") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if detail := outcome(platformProcess.Result{ExitCode: 1, SandboxDenied: true, SandboxMode: platformProcess.ModeReadOnly}, nil).Detail; !strings.Contains(detail, "read-only mode") {
		t.Fatalf("background marker: %s", detail)
	}
	args.SandboxPermissions = new("workspace-write")
	if _, err := h.provider.bash(t.Context(), inv, args); err == nil {
		t.Fatal("narrow grant accepted without justification")
	}
	journal.err = errors.New("log unavailable")
	if _, err := h.provider.bash(t.Context(), inv, args); !errors.Is(err, journal.err) {
		t.Fatalf("policy error: %v", err)
	}
	journal.err = nil
	journal.set(session.SandboxDangerFullAccess)
	args.SandboxPermissions = new("danger-full-access")
	if result, err := h.provider.bash(t.Context(), inv, args); err != nil || result.Text == "" || runner.last().Mode != platformProcess.ModeHost {
		t.Fatalf("standing host repeat: %+v %v", result, err)
	}
	inv.Delegated = true
	if _, err := h.provider.bash(t.Context(), inv, args); err == nil {
		t.Fatal("delegated host authorized")
	}
}

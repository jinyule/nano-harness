package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func code(err error) Code {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func TestService_ValidatesLifecycleAndRequests(t *testing.T) {
	h := startHarness(t)
	for _, test := range []struct {
		registry   *agent.Registry
		jobs       *job.Service
		repository bool
	}{{nil, h.jobs, true}, {h.registry, nil, true}, {h.registry, h.jobs, false}} {
		var err error
		if test.repository {
			_, err = New(test.registry, test.jobs, h.manager)
		} else {
			_, err = New(test.registry, test.jobs, nil)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", test, err)
		}
	}
	if h.service.ID() != "subagents" {
		t.Fatalf("ID = %q", h.service.ID())
	}
	if err := h.service.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double Start = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	failed, _ := New(h.registry, h.jobs, h.manager)
	if err := failed.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope Start = %v", err)
	}

	journal := &memoryJournal{}
	valid := StartRequest{ParentID: "root", Journal: journal, Turn: 1, Step: 1, Description: "scan", Prompt: "scan the tree"}
	for name, test := range map[string]struct {
		mutate func(*StartRequest)
		want   string
	}{
		"journal":     {func(request *StartRequest) { request.Journal = nil }, "subagent delegation requires a calling session journal"},
		"description": {func(request *StartRequest) { request.Description = " \n" }, "invalid description: expected a non-empty string"},
		"prompt":      {func(request *StartRequest) { request.Prompt = "  " }, "invalid prompt: expected a non-empty string"},
	} {
		request := valid
		test.mutate(&request)
		if _, err := h.service.Run(context.Background(), request); code(err) != CodeInvalidRequest || err.Error() != test.want {
			t.Errorf("%s: Run = %v", name, err)
		}
	}
	missing := valid
	missing.ParentID = "missing"
	if _, err := h.service.StartContinuable(context.Background(), missing); !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("missing parent = %v", err)
	}
	if len(journal.records) != 0 {
		t.Fatalf("rejected requests recorded %#v", journal.records)
	}

	if err := h.serviceScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.StartBackground(context.Background(), valid); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped StartBackground = %v", err)
	}
	if _, err := h.service.ListChildren(context.Background(), "root"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped ListChildren = %v", err)
	}
	if _, err := h.service.ListDescendants(context.Background(), "root"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped ListDescendants = %v", err)
	}
	if _, err := h.service.List(""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped List = %v", err)
	}
	if err := h.service.SendMessage(context.Background(), "root", "child", "hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped SendMessage = %v", err)
	}
	if err := h.service.Interrupt("root", "child"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Interrupt = %v", err)
	}
}

func TestCheckStart_TruncatesLabelOnRuneBoundary(t *testing.T) {
	label, err := checkStart(StartRequest{Journal: &memoryJournal{}, Description: " " + strings.Repeat("a", 126) + "中文", Prompt: "p"})
	if err != nil || label != strings.Repeat("a", 126) {
		t.Fatalf("label = %q, %v", label, err)
	}
	label, _ = checkStart(StartRequest{Journal: &memoryJournal{}, Description: strings.Repeat("b", 127) + " 中", Prompt: "p"})
	if label != strings.Repeat("b", 127) {
		t.Fatalf("trailing space label = %q", label)
	}
}

func TestMessages_RenderUpstreamWording(t *testing.T) {
	task := taskMessage("do it", "session-1", true)
	if len(task.Content) != 2 || task.Content[0].Text != "do it" || task.Source.Kind != SourceDelegation ||
		task.Content[1].Text != `Your parent agent id is "session-1". Before you finish, send your result to that agent with send_message({ agent_id: "session-1", message: "<self-contained result>" }). The parent shares your workspace but does not automatically receive your transcript, tool output, or reasoning. Send earlier messages as well when a finding changes what the parent should do next; sending a message does not end your turn.` {
		t.Fatalf("continuable task = %#v", task)
	}
	if oneShot := taskMessage("do it", "session-1", false); len(oneShot.Content) != 1 {
		t.Fatalf("one-shot task = %#v", oneShot)
	}
	if relayed := agentMessage("root", ""); len(relayed.Content) != 1 || relayed.Content[0].Text != "Agent root sent a message: " || relayed.Source.Kind != SourceAgentMessage {
		t.Fatalf("empty relayed message = %#v", relayed)
	}
	for outcome, want := range map[session.TurnOutcome]string{
		session.OutcomeCompleted:   "Background subagent c finished and will do no further work unless you send it more.",
		session.OutcomeCanceled:    "Background subagent c was stopped before it finished.",
		session.OutcomeInterrupted: "Background subagent c was stopped before it finished.",
		session.OutcomeError:       "Background subagent c failed before it finished.",
		session.OutcomeMaxTokens:   "Background subagent c ended abnormally (max_tokens) before it finished.",
		session.OutcomeStepLimit:   "Background subagent c ended abnormally (step_limit) before it finished.",
	} {
		if got := settlementSummary("c", outcome); got != want {
			t.Errorf("%s: %q", outcome, got)
		}
	}
	silent := settlementMessage("c", session.OutcomeCompleted, "")
	spoken := settlementMessage("c", session.OutcomeError, "partial")
	if session.Text(silent) != settlementSummary("c", session.OutcomeCompleted)+"It left no closing message." || len(spoken.Content) != 3 || spoken.Content[1].Text != "Its closing message:" || spoken.Source.Kind != SourceSettled {
		t.Fatalf("settlement messages = %#v %#v", silent, spoken)
	}
}

func TestJobOutcome_MapsRunEndings(t *testing.T) {
	for name, test := range map[string]struct {
		report Report
		err    error
		want   job.Outcome
	}{
		"cancelled":    {err: context.Canceled, want: job.Outcome{Status: job.StatusKilled}},
		"deadline":     {err: errors.Join(context.DeadlineExceeded, errors.New("close")), want: job.Outcome{Status: job.StatusKilled}},
		"failure":      {err: errors.New("broken"), want: job.Outcome{Status: job.StatusFailed, Detail: "broken"}},
		"completed":    {report: Report{Outcome: session.OutcomeCompleted, Text: "answer"}, want: job.Outcome{Status: job.StatusCompleted, Result: "answer"}},
		"canceled":     {report: Report{Outcome: session.OutcomeCanceled}, want: job.Outcome{Status: job.StatusKilled}},
		"interrupted":  {report: Report{Outcome: session.OutcomeInterrupted}, want: job.Outcome{Status: job.StatusKilled}},
		"error":        {report: Report{Outcome: session.OutcomeError}, want: job.Outcome{Status: job.StatusFailed, Detail: "error"}},
		"output limit": {report: Report{Outcome: session.OutcomeMaxTokens}, want: job.Outcome{Status: job.StatusFailed, Detail: "max_tokens"}},
		"step limit":   {report: Report{Outcome: session.OutcomeStepLimit}, want: job.Outcome{Status: job.StatusFailed, Detail: "step_limit"}},
	} {
		if got := jobOutcome(test.report, test.err); got != test.want {
			t.Errorf("%s: %#v", name, got)
		}
	}
}

type memoryJournal struct {
	records []session.Record
	err     error
}

func (journal *memoryJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	if journal.err != nil {
		return session.Event{}, journal.err
	}
	journal.records = append(journal.records, record)
	return session.Event{Sequence: uint64(len(journal.records)), Record: record}, nil
}

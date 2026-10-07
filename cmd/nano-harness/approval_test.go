package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// scopedApproval preserves the real composition and exposes only the
// approval Scope, so stopping it does not cancel the agent's turn context.
type scopedApproval struct {
	*approval.Service
	scope *plugin.Scope
}

func (service *scopedApproval) Start(ctx context.Context, scope *plugin.Scope) error {
	service.scope = scope
	return service.Service.Start(ctx, scope)
}

type lateApprovalBroker struct{ entered chan struct{} }

func (broker lateApprovalBroker) Ask(ctx context.Context, _ approval.Question) session.ApprovalOutcome {
	close(broker.entered)
	<-ctx.Done()
	return session.ApprovalAllowedOnce
}

func TestComposition_ApprovalCleanupRejectsLateConsent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	server, seen := scriptedModel(t, []modelStep{
		{tool: "write", arguments: `{"file_path":"blocked.txt","content":"must not be written"}`},
		{text: "denied"},
	})
	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "approval-cleanup", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	approvalPlugin := &scopedApproval{Service: app.approval}
	for index, candidate := range app.plugins {
		if candidate == app.approval {
			app.plugins[index] = approvalPlugin
		}
	}
	runtime, err := plugin.New(app.plugins...)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	brokerScope := &plugin.Scope{}
	t.Cleanup(func() { _ = brokerScope.Close(context.Background()) })
	broker := lateApprovalBroker{entered: make(chan struct{})}
	if err := app.approval.RegisterBroker(broker, brokerScope); err != nil {
		t.Fatal(err)
	}
	controller, err := app.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	turn, err := controller.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "write a file"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-broker.entered
	if err := approvalPlugin.scope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result := <-turn; result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "denied" {
		t.Fatalf("approval cleanup affected the live turn: %+v", result)
	}
	assembled := &assembledApp{runtime: runtime, transcript: filepath.Join(data, "sessions", "approval-cleanup.jsonl")}
	records := assembled.records(t)
	var asked, decided *session.ApprovalData
	for _, record := range records {
		if record.Type == session.RecordApprovalAsked {
			asked = record.Approval
		}
		if record.Type == session.RecordApprovalDecided {
			decided = record.Approval
		}
	}
	if asked == nil || decided == nil || asked.ID != decided.ID || decided.Outcome != session.ApprovalCancelled || decided.Source != "cancellation" {
		t.Fatalf("cleanup returned without durable cancellation: asked=%+v decided=%+v", asked, decided)
	}
	if _, err := os.Stat(filepath.Join(root, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("approval cleanup authorized a real write: %v", err)
	}
	results := orderedToolResults(records)
	if len(results) != 1 || !results[0].IsError || results[0].Output != "Error: approval cancelled" {
		t.Fatalf("late consent reached the tool: %+v", results)
	}
	if t.Context().Err() != nil || len(seen()) != 2 {
		t.Fatal("the caller did not remain live after approval cleanup")
	}
}

func TestComposition_ApprovalAfterRestartReachesTheOperator(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	server, seen := scriptedModel(t, []modelStep{
		{tool: "write", arguments: `{"file_path":"first.txt","content":"one"}`},
		{text: "first done"},
		{tool: "write", arguments: `{"file_path":"second.txt","content":"two"}`},
		{text: "second done"},
	})
	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(data, "sessions", "approval-restart.jsonl")
	// Each pass builds the whole composition from configuration, as a new
	// process does; the second pass resumes the transcript the first wrote.
	run := func(text string) (agent.TurnResult, *assembledApp) {
		t.Helper()
		config, err := normalizeConfig(applicationConfig{
			workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
			credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
			agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "approval-restart", maxSteps: 8,
		})
		if err != nil {
			t.Fatal(err)
		}
		app, err := composeApplication(config, dependencies{httpClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := plugin.New(append(app.plugins, &approvingFrontend{app: app})...)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
		controller, err := app.root.Agent()
		if err != nil {
			t.Fatal(err)
		}
		assembled := &assembledApp{app: app, root: controller, runtime: runtime, transcript: transcript}
		return assembled.turn(t, text), assembled
	}
	if result, first := run("write the first file"); result.Err != nil || result.Text != "first done" {
		t.Fatalf("first process turn = %+v", result)
	} else {
		first.records(t)
	}
	result, second := run("write the second file")
	if result.Err != nil || result.Text != "second done" {
		t.Fatalf("turn after restart = %+v", result)
	}
	records := second.records(t)
	var asked, decided []*session.ApprovalData
	for _, record := range records {
		if record.Type == session.RecordApprovalAsked {
			asked = append(asked, record.Approval)
		}
		if record.Type == session.RecordApprovalDecided {
			decided = append(decided, record.Approval)
		}
	}
	if len(asked) != 2 || len(decided) != 2 || asked[0].ID == asked[1].ID {
		var failures []string
		for _, result := range orderedToolResults(records) {
			if result.IsError {
				failures = append(failures, result.Output)
			}
		}
		t.Fatalf("approvals across restart: %d asked, %d decided, failed results %q", len(asked), len(decided), failures)
	}
	for index, question := range asked {
		if decided[index].ID != question.ID || decided[index].Outcome != session.ApprovalAllowedOnce || decided[index].Source != "operator" {
			t.Fatalf("decision %d = %+v for question %+v", index, decided[index], question)
		}
	}
	for _, name := range []string{"first.txt", "second.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("approved write %s: %v", name, err)
		}
	}
	if results := orderedToolResults(records); len(results) != 2 || results[0].IsError || results[1].IsError {
		t.Fatalf("tool results = %+v", results)
	}
	if len(seen()) != 4 {
		t.Fatalf("model requests = %d", len(seen()))
	}
}

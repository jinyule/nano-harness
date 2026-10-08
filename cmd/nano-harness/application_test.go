package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyule/nano-harness/internal/adapter/tui"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// This frontend exercises the shared composition without linking UI behavior to Tea.
type probeFrontend struct {
	app        *application
	events     <-chan session.Event
	controller agent.Controller
	closed     bool
}

func (*probeFrontend) ID() string { return "probe-frontend" }
func (*probeFrontend) Ask(context.Context, approval.Question) session.ApprovalOutcome {
	return session.ApprovalRejected
}
func (frontend *probeFrontend) Start(ctx context.Context, scope *plugin.Scope) error {
	controller, err := frontend.app.root.Agent()
	if err != nil {
		return err
	}
	frontend.controller = controller
	events, dispose, err := controller.Subscribe(1)
	if err != nil {
		return err
	}
	frontend.events = events
	if err := scope.Defer(func(context.Context) error {
		dispose()
		// Dependencies must still be active during frontend teardown.
		_, err := controller.Events(ctx)
		frontend.closed = err == nil
		return err
	}); err != nil {
		dispose()
		return err
	}
	return frontend.app.approval.RegisterBroker(frontend, scope)
}

func TestApplication_SupportsIndependentFrontendPlugin(t *testing.T) {
	previous := newTerminal
	t.Cleanup(func() { newTerminal = previous })
	newTerminal = func(tui.Config) (*tui.App, error) { t.Fatal("shared application constructed TUI"); return nil, nil }
	root, private := t.TempDir(), t.TempDir()
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(private, "sessions"), spillRoot: filepath.Join(t.TempDir(), "spill"), attachmentRoot: filepath.Join(t.TempDir(), "attachments"),
		settingsPath: filepath.Join(private, "settings.yaml"), credentialPath: filepath.Join(private, "credentials.yaml"),
		skillsDir: filepath.Join(root, "skills"), agentsSkillsDir: filepath.Join(root, "agents-skills"),
		sessionID: "frontend-probe", maxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	frontend := &probeFrontend{app: app}
	runtime, err := plugin.New(append(app.plugins, frontend)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if err := app.registry.SetPolicy(t.Context(), "frontend-probe", session.ApprovalNever); err != nil {
		t.Fatal(err)
	}
	event := <-frontend.events
	if event.Record.Type != session.RecordApprovalPolicy {
		t.Fatalf("frontend saw %s", event.Record.Type)
	}
	if frontend.controller.Status().SessionID != "frontend-probe" {
		t.Fatal("frontend used a different session")
	}
	if err := runtime.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !frontend.closed {
		t.Fatal("frontend stopped after its dependencies")
	}
	if _, open := <-frontend.events; open {
		t.Fatal("frontend subscription survived shutdown")
	}
}

// allowFrontend grants every approval so the real tool guards, not the
// approval policy, decide what happens.
type allowFrontend struct{ app *application }

func (*allowFrontend) ID() string { return "allow-frontend" }
func (*allowFrontend) Ask(context.Context, approval.Question) session.ApprovalOutcome {
	return session.ApprovalAllowedOnce
}
func (frontend *allowFrontend) Start(_ context.Context, scope *plugin.Scope) error {
	return frontend.app.approval.RegisterBroker(frontend, scope)
}

// functionCalls renders one Responses stream that issues the given calls.
func functionCalls(calls ...[2]string) string {
	var stream strings.Builder
	for index, call := range calls {
		arguments, _ := json.Marshal(call[1])
		item := fmt.Sprintf(`{"type":"function_call","call_id":"call-%d-%s","name":%q,"arguments":%s}`, index, call[0], call[0], arguments)
		fmt.Fprintf(&stream, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
	}
	return stream.String()
}

var spillLocator = regexp.MustCompile(`Full grep result stored at: (\S+?-grep-results\.txt)\. Use read`)

// TestComposition_SpillsResultsAndGuardsWritesEndToEnd drives the real shared
// composition with a scripted model: a blind overwrite is refused, a capped
// grep saves its complete result, read opens the artifact outside the
// workspace, an edit after a read succeeds, and an oversized result becomes
// a preview with a locator.
func TestComposition_SpillsResultsAndGuardsWritesEndToEnd(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	var step atomic.Int32
	failures := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		switch step.Add(1) {
		case 1:
			_, _ = io.WriteString(writer, functionCalls([2]string{"write", `{"file_path":"proof.txt","content":"clobbered"}`}))
		case 2:
			_, _ = io.WriteString(writer, functionCalls([2]string{"grep", `{"pattern":"needle","path":"many.txt"}`}))
		case 3:
			match := spillLocator.FindSubmatch(body)
			if match == nil {
				failures <- "the grep result carried no locator"
				_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"no locator\"}\n\n")
				break
			}
			_, _ = io.WriteString(writer, functionCalls(
				[2]string{"read", fmt.Sprintf(`{"file_path":%q,"limit":2}`, match[1])},
				[2]string{"read", `{"file_path":"proof.txt"}`},
			))
		case 4:
			_, _ = io.WriteString(writer, functionCalls([2]string{"edit", `{"file_path":"proof.txt","old_string":"evidence","new_string":"edited"}`}))
		case 5:
			_, _ = io.WriteString(writer, functionCalls([2]string{"grep", `{"pattern":"wide","path":"wide.txt"}`}))
		default:
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
	}))
	t.Cleanup(server.Close)

	root, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof.txt"), []byte("evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(strings.Repeat("needle\n", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	// 250 inline matches of 1000-byte lines exceed the inline token budget.
	if err := os.WriteFile(filepath.Join(root, "wide.txt"), []byte(strings.Repeat("wide"+strings.Repeat("x", 996)+"\n", 260)), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 200000\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"),
		settingsPath: settingsPath, credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-spill", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := plugin.New(append(app.plugins, &allowFrontend{app: app})...)
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
	results, err := controller.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "done" {
		t.Fatalf("turn = %#v", result)
	}
	if err := runtime.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}

	// The outside world: the edit landed, the blind write did not.
	if content, _ := os.ReadFile(filepath.Join(root, "proof.txt")); string(content) != "edited\n" { //nolint:gosec // test workspace path
		t.Fatalf("proof.txt = %q", content)
	}
	transcript, err := os.ReadFile(filepath.Join(data, "sessions", "session-spill.jsonl")) //nolint:gosec // test session path
	if err != nil {
		t.Fatal(err)
	}
	notRead := fmt.Sprintf("Error: cannot modify %q: file has not been read — read the file, then retry", filepath.Join(config.workspaceRoot, "proof.txt"))
	encodedNotRead, _ := json.Marshal(notRead)
	match := spillLocator.FindSubmatch(transcript)
	if !bytes.Contains(transcript, encodedNotRead[1:len(encodedNotRead)-1]) || match == nil {
		t.Fatalf("transcript lacks the refusal or the locator")
	}
	locator := string(match[1])
	partition := filepath.Join(config.spillRoot, "workspace-")
	info, err := os.Stat(locator) //nolint:gosec // the locator comes from this test's transcript and is checked against the spill root below
	if err != nil || !strings.HasPrefix(locator, partition) || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact %s: %v %v", locator, info, err)
	}
	artifact, _ := os.ReadFile(locator) //nolint:gosec // the locator was checked to lie in the test spill root
	if !strings.HasPrefix(string(artifact), "Found 300 matches\n\nmany.txt\nLine 1: needle\n") || !strings.HasSuffix(string(artifact), "Line 300: needle") {
		t.Fatalf("artifact = %q", artifact[:min(len(artifact), 80)])
	}
	for _, fact := range []string{
		"Found 250 of 300 matches",
		`1: Found 300 matches\n2: \n\n(Showing lines 1-2 of 303. Use offset=3 to continue.)`,
		"Read an existing file before overwriting it with write (the default fs-observation-policy requires it) and prefer edit for targeted changes.",
		"has been updated successfully.",
		"Found 250 of 260 matches",
		"\\n\\n[...]\\n\\n",
		"Full formatted result stored at: " + partition,
		"-grep.txt. Use read with offset/limit, or grep this path to search within it.)",
	} {
		if !bytes.Contains(transcript, []byte(fact)) {
			t.Errorf("transcript lacks %q", fact)
		}
	}
}

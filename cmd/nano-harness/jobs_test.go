package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/approval"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// approvingFrontend grants every one-shot approval so bash can run.
type approvingFrontend struct{ app *application }

func (*approvingFrontend) ID() string { return "approving-frontend" }
func (*approvingFrontend) Ask(context.Context, approval.Question) session.ApprovalOutcome {
	return session.ApprovalAllowedOnce
}
func (frontend *approvingFrontend) Start(_ context.Context, scope *plugin.Scope) error {
	return frontend.app.approval.RegisterBroker(frontend, scope)
}

// scriptedCall is one function call a scripted model response emits.
type scriptedCall struct {
	name      string
	arguments map[string]any
}

// writeCalls streams Responses SSE for function calls, or text when none.
func writeCalls(writer io.Writer, step int, text string, calls []scriptedCall) {
	for index, call := range calls {
		arguments, _ := json.Marshal(call.arguments)
		item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call-%d-%d", step, index), "name": call.name, "arguments": string(arguments)})
		_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
	}
	if len(calls) == 0 {
		delta, _ := json.Marshal(text)
		_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", delta)
	}
	_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
}

// TestComposition_BackgroundJobsEndToEnd drives the real composition and
// host processes: bash starts two background jobs, job_list and job_kill
// control them, job_output waits and reads, and the second job's
// completion wakes the idle root agent with a durable tool-jobs notice.
func TestComposition_BackgroundJobsEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	host := func(arguments map[string]any) map[string]any {
		arguments["sandbox_permissions"], arguments["justification"] = "danger-full-access", "e2e host job"
		return arguments
	}
	trapped := `trap 'printf cleaned > term-cleanup; trap - TERM; kill -TERM $$' TERM; printf one; touch printed; while :; do :; done`
	// Files order the steps causally: the foreground bash call returns only
	// after bash-1 printed, and bash-2 finishes only after turn one.
	script := [][]scriptedCall{
		{
			{"bash", host(map[string]any{"description": "Print then hold", "command": trapped, "run_in_background": true})},
			{"bash", host(map[string]any{"description": "Wait for notify", "command": "while [ ! -e notify ]; do sleep 0.05; done; printf three", "run_in_background": true})},
		},
		{{"bash", host(map[string]any{"description": "Wait for print", "command": "while [ ! -e printed ]; do sleep 0.01; done"})}, {"job_list", map[string]any{}}},
		{
			{"job_kill", map[string]any{"job_id": "bash-1", "reason": "not needed"}},
			{"job_output", map[string]any{"job_id": "bash-1", "wait": true, "timeout_ms": 10000}},
			{"job_output", map[string]any{"job_id": "bash-2", "wait": true, "timeout_ms": 50}},
		},
		nil,
		{{"job_output", map[string]any{"job_id": "bash-2"}}},
		nil,
	}
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		step := len(bodies)
		bodies = append(bodies, string(body))
		mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer test-key" || step >= len(script) {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeCalls(writer, step, fmt.Sprintf("answer %d", step), script[step])
	}))
	t.Cleanup(server.Close)

	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-jobs", maxSteps: 8,
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
	rootAgent, err := app.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "run jobs"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Text != "answer 3" {
		t.Fatalf("first turn = %+v", result)
	}
	// bash-2 finishes only now, while the agent is idle; its notice opens
	// a second turn.
	if err := os.WriteFile(filepath.Join(root, "notify"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); rootAgent.Status().Last.Turn < 2 || rootAgent.Status().Busy; {
		if time.Now().After(deadline) {
			t.Fatalf("notice did not open a turn: %+v", rootAgent.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last := rootAgent.Status().Last; last.Text != "answer 5" || last.Outcome != session.OutcomeCompleted {
		t.Fatalf("notice turn = %+v", last)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	outputs, notices := map[string]string{}, []string{}
	file, err := os.Open(filepath.Join(data, "sessions", "session-jobs.jsonl")) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 8<<20)
	for scanner.Scan() {
		var event session.Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		record := event.Record
		if record.Type == session.RecordToolResult {
			outputs[record.Result.CallID] = record.Result.Output
		}
		if record.Type == session.RecordUserMessage && record.Message.Source.Kind == appJob.NoticeSource {
			notices = append(notices, fmt.Sprintf("turn %d: %s", record.Turn, session.Text(*record.Message)))
		}
	}
	for callID, want := range map[string]string{
		"call-0-0": "started background job bash-1",
		"call-0-1": "started background job bash-2",
		"call-1-0": "(no output)",
		"call-2-0": "requested cancellation of job bash-1",
		"call-2-1": "one\n[status: killed, signal: SIGTERM; not needed]",
		"call-2-2": "(no new output)\n[status: running]",
		"call-4-0": "three\n[status: completed, exit code: 0]",
	} {
		if outputs[callID] != want {
			t.Errorf("%s = %q, want %q", callID, outputs[callID], want)
		}
	}
	if list := outputs["call-1-1"]; list != "bash-1 [bash] running — "+trapped+"\nbash-2 [bash] running — while [ ! -e notify ]; do sleep 0.05; done; printf three" {
		t.Errorf("job_list = %q", list)
	}
	if data, err := os.ReadFile(filepath.Join(root, "term-cleanup")); err != nil || string(data) != "cleaned" { //nolint:gosec // rooted in this test's private workspace
		t.Fatalf("assembled job_kill did not run TERM trap: %q, %v", data, err)
	}
	// Only bash-2 notifies: bash-1 was killed by the model.
	want := "turn 2: background job bash-2 (bash: while [ ! -e notify ]; do sleep 0.05; done; printf three) finished [status: completed, exit code: 0]. Read its output with job_output."
	if len(notices) != 1 || notices[0] != want {
		t.Fatalf("notices = %q", notices)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 6 || !strings.Contains(bodies[4], "background job bash-2 (bash:") || !bytes.Contains([]byte(bodies[0]), []byte(`"name":"job_output"`)) {
		t.Fatalf("provider requests = %d, notice visible = %v", len(bodies), len(bodies) == 6 && strings.Contains(bodies[4], "background job bash-2 (bash:"))
	}
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// shutdownModel plays a root agent and one continuable child. The root's
// first step starts the child and a foreground bash heartbeat; any later
// root step would call bash again, which after tool withdrawal would yield
// an unknown-tool result. The child's first step blocks until its request
// is cancelled, so the child is mid-turn when shutdown begins.
type shutdownModel struct {
	mu           sync.Mutex
	rootRequests int
	childStarted chan struct{}
	childEnded   chan struct{}
	once         sync.Once
}

func (model *shutdownModel) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Input []map[string]any `json:"input"`
	}
	raw, _ := io.ReadAll(request.Body)
	if json.Unmarshal(raw, &body) != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	child := false
	for _, item := range body.Input {
		if item["role"] == "user" && strings.Contains(fmt.Sprint(item["content"]), "CHILD_TASK") {
			child = true
		}
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	if child {
		model.once.Do(func() { close(model.childStarted) })
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		close(model.childEnded)
		return
	}
	model.mu.Lock()
	model.rootRequests++
	step := model.rootRequests
	model.mu.Unlock()
	host := func(arguments map[string]any) map[string]any {
		arguments["sandbox_permissions"], arguments["justification"] = "danger-full-access", "shutdown test"
		return arguments
	}
	heartbeat := `printf %s $$ > pid; while :; do date > "$TMPDIR/beat" || : > lost; sleep 0.02; done`
	calls := []scriptedCall{{"bash", host(map[string]any{"description": "Late call", "command": "true"})}}
	if step == 1 {
		calls = []scriptedCall{
			{"subagent", map[string]any{"description": "Blocked child", "prompt": "CHILD_TASK"}},
			{"bash", host(map[string]any{"description": "Heartbeat in TMPDIR", "command": heartbeat, "timeoutMs": 60000})},
		}
	}
	writeCalls(writer, step, "", calls)
}

// TestComposition_ShutdownQuiescesAgentsBeforeToolsAndTemporaryFiles proves
// the shutdown order through the real composition: closing the runtime
// cancels the root's foreground bash and the child's model request and
// waits for both before tools are withdrawn and the shell temporary
// directory is removed, and no agent issues another model request.
func TestComposition_ShutdownQuiescesAgentsBeforeToolsAndTemporaryFiles(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := &shutdownModel{childStarted: make(chan struct{}), childEnded: make(chan struct{})}
	server := httptest.NewServer(model)
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
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-shutdown", maxSteps: 8,
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
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "start work"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-model.childStarted
	pidPath, beatPath := filepath.Join(root, "pid"), ""
	for deadline := time.Now().Add(20 * time.Second); beatPath == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat never started")
		}
		matches, _ := filepath.Glob(filepath.Join(root, ".nano-harness-tmp-*", "beat"))
		if _, err := os.Stat(pidPath); err == nil && len(matches) == 1 {
			beatPath = matches[0]
		}
	}
	shutdown, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	if err := runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	select {
	case <-model.childEnded:
	default:
		t.Fatal("shutdown returned before the child's in-flight request was cancelled")
	}
	if result := <-results; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("root turn = %+v", result)
	}
	model.mu.Lock()
	requests := model.rootRequests
	model.mu.Unlock()
	if requests != 1 {
		t.Fatalf("root issued %d model requests; shutdown must not let a turn continue", requests)
	}
	// The heartbeat writes into TMPDIR until it is killed; a removed TMPDIR
	// while it still ran would have left the lost marker in the workspace.
	if _, err := os.Stat(filepath.Join(root, "lost")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the shell temporary directory was removed while the foreground command still ran")
	}
	if _, err := os.Stat(filepath.Dir(beatPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell temporary directory survived shutdown: %v", err)
	}
	pidText, err := os.ReadFile(pidPath) //nolint:gosec // the path is inside this test's temporary workspace
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidText))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("heartbeat process %d is still alive: %v", pid, err)
	}

	outputs, outcomes := rootRecords(t, filepath.Join(data, "sessions", "session-shutdown.jsonl"))
	if outputs["call-1-1"] != "Error: tool call aborted" || !strings.HasPrefix(outputs["call-1-0"], "started subagent ") {
		t.Fatalf("root tool results = %#v", outputs)
	}
	for callID, output := range outputs {
		if strings.Contains(output, "unknown tool") {
			t.Fatalf("%s saw a withdrawn tool: %q", callID, output)
		}
	}
	if len(outcomes) != 1 || outcomes[0] != session.OutcomeCanceled {
		t.Fatalf("root turn outcomes = %v", outcomes)
	}
}

// rootRecords reads tool outputs by call ID and turn outcomes from a
// closed transcript.
func rootRecords(t *testing.T, path string) (map[string]string, []session.TurnOutcome) {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	outputs := map[string]string{}
	var outcomes []session.TurnOutcome
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 8<<20)
	for scanner.Scan() {
		var event session.Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Record.Type == session.RecordToolResult {
			outputs[event.Record.Result.CallID] = event.Record.Result.Output
		}
		if event.Record.Type == session.RecordTurnEnd {
			outcomes = append(outcomes, event.Record.Outcome)
		}
	}
	return outputs, outcomes
}

// TestComposition_StartOrderEncodesShutdownQuiescence pins the start order
// that reverse cleanup relies on: the goal driver, root, and registry start
// last, so goal rounds stop and every agent closes before any tool, job,
// delegation service, spill store, or session resource is torn down; jobs
// start after shell tools so background processes end before the shell
// temporary directory is removed.
func TestComposition_StartOrderEncodesShutdownQuiescence(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"),
		settingsPath: filepath.Join(data, "settings.yaml"), credentialPath: filepath.Join(data, "credentials.yaml"),
		skillsDir: filepath.Join(data, "skills"), agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "order", maxSteps: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(app.plugins))
	position := map[string]int{}
	for index, candidate := range app.plugins {
		ids[index] = candidate.ID()
		position[candidate.ID()] = index
	}
	last := len(ids) - 1
	if ids[last] != "goal-driver" || ids[last-1] != "root-agent" || ids[last-2] != "agents" {
		t.Fatalf("agent tier must start last: %v", ids)
	}
	if position["jobs"] < position["shell-tools"] {
		t.Fatalf("jobs must start after shell tools: %v", ids)
	}
	// The attachment store outlives every request image read and image write.
	if position["attachments"] > position["llm"] || position["attachments"] > position["fs-tools"] {
		t.Fatalf("attachments must start before the LLM runtime and the file tools: %v", ids)
	}
	tools := 0
	for _, id := range ids {
		if strings.HasSuffix(id, "-tools") {
			tools++
		}
	}
	if tools < 10 {
		t.Fatalf("tool providers are no longer identifiable by ID: %v", ids)
	}
}

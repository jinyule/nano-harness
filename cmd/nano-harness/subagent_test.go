package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const subagentRoot = "session-sub"

// runtimeContextPrefix opens the runtime-context snapshot a delegated child
// receives before its first step.
const runtimeContextPrefix = "Current runtime context."

// subagentModel plays every agent of one composition on a loopback
// Responses endpoint. Notices can land in any turn, so the root follows a
// state machine over the tool outputs already in its history; children are
// routed by their latest user message. Gates order concurrent agents.
type subagentModel struct {
	mu       sync.Mutex
	childID  string
	requests []string
	gateOpen bool
	// childGate holds the continuable child's first step until the root
	// has listed it and sent it a message.
	childGate chan struct{}
	// resumed is closed when the cold-resumed child's request arrives, so
	// the root interrupts a turn that is really running.
	resumed     chan struct{}
	resumedOnce sync.Once
}

var forkDescription = "  " + strings.Repeat("审阅", 40) + " \n"

var startedPattern = regexp.MustCompile(`started subagent (\S+)`)

func (model *subagentModel) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Input []map[string]any `json:"input"`
	}
	raw, _ := io.ReadAll(request.Body)
	if request.URL.Path != "/v1/responses" || json.Unmarshal(raw, &body) != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	var users, outputs []string
	latestOutputs := 0
	for _, item := range body.Input {
		if item["role"] == "user" {
			var text strings.Builder
			for _, block := range item["content"].([]any) {
				text.WriteString(block.(map[string]any)["text"].(string))
			}
			// A delegated child's runtime context is harness input, not a task.
			if !strings.HasPrefix(text.String(), runtimeContextPrefix) {
				users, latestOutputs = append(users, text.String()), 0
			}
		}
		if item["type"] == "function_call_output" {
			outputs, latestOutputs = append(outputs, item["output"].(string)), latestOutputs+1
		}
	}
	history, latest := strings.Join(outputs, "\n"), users[len(users)-1]
	asked := func(text string) bool { return slices.Contains(users, text) }
	model.mu.Lock()
	model.requests = append(model.requests, string(raw))
	index := len(model.requests)
	if match := startedPattern.FindStringSubmatch(history); match != nil && model.childID == "" {
		model.childID = match[1]
	}
	child := model.childID
	model.mu.Unlock()
	writer.Header().Set("Content-Type", "text/event-stream")
	text, calls := "noted", []scriptedCall(nil)
	wait := func(gate <-chan struct{}) bool {
		select {
		case <-gate:
			return true
		case <-request.Context().Done():
			return false
		}
	}
	switch {
	case strings.HasPrefix(latest, "FORK_TASK"):
		// A fork sees the completed first turn but not the in-flight one.
		text = "fork lacks context"
		if asked("FIRST") && strings.Contains(string(raw), "first answer") && !asked("SECOND") {
			text = "fork answer"
		}
	case strings.HasPrefix(latest, "FORK_BG"):
		text = "bg answer"
	case strings.HasPrefix(users[0], "CHILD_TASK"):
		switch {
		case strings.HasSuffix(latest, "sent a message: AGAIN"):
			model.resumedOnce.Do(func() { close(model.resumed) })
			<-request.Context().Done()
			return
		case strings.HasSuffix(latest, "sent a message: PING"):
			text = "child report after PING"
		case latestOutputs == 0:
			if !wait(model.childGate) {
				return
			}
			calls = []scriptedCall{{"send_message", map[string]any{"agent_id": subagentRoot, "message": "UP"}}}
		}
	case !asked("SECOND"):
		text = "first answer"
	case !strings.Contains(history, "started subagent"):
		calls = []scriptedCall{
			{"subagent", map[string]any{"description": "worker", "prompt": "CHILD_TASK"}},
			{"subagent_fork", map[string]any{"description": forkDescription, "prompt": "FORK_TASK"}},
		}
	case !strings.Contains(history, "[running]"):
		calls = []scriptedCall{{"list_agents", map[string]any{}}, {"list_agents", map[string]any{"scope": "descendants"}}}
	case !strings.Contains(history, "message delivered to agent"):
		calls = []scriptedCall{{"send_message", map[string]any{"agent_id": child, "message": "PING"}}}
	case !asked("THIRD"):
		model.mu.Lock()
		if !model.gateOpen {
			model.gateOpen = true
			close(model.childGate)
		}
		model.mu.Unlock()
	case !strings.Contains(history, "started background subagent job"):
		calls = []scriptedCall{
			{"send_message", map[string]any{"agent_id": child, "message": "AGAIN"}},
			{"subagent_fork", map[string]any{"description": "bg review", "prompt": "FORK_BG", "run_in_background": true}},
		}
	case !strings.Contains(history, "interrupt requested"):
		if !wait(model.resumed) {
			return
		}
		calls = []scriptedCall{{"interrupt_agent", map[string]any{"agent_id": child}}}
	case slices.ContainsFunc(users, func(user string) bool { return strings.HasPrefix(user, "background job subagent-1") }) && !strings.Contains(history, "bg answer"):
		calls = []scriptedCall{{"job_output", map[string]any{"job_id": "subagent-1"}}}
	}
	writeCalls(writer, index, text, calls)
}

// TestComposition_SubagentsEndToEnd drives the real composition: a
// background continuable child and a foreground fork start in one step,
// list_agents reports the working child, send_message crosses the edge in
// both directions, the child's settlement notifies the root, a later
// message cold-resumes it, interrupt_agent stops it, and a background fork
// job hands its answer to job_output. Assertions read the transcripts.
func TestComposition_SubagentsEndToEnd(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := &subagentModel{childGate: make(chan struct{}), resumed: make(chan struct{})}
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: subagentRoot, maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := plugin.New(app.plugins...)
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
	transcript := func(id string) []session.Record {
		events := readTranscript(t, filepath.Join(data, "sessions", id+".jsonl"))
		records := make([]session.Record, len(events))
		for index, event := range events {
			records[index] = event.Record
		}
		return records
	}
	notices := func(records []session.Record, kind string) []string {
		var texts []string
		for _, record := range records {
			if record.Type == session.RecordUserMessage && record.Message.Source.Kind == kind {
				texts = append(texts, session.Text(*record.Message))
			}
		}
		return texts
	}
	// settle waits until the root is idle, no child is live, and the root
	// holds the expected number of settlement notices.
	settle := func(settled int) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			live, _ := app.subagents.List("")
			if len(live) == 0 && !rootAgent.Status().Busy && rootAgent.Status().Pending == 0 && len(notices(transcript(subagentRoot), appSubagent.SourceSettled)) >= settled {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := rootAgent.WhenIdle(ctx)
				cancel()
				if err == nil {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("subagents did not settle: live=%v root=%+v", live, rootAgent.Status())
			}
		}
	}
	submit := func(text string) {
		t.Helper()
		results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
		if err != nil {
			t.Fatal(err)
		}
		if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
			t.Fatalf("%s turn = %+v", text, result)
		}
	}
	submit("FIRST")
	submit("SECOND")
	settle(1)
	model.mu.Lock()
	child := model.childID
	model.mu.Unlock()

	records := transcript(subagentRoot)
	outputs := func(records []session.Record) []string {
		var texts []string
		for _, record := range records {
			if record.Type == session.RecordToolResult {
				texts = append(texts, record.Result.Output)
			}
		}
		return texts
	}
	for _, output := range []string{
		"started subagent " + child,
		"fork answer",
		child + " [running] — worker",
		child + " [running] parent=" + subagentRoot + " depth=1 — worker",
		"message delivered to agent " + child,
	} {
		if !slices.Contains(outputs(records), output) {
			t.Errorf("root tool results %q lack %q", outputs(records), output)
		}
	}
	if relayed := notices(records, appSubagent.SourceAgentMessage); len(relayed) != 1 || relayed[0] != "Agent "+child+" sent a message: UP" {
		t.Errorf("root relayed = %q", relayed)
	}
	settled := notices(records, appSubagent.SourceSettled)
	if len(settled) != 1 || settled[0] != "Background subagent "+child+" finished and will do no further work unless you send it more.Its closing message:child report after PING" {
		t.Errorf("root settlement = %q", settled)
	}
	// The two delegations ran concurrently in one step, so their catalog
	// records may commit in either order; identify them by label.
	children := map[string]session.SubagentCatalog{}
	for _, entry := range session.Children(readTranscript(t, filepath.Join(data, "sessions", subagentRoot+".jsonl"))) {
		children[entry.Label] = entry
	}
	worker, review := children["worker"], children[forkDescription]
	if len(children) != 2 || worker.SessionID != child || worker.Mode != session.SubagentContinuable || review.Mode != session.SubagentOneShot || review.SessionID == "" {
		t.Fatalf("root catalog = %#v", children)
	}
	reviewOwn := session.OwnEvents(readTranscript(t, filepath.Join(data, "sessions", review.SessionID+".jsonl")))
	if reviewOwn[0].Record.Subagent.Label != forkDescription {
		t.Errorf("fork descriptor label = %q", reviewOwn[0].Record.Subagent.Label)
	}
	childRecords := transcript(child)
	if childRecords[0].Subagent.Provider != session.SubagentSpawn || childRecords[1].Approval.Policy != session.ApprovalNever {
		t.Errorf("child creation = %#v %#v", childRecords[0], childRecords[1])
	}
	if relayed := notices(childRecords, appSubagent.SourceAgentMessage); len(relayed) != 1 || relayed[0] != "Agent "+subagentRoot+" sent a message: PING" {
		t.Errorf("child relayed = %q", relayed)
	}
	if tasks := notices(childRecords, appSubagent.SourceDelegation); len(tasks) != 1 || !strings.Contains(tasks[0], `Your parent agent id is "`+subagentRoot+`".`) {
		t.Errorf("child task = %q", tasks)
	}
	// The child inherits the parent's route, receives its delegation scope as
	// runtime context, and keeps the parent's system prompt unchanged.
	var rootHeader, childHeader *session.RequestHeader
	for _, record := range records {
		if record.Type == session.RecordRequestHeader {
			rootHeader = record.Header
		}
	}
	for _, record := range childRecords {
		if record.Type == session.RecordRequestHeader && childHeader == nil {
			childHeader = record.Header
		}
		if record.Type == session.RecordUserMessage && record.Message.Source.Kind == appSubagent.SourceAgentMessage && record.Message.Source.SenderSessionID != subagentRoot {
			t.Errorf("child message sender = %q", record.Message.Source.SenderSessionID)
		}
	}
	if route := childRecords[0].Subagent.Route; route != (session.SubagentRoute{Provider: rootHeader.Provider, Model: rootHeader.Model, Effort: rootHeader.Effort}) {
		t.Errorf("inherited route = %#v, root request %s/%s %q", route, rootHeader.Provider, rootHeader.Model, rootHeader.Effort)
	}
	if childHeader == nil || childHeader.System != rootHeader.System || childHeader.Provider != rootHeader.Provider || childHeader.Model != rootHeader.Model {
		t.Errorf("child request header differs from the root's: %+v", childHeader)
	}
	if contexts := notices(childRecords, appSubagent.SourceRuntimeContext); len(contexts) != 2 || !strings.Contains(contexts[0], "Current DSH file policy:") || !strings.Contains(contexts[1], "do not retry the denied operation") {
		t.Errorf("child runtime context = %q", contexts)
	}
	forkEvents := readTranscript(t, filepath.Join(data, "sessions", review.SessionID+".jsonl"))
	own := session.OwnEvents(forkEvents)
	if descriptor := own[0].Record.Subagent; descriptor == nil || descriptor.Provider != session.SubagentFork || descriptor.Inherited == 0 || forkEvents[0].Record.Type != session.RecordApprovalPolicy {
		t.Errorf("fork transcript starts %#v, own %#v", forkEvents[0], own[0])
	}
	parentEvents := readTranscript(t, filepath.Join(data, "sessions", subagentRoot+".jsonl"))
	inherited := own[0].Record.Subagent.Inherited
	parentPrefix, _ := json.Marshal(parentEvents[:inherited])
	forkPrefix, _ := json.Marshal(forkEvents[:inherited])
	if string(parentPrefix) != string(forkPrefix) {
		t.Fatal("sandbox context changed the inherited fork prefix")
	}
	var ownRecords []session.Record
	for _, event := range own {
		ownRecords = append(ownRecords, event.Record)
		if event.Record.Header != nil && event.Record.Header.System != rootHeader.System {
			t.Error("fork system prompt differs from its parent's")
		}
	}
	if contexts := notices(ownRecords, appSubagent.SourceRuntimeContext); len(contexts) != 1 || !strings.Contains(contexts[0], "do not retry the denied operation") {
		t.Errorf("fork must retain its inherited sandbox snapshot and add delegation context: %q", contexts)
	}

	// A later message cold-resumes the settled child; the root interrupts it.
	submit("THIRD")
	settle(2)
	records = transcript(subagentRoot)
	for _, output := range []string{
		"started background subagent job subagent-1",
		"interrupt requested for agent " + child,
		"bg answer\n[status: completed]",
	} {
		if !slices.Contains(outputs(records), output) {
			t.Errorf("root tool results %q lack %q", outputs(records), output)
		}
	}
	if delivered := slices.DeleteFunc(outputs(records), func(output string) bool { return output != "message delivered to agent "+child }); len(delivered) != 2 {
		t.Errorf("deliveries = %q", delivered)
	}
	settled = notices(records, appSubagent.SourceSettled)
	if len(settled) != 2 || settled[1] != "Background subagent "+child+" was stopped before it finished.It left no closing message." {
		t.Errorf("interrupted settlement = %q", settled)
	}
	if outcome, _ := session.LastOutcome(readTranscript(t, filepath.Join(data, "sessions", child+".jsonl"))); outcome != session.OutcomeCanceled {
		t.Errorf("resumed child outcome = %q", outcome)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	locks, _ := filepath.Glob(filepath.Join(data, "sessions", "*.lock"))
	if len(locks) != 0 {
		t.Fatalf("locks left after shutdown: %v", locks)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if !slices.ContainsFunc(model.requests, func(body string) bool { return strings.Contains(body, `"name":"send_message"`) }) {
		t.Fatal("provider never saw the send_message definition")
	}
}

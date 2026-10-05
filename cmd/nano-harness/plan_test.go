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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/question"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// modelStep is one scripted Responses turn: a tool call or final text.
type modelStep struct {
	tool      string
	arguments string
	text      string
}

// seenRequest is the part of one provider request the tests inspect.
type seenRequest struct {
	Instructions string            `json:"instructions"`
	Input        []json.RawMessage `json:"input"`
}

// scriptedModel serves steps in order over loopback Responses SSE.
func scriptedModel(t *testing.T, steps []modelStep) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body seenRequest
		if request.URL.Path != "/v1/responses" || json.NewDecoder(request.Body).Decode(&body) != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		index := len(seen)
		seen = append(seen, body)
		mu.Unlock()
		if index >= len(steps) {
			http.Error(writer, "script exhausted", http.StatusBadRequest)
			return
		}
		step := steps[index]
		writer.Header().Set("Content-Type", "text/event-stream")
		if step.tool != "" {
			item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": fmt.Sprintf("call-%d", index), "name": step.tool, "arguments": step.arguments})
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":%s}\n\n", item)
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
		} else {
			delta, _ := json.Marshal(step.text)
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", delta)
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
	}))
	t.Cleanup(server.Close)
	return server, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

// answeringFrontend stands in for the terminal: it registers a question
// broker on the shared composition and answers from a script.
type answeringFrontend struct {
	app     *application
	mu      sync.Mutex
	answers [][]question.Answer
	seen    []question.Request
	ask     func(context.Context, question.Request) ([]question.Answer, error)
}

func (*answeringFrontend) ID() string { return "answering-frontend" }

func (frontend *answeringFrontend) Start(_ context.Context, scope *plugin.Scope) error {
	return frontend.app.questions.RegisterBroker(frontend, scope)
}

func (frontend *answeringFrontend) Ask(ctx context.Context, request question.Request) ([]question.Answer, error) {
	frontend.mu.Lock()
	defer frontend.mu.Unlock()
	frontend.seen = append(frontend.seen, request)
	if frontend.ask != nil {
		return frontend.ask(ctx, request)
	}
	if len(frontend.answers) == 0 {
		return nil, question.ErrCancelled
	}
	answers := frontend.answers[0]
	frontend.answers = frontend.answers[1:]
	return answers, nil
}

func TestComposition_CancelledPlanReviewCannotScheduleExit(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "exit_plan_mode", arguments: `{"plan":"# Plan\n\n- implement"}`},
		{text: "still planning"},
	})
	if _, err := assembled.app.registry.SetPlanMode(t.Context(), "session-plan", true); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	assembled.frontend.ask = func(ctx context.Context, _ question.Request) ([]question.Answer, error) {
		close(entered)
		<-ctx.Done()
		return []question.Answer{{ID: "plan-review", Selected: []string{"Approve"}}}, nil
	}
	turn, err := assembled.root.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "review the plan"}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal("review did not reach the broker")
	}
	assembled.root.Interrupt()
	if result := <-turn; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("cancelled turn = %+v", result)
	}
	if result := assembled.turn(t, "continue planning"); result.Err != nil || result.Text != "still planning" {
		t.Fatalf("next turn = %+v", result)
	}
	requests := seen()
	if len(requests) != 2 || !strings.Contains(requests[1].Instructions, upstreamSection(t, "plan:policy")) {
		t.Error("cancelled approval removed the next request's plan policy")
	}
	records := assembled.records(t)
	results := orderedToolResults(records)
	if len(results) != 1 || !results[0].IsError || results[0].Output != "Error: "+question.ErrAborted.Error() {
		t.Errorf("cancelled review result = %+v", results)
	}
	for _, record := range records {
		if record.Type == session.RecordPlanMode && !record.Plan.Active {
			t.Error("cancelled approval committed an exit at the next boundary")
		}
	}
}

type assembledApp struct {
	app        *application
	frontend   *answeringFrontend
	root       agent.Controller
	runtime    *plugin.Runtime
	transcript string
}

// startAssembled runs the real shared composition against a scripted model,
// with the answering frontend in place of the terminal.
func startAssembled(t *testing.T, steps []modelStep, answers ...[]question.Answer) (*assembledApp, func() []seenRequest) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	server, seen := scriptedModel(t, steps)
	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-plan", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	frontend := &answeringFrontend{app: app, answers: answers}
	runtime, err := plugin.New(append(app.plugins, frontend)...)
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
	return &assembledApp{app: app, frontend: frontend, root: controller, runtime: runtime, transcript: filepath.Join(data, "sessions", "session-plan.jsonl")}, seen
}

func (assembled *assembledApp) turn(t *testing.T, text string) agent.TurnResult {
	t.Helper()
	results, err := assembled.root.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	return <-results
}

// records shuts the composition down and decodes the durable transcript.
func (assembled *assembledApp) records(t *testing.T) []session.Record {
	t.Helper()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(assembled.transcript)
	if err != nil {
		t.Fatal(err)
	}
	var records []session.Record
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var entry struct {
			Record session.Record `json:"record"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Record.Type != "" {
			records = append(records, entry.Record)
		}
	}
	return records
}

func orderedToolResults(records []session.Record) []session.ToolResult {
	var results []session.ToolResult
	for _, record := range records {
		if record.Type == session.RecordToolResult {
			results = append(results, *record.Result)
		}
	}
	return results
}

func TestComposition_AskUserQuestionReturnsTheUserAnswer(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "ask_user_question", arguments: `{"questions":[{"id":"mode","question":"Which mode?","header":"Choose Mode","options":[{"label":"Fast (Recommended)","description":"Less work"},{"label":"Thorough"}]}]}`},
		{text: "done"},
	}, []question.Answer{{ID: "mode", Selected: []string{"Thorough"}}})
	if result := assembled.turn(t, "ask me"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	request := assembled.frontend.seen[0]
	if request.SessionID != "session-plan" || request.CallID != "call-0" || request.Questions[0].Header != "Choose Mode" || request.Questions[0].Options[1].Label != "Thorough" {
		t.Fatalf("broker request = %+v", request)
	}
	const answer = `{"answers":[{"id":"mode","selected":["Thorough"]}]}`
	results := orderedToolResults(assembled.records(t))
	if len(results) != 1 || results[0].IsError || results[0].Output != answer {
		t.Fatalf("results = %+v", results)
	}
	requests := seen()
	if len(requests) != 2 || !strings.Contains(string(requests[1].Input[len(requests[1].Input)-1]), `\"selected\":[\"Thorough\"]`) {
		t.Fatalf("answer did not reach the model: %s", requests[len(requests)-1].Input)
	}
}

func TestComposition_PlanModeReviewKeepsPlanningThenApproves(t *testing.T) {
	const proposal = `{"plan":"# Cache plan\n\n- add a cache"}`
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "exit_plan_mode", arguments: proposal},
		{tool: "exit_plan_mode", arguments: proposal},
		{text: "implementing"},
		{tool: "exit_plan_mode", arguments: proposal},
		{text: "default mode"},
	},
		[]question.Answer{{ID: "plan-review", Custom: "add tests"}},
		[]question.Answer{{ID: "plan-review", Selected: []string{"Approve"}}},
	)
	if change, err := assembled.app.registry.SetPlanMode(t.Context(), "session-plan", true); err != nil || change != plan.Committed {
		t.Fatalf("enter = %s, %v", change, err)
	}
	if result := assembled.turn(t, "plan the cache"); result.Err != nil || result.Text != "implementing" {
		t.Fatalf("planning turn = %+v", result)
	}
	if result := assembled.turn(t, "exit again"); result.Err != nil || result.Text != "default mode" {
		t.Fatalf("default turn = %+v", result)
	}
	review := assembled.frontend.seen[0].Questions[0]
	if len(assembled.frontend.seen) != 2 || review.ID != "plan-review" || review.Detail != "# Cache plan\n\n- add a cache" || review.Intent == nil || review.Intent.Approve != "Approve" {
		t.Fatalf("review requests = %+v", assembled.frontend.seen)
	}
	section := upstreamSection(t, "plan:policy")
	requests := seen()
	if len(requests) != 5 {
		t.Fatalf("model requests = %d", len(requests))
	}
	for index, inPlan := range []bool{true, true, false, false, false} {
		if strings.Contains(requests[index].Instructions, section) != inPlan {
			t.Errorf("request %d plan policy = %t, want %t", index, !inPlan, inPlan)
		}
	}
	records := assembled.records(t)
	results := orderedToolResults(records)
	want := []string{
		"Error: The user chose to keep planning; their feedback: add tests",
		"Plan approved — plan mode exited; carry out the plan starting with your next step.",
		"Error: exit_plan_mode is only available in plan mode",
	}
	if len(results) != len(want) {
		t.Fatalf("results = %+v", results)
	}
	for index, text := range want {
		if results[index].Output != text || results[index].IsError != strings.HasPrefix(text, "Error: ") {
			t.Errorf("result %d = %+v, want %q", index, results[index], text)
		}
	}
	var modes []string
	for index, record := range records {
		if record.Type == session.RecordPlanMode {
			modes = append(modes, fmt.Sprintf("%t@turn%d before %s", record.Plan.Active, record.Turn, records[index+1].Type))
		}
	}
	if strings.Join(modes, ",") != "true@turn0 before turn/start,false@turn1 before step/start" {
		t.Fatalf("plan/mode records = %v", modes)
	}
}

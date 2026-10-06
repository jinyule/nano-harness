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
	"strings"
	"sync"
	"testing"
	"time"

	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// goalStep answers one model request; hold, when set, keeps the response
// open until it is closed or the request is cancelled.
type goalStep struct {
	tool      string
	arguments func(body string) string
	text      string
	hold      chan struct{}
}

func fixed(arguments string) func(string) string { return func(string) string { return arguments } }

var goalIDPattern = regexp.MustCompile(`goal-[0-9a-f]{32}`)

// withGoalID fills the goal ID the model last saw in a tool result into a
// template; round prompts carry only the objective.
func withGoalID(template string) func(string) string {
	return func(body string) string {
		ids := goalIDPattern.FindAllString(body, -1)
		return fmt.Sprintf(template, ids[len(ids)-1])
	}
}

type goalModel struct {
	server  *httptest.Server
	mu      sync.Mutex
	steps   []goalStep
	bodies  []string
	started chan int
}

// newGoalModel serves scripted steps over loopback Responses SSE and
// records every request body.
func newGoalModel(t *testing.T, steps ...goalStep) *goalModel {
	t.Helper()
	model := &goalModel{steps: steps, started: make(chan int, 64)}
	model.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		model.mu.Lock()
		index := len(model.bodies)
		model.bodies = append(model.bodies, string(raw))
		model.mu.Unlock()
		model.started <- index
		if index >= len(model.steps) {
			http.Error(writer, "script exhausted", http.StatusBadRequest)
			return
		}
		step := model.steps[index]
		if step.hold != nil {
			select {
			case <-step.hold:
			case <-request.Context().Done():
				return
			}
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		if step.tool != "" {
			item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": fmt.Sprintf("call-%d", index), "name": step.tool, "arguments": step.arguments(string(raw))})
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":%s}\n\n", item)
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
		} else {
			delta, _ := json.Marshal(step.text)
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", delta)
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
	}))
	t.Cleanup(model.server.Close)
	return model
}

func (model *goalModel) requests() []string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]string(nil), model.bodies...)
}

// awaitRequest waits until the model has received request index.
func (model *goalModel) awaitRequest(t *testing.T, index int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case seen := <-model.started:
			if seen >= index {
				return
			}
		case <-deadline:
			t.Fatalf("model never received request %d", index)
		}
	}
}

type goalApp struct {
	app        *application
	runtime    *plugin.Runtime
	transcript string
}

func goalConfig(t *testing.T, serverURL, data string) applicationConfig {
	t.Helper()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", serverURL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: t.TempDir(), sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-goal", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// startGoalApp runs the real shared composition, goal service and driver
// included, against the scripted model.
func startGoalApp(t *testing.T, model *goalModel, config applicationConfig) *goalApp {
	t.Helper()
	app, err := composeApplication(config, dependencies{httpClient: model.server.Client()})
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
	return &goalApp{app: app, runtime: runtime, transcript: filepath.Join(config.sessionRoot, "session-goal.jsonl")}
}

func (assembled *goalApp) say(t *testing.T, text string) {
	t.Helper()
	root, err := assembled.app.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	results, err := root.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil {
		t.Fatalf("turn = %+v", result)
	}
}

// awaitGoal polls the goal until done accepts it; the driver runs rounds
// asynchronously.
func (assembled *goalApp) awaitGoal(t *testing.T, done func(*appGoal.View) bool) *appGoal.View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		view, err := assembled.app.goals.Get(t.Context(), "session-goal")
		if err == nil && done(view) {
			root, _ := assembled.app.root.Agent()
			if err := root.WhenIdle(t.Context()); err != nil {
				t.Fatal(err)
			}
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("goal never reached the expected state: %+v, %v", view, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (assembled *goalApp) stop(t *testing.T) []session.Event {
	t.Helper()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	return readTranscript(t, assembled.transcript)
}

// trace summarizes the goal facts and turn boundaries of a transcript.
func trace(events []session.Event) []string {
	var lines []string
	for _, event := range events {
		record := event.Record
		switch {
		case record.Type == session.RecordGoalChange && record.Goal.Operation == session.GoalOpClear:
			lines = append(lines, "goal:clear")
		case record.Type == session.RecordGoalChange:
			line := fmt.Sprintf("goal:%s r%d %s %d/%d", record.Goal.Operation, record.Goal.Snapshot.Revision, record.Goal.Snapshot.Phase, record.Goal.RoundsStarted, record.Goal.Snapshot.MaxRounds)
			if reason := record.Goal.Snapshot.BlockedReason; reason != nil {
				line += " " + reason.Code
			}
			lines = append(lines, line)
		case record.Type == session.RecordUserMessage && record.Message.Source.Kind != "runtime-context":
			source := record.Message.Source
			if source.Kind == session.GoalSource {
				lines = append(lines, fmt.Sprintf("turn%d round %d@r%d", record.Turn, source.GoalRound, source.GoalRevision))
			} else {
				lines = append(lines, fmt.Sprintf("turn%d %s", record.Turn, source.Kind))
			}
		case record.Type == session.RecordToolResult:
			lines = append(lines, "result "+record.Result.Output)
		case record.Type == session.RecordTurnEnd:
			lines = append(lines, fmt.Sprintf("turn%d %s", record.Turn, record.Outcome))
		}
	}
	return lines
}

func upstreamSection(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "upstream-base-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Sections []struct {
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"prompt_sections"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, section := range document.Sections {
		if section.Name == name {
			return section.Text
		}
	}
	t.Fatalf("no prompt section %q", name)
	return ""
}

func expectTrace(t *testing.T, events []session.Event, want []string) {
	t.Helper()
	if got := trace(events); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestComposition_GoalCreatedByTheModelRunsARoundAndCompletes proves that a
// goal created from a person's request is continued by the driver in an
// automatic round, that the round may complete it, and that the model then
// receives the closing instruction and the driver stops.
func TestComposition_GoalCreatedByTheModelRunsARoundAndCompletes(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := newGoalModel(t,
		goalStep{tool: "create_goal", arguments: fixed(`{"objective":"keep docs green","max_goal_rounds":3}`)},
		goalStep{text: "goal set"},
		goalStep{tool: "get_goal", arguments: fixed(`{}`)},
		goalStep{tool: "update_goal", arguments: withGoalID(`{"goal_id":%q,"revision":1,"action":"complete"}`)},
		goalStep{text: "all docs gates are green"},
	)
	assembled := startGoalApp(t, model, goalConfig(t, model.server.URL, t.TempDir()))
	assembled.say(t, "make keeping the docs green a long-running goal")
	view := assembled.awaitGoal(t, func(view *appGoal.View) bool { return view != nil && view.Goal.Phase == session.GoalComplete })
	if view.RoundsStarted != 1 || view.Armed {
		t.Fatalf("goal = %+v", view)
	}
	events := assembled.stop(t)
	expectTrace(t, events, []string{
		"turn1 user",
		"goal:create r1 active 0/3",
		`result {"goal":{"id":"` + view.Goal.ID + `","revision":1,"objective":"keep docs green","phase":"active","roundsStarted":0,"maxGoalRounds":3},"activation":"armed"}`,
		"turn1 completed",
		"turn2 round 1@r1",
		`result {"goal":{"id":"` + view.Goal.ID + `","revision":1,"objective":"keep docs green","phase":"active","roundsStarted":1,"maxGoalRounds":3},"activation":"armed"}`,
		"goal:complete r2 complete 1/3",
		`result {"goal":{"id":"` + view.Goal.ID + `","revision":2,"objective":"keep docs green","phase":"complete","roundsStarted":1,"maxGoalRounds":3},"activation":"disarmed"}`,
		"turn2 tool-goal",
		"turn2 completed",
	})
	requests := model.requests()
	if len(requests) != 5 {
		t.Fatalf("model requests = %d", len(requests))
	}
	guidance := upstreamSection(t, "tool:goal")
	for index, body := range requests {
		var request seenRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil || !strings.Contains(request.Instructions, guidance) {
			t.Fatalf("request %d lacks the goal policy: %v", index, err)
		}
	}
	round := session.Text(appGoal.RoundMessage(appGoal.View{Goal: session.GoalSnapshot{ID: view.Goal.ID, Revision: 1, Objective: "keep docs green", MaxRounds: 3}}))
	if !strings.Contains(round, "Objective: \"keep docs green\"\nRound: 1/3") || !strings.Contains(requests[2], mustJSON(t, round)) {
		t.Fatalf("round prompt missing from the round request:\n%s", requests[2])
	}
	if !strings.Contains(requests[4], mustJSON(t, session.Text(appGoal.WrapUpMessage("keep docs green", "")))) {
		t.Fatal("closing instruction missing from the final request")
	}
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(string(encoded), `"`)
}

// TestComposition_GoalRoundsEnforceAuthorityAndTheRoundLimit proves that a
// goal round cannot pause or block early, that the driver blocks the goal
// at its round cap, and that a person's turn may raise the cap and resume.
func TestComposition_GoalRoundsEnforceAuthorityAndTheRoundLimit(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := newGoalModel(t,
		goalStep{tool: "get_goal", arguments: fixed(`{}`)},
		goalStep{tool: "update_goal", arguments: withGoalID(`{"goal_id":%q,"revision":1,"action":"pause"}`)},
		goalStep{tool: "update_goal", arguments: withGoalID(`{"goal_id":%q,"revision":1,"action":"blocked","blocked_reason":"the host is down"}`)},
		goalStep{text: "round one"},
		goalStep{text: "round two"},
		goalStep{tool: "get_goal", arguments: fixed(`{}`)},
		goalStep{tool: "update_goal", arguments: withGoalID(`{"goal_id":%q,"revision":2,"action":"edit","max_goal_rounds":3}`)},
		goalStep{tool: "update_goal", arguments: withGoalID(`{"goal_id":%q,"revision":3,"action":"resume"}`)},
		goalStep{text: "resumed"},
		goalStep{text: "round three"},
	)
	assembled := startGoalApp(t, model, goalConfig(t, model.server.URL, t.TempDir()))
	if _, err := assembled.app.goals.Create(t.Context(), "session-goal", "fix the docs", new(float64(2)), appGoal.ActorHost); err != nil {
		t.Fatal(err)
	}
	blocked := assembled.awaitGoal(t, func(view *appGoal.View) bool { return view.Goal.Phase == session.GoalBlocked })
	if blocked.Goal.BlockedReason.Code != "round-limit" || blocked.Goal.BlockedReason.Message != "Goal reached its configured limit of 2 rounds." {
		t.Fatalf("blocked = %+v", blocked.Goal)
	}
	if _, err := assembled.app.goals.Resume(t.Context(), "session-goal", blocked.Goal.Ref(), appGoal.ActorHost); err == nil {
		t.Fatal("exhausted goal resumed")
	}
	assembled.say(t, "raise the cap to 3 and continue")
	assembled.awaitGoal(t, func(view *appGoal.View) bool {
		return view.Goal.Phase == session.GoalBlocked && view.RoundsStarted == 3
	})
	events := assembled.stop(t)
	expectTrace(t, events, []string{
		"goal:create r1 active 0/2",
		"turn1 round 1@r1",
		`result {"goal":{"id":"` + blocked.Goal.ID + `","revision":1,"objective":"fix the docs","phase":"active","roundsStarted":1,"maxGoalRounds":2},"activation":"armed"}`,
		"result Error: this goal operation requires a direct human turn on a top-level agent",
		"result Error: blocked requires at least 3 consecutive goal rounds; current round is 1",
		"turn1 completed",
		"turn2 round 2@r1",
		"turn2 completed",
		"goal:block r2 blocked 2/2 round-limit",
		"turn3 user",
		`result {"goal":{"id":"` + blocked.Goal.ID + `","revision":2,"objective":"fix the docs","phase":"blocked","roundsStarted":2,"maxGoalRounds":2,"blockedReason":{"code":"round-limit","message":"Goal reached its configured limit of 2 rounds."}},"activation":"disarmed"}`,
		"goal:edit r3 blocked 2/3 round-limit",
		`result {"goal":{"id":"` + blocked.Goal.ID + `","revision":3,"objective":"fix the docs","phase":"blocked","roundsStarted":2,"maxGoalRounds":3,"blockedReason":{"code":"round-limit","message":"Goal reached its configured limit of 2 rounds."}},"activation":"disarmed"}`,
		"goal:resume r4 active 2/3",
		`result {"goal":{"id":"` + blocked.Goal.ID + `","revision":4,"objective":"fix the docs","phase":"active","roundsStarted":2,"maxGoalRounds":3},"activation":"armed"}`,
		"turn3 completed",
		"turn4 round 3@r4",
		"turn4 completed",
		"goal:block r5 blocked 3/3 round-limit",
	})
	if got := errorClasses(events); strings.Join(got, ",") != "HarnessError/GOAL_TOOL_AUTHORITY_REQUIRED,HarnessError/GOAL_TOOL_BLOCK_THRESHOLD" {
		t.Fatalf("persisted classifications = %v", got)
	}
}

// errorClasses lists the classification of every failed tool result on disk.
func errorClasses(events []session.Event) []string {
	var classes []string
	for _, event := range events {
		if result := event.Record.Result; result != nil && result.IsError {
			class := "none"
			if result.Error != nil {
				class = result.Error.Name + "/" + result.Error.Code
			}
			classes = append(classes, class)
		}
	}
	return classes
}

// TestComposition_HostPauseInterruptsAndResumeSurvivesRestart proves that a
// person's pause interrupts the running round, that a restarted process
// restores the goal disarmed, and that a person's resume continues it.
func TestComposition_HostPauseInterruptsAndResumeSurvivesRestart(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	hold := make(chan struct{})
	model := newGoalModel(t,
		goalStep{text: "never sent", hold: hold},
		goalStep{text: "round two"},
		goalStep{text: "interrupted by shutdown", hold: hold},
		goalStep{text: "round three"},
	)
	data := t.TempDir()
	config := goalConfig(t, model.server.URL, data)
	assembled := startGoalApp(t, model, config)
	created, err := assembled.app.goals.Create(t.Context(), "session-goal", "ship", new(float64(3)), appGoal.ActorHost)
	if err != nil {
		t.Fatal(err)
	}
	model.awaitRequest(t, 0)
	paused, err := assembled.app.goals.Pause(t.Context(), "session-goal", created.Goal.Ref(), appGoal.ActorHost)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := assembled.app.root.Agent()
	if err := root.WhenIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := assembled.app.goals.Resume(t.Context(), "session-goal", paused.Goal.Ref(), appGoal.ActorHost); err != nil {
		t.Fatal(err)
	}
	model.awaitRequest(t, 2)
	events := assembled.stop(t)
	expectTrace(t, events, []string{
		"goal:create r1 active 0/3",
		"turn1 round 1@r1",
		"goal:pause r2 paused 1/3",
		"turn1 canceled",
		"goal:resume r3 active 1/3",
		"turn2 round 2@r3",
		"turn2 completed",
		"turn3 round 3@r3",
		"turn3 canceled",
	})

	config.create = false
	restarted := startGoalApp(t, model, config)
	restored, err := restarted.app.goals.Get(t.Context(), "session-goal")
	if err != nil || restored.Goal.Phase != session.GoalActive || restored.Armed || restored.RoundsStarted != 3 {
		t.Fatalf("restored = %+v, %v", restored, err)
	}
	objective := "ship it all"
	edited, err := restarted.app.goals.Edit(t.Context(), "session-goal", restored.Goal.Ref(), &objective, new(float64(4)), appGoal.ActorHost)
	if err != nil || edited.Armed {
		t.Fatalf("edit = %+v, %v", edited, err)
	}
	if _, err := restarted.app.goals.Resume(t.Context(), "session-goal", edited.Goal.Ref(), appGoal.ActorHost); err != nil {
		t.Fatal(err)
	}
	restarted.awaitGoal(t, func(view *appGoal.View) bool { return view.Goal.Phase == session.GoalBlocked })
	events = restarted.stop(t)
	if got := trace(events)[9:]; strings.Join(got, "\n") != strings.Join([]string{
		"goal:edit r4 active 3/4",
		"goal:resume r5 active 3/4",
		"turn4 round 4@r5",
		"turn4 completed",
		"goal:block r6 blocked 4/4 round-limit",
	}, "\n") {
		t.Fatalf("restarted trace = %q", got)
	}
}

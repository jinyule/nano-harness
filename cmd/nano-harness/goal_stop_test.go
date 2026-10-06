package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// observedSettlement exposes the driver's completed idle checkpoint, so
// assertions do not race its application of the durable turn outcome.
type observedSettlement struct {
	*appGoal.Service
	checked chan *appGoal.View
}

func TestCompositionID_RejectsPreviousGoalStopSemantics(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
	previous := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00fs-tools-v3\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v1\x00subagent-tools-v3\x00todo-tools-v1\x00web-tools-v1\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00goal-tools-v1\x00spill-v1\x00session-v2"))
	root := filepath.Join(t.TempDir(), "sessions")
	old, err := sessionjsonl.New(sessionjsonl.Config{Root: root, CompositionID: hex.EncodeToString(previous[:])})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := old.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := old.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "old", Cwd: config.workspaceRoot, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "old.jsonl")
	before, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned session root
	if err != nil {
		t.Fatal(err)
	}
	current, err := sessionjsonl.New(sessionjsonl.Config{Root: root, CompositionID: compositionID(config)})
	if err != nil {
		t.Fatal(err)
	}
	currentScope := &plugin.Scope{}
	if err := current.Start(t.Context(), currentScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = currentScope.Close(context.Background()) })
	if _, err := current.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "old", Cwd: config.workspaceRoot}); !errors.Is(err, sessionjsonl.ErrCorruptSession) {
		t.Fatalf("old goal semantics accepted: %v", err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned session root
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected session changed: %v", err)
	}
}

func (goals *observedSettlement) Settle(ctx context.Context, id string, after uint64) (uint64, error) {
	last, err := goals.Service.Settle(ctx, id, after)
	view, _ := goals.Get(ctx, id)
	select {
	case goals.checked <- view:
	case <-ctx.Done():
	}
	return last, err
}

func TestComposition_OutputLimitStopsGoalRoundsForEveryProvider(t *testing.T) {
	for _, wire := range []struct{ provider, stream string }{
		{"openai", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"},
		{"anthropic", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"openrouter", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(wire.provider, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
				t.Setenv(key, "test-key")
			}
			model := &goalModel{}
			model.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				model.mu.Lock()
				model.bodies = append(model.bodies, string(body))
				model.mu.Unlock()
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, wire.stream)
			}))
			t.Cleanup(model.server.Close)
			config := goalConfig(t, model.server.URL, t.TempDir())
			settings := fmt.Sprintf("route:\n  provider: %s\n  model: test-model\nproviders:\n  %s:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", wire.provider, wire.provider, model.server.URL)
			if err := os.WriteFile(config.settingsPath, []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			app, err := composeApplication(config, dependencies{httpClient: model.server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			observed := &observedSettlement{Service: app.goals, checked: make(chan *appGoal.View, 16)}
			for i, component := range app.plugins {
				if component.ID() == "goal-driver" {
					app.plugins[i], err = appGoal.NewDriver(observed, app.root)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			runtime, err := plugin.New(app.plugins...)
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
			_, err = app.goals.Create(ctx, "session-goal", "ship", nil, appGoal.ActorHost)
			if err != nil {
				t.Fatal(err)
			}
			var view *appGoal.View
			for round := uint64(1); round <= 2; round++ {
				for {
					select {
					case checked := <-observed.checked:
						if checked == nil || checked.RoundsStarted < round {
							continue
						}
						if checked.Armed || checked.Goal.Phase != session.GoalActive || checked.RoundsStarted != round {
							t.Fatalf("output limit continued the goal: %+v", checked)
						}
						view = checked
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					break
				}
				if round == 1 {
					if _, err := app.goals.Resume(ctx, "session-goal", view.Goal.Ref(), appGoal.ActorHost); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := runtime.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			events := readTranscript(t, filepath.Join(config.sessionRoot, "session-goal.jsonl"))
			ends := 0
			for _, event := range events {
				if event.Record.Type == session.RecordTurnEnd {
					ends++
					if event.Record.Outcome != session.OutcomeMaxTokens {
						t.Fatalf("durable output limit = %s", event.Record.Outcome)
					}
				}
			}
			if ends != 2 || len(model.requests()) != 2 {
				t.Fatalf("turns=%d requests=%d", ends, len(model.requests()))
			}
		})
	}
}

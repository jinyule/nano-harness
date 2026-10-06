package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestDecoder_ECMAScriptDurableWhitespace(t *testing.T) {
	for _, kind := range []string{"todo", "goal objective", "goal id", "block reason"} {
		for _, value := range []string{"\u0085", "\u0085x\u0085", "\ufeff", "\ufeffx", "x\ufeff"} {
			t.Run(kind+value, func(t *testing.T) {
				var fixture []byte
				var err error
				if kind == "todo" {
					fixture, err = os.ReadFile("testdata/session-v2-todo.jsonl")
					if err != nil {
						t.Fatal(err)
					}
					quoted, _ := json.Marshal(value)
					fixture = bytes.ReplaceAll(fixture, []byte(`"content":"write tests"`), append([]byte(`"content":`), quoted...))
				} else {
					objective, id, reason := "ship", "g", "blocked"
					switch kind {
					case "goal objective":
						objective = value
					case "goal id":
						id = value
					case "block reason":
						reason = value
					}
					fixture = []byte(`{"type":"session","version":2,"header":{"session_id":"fixture","composition_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","created_at_unix_ms":1,"cwd":"/synthetic/workspace"}}` + "\n")
					for index, record := range []session.Record{
						{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpCreate, Snapshot: &session.GoalSnapshot{ID: id, Revision: 1, Objective: objective, Phase: session.GoalActive, MaxRounds: 3}, CreatedAtUnixMS: 1, UpdatedAtUnixMS: 1}},
						{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpBlock, Snapshot: &session.GoalSnapshot{ID: id, Revision: 2, Objective: objective, Phase: session.GoalBlocked, MaxRounds: 3, BlockedReason: &session.GoalBlockReason{Code: "model-reported", Message: reason}}, CreatedAtUnixMS: 1, UpdatedAtUnixMS: 2}},
					} {
						encoded, err := json.Marshal(session.Event{Sequence: uint64(index + 1), Record: record})
						if err != nil {
							t.Fatal(err)
						}
						fixture = append(append(fixture, encoded...), '\n')
					}
				}
				manager, scope := startManager(t)
				t.Cleanup(func() { _ = scope.Close(context.Background()) })
				path := filepath.Join(manager.config.Root, "fixture.jsonl")
				if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
					t.Fatal(err)
				}
				_, events, err := manager.Inspect(t.Context(), "fixture")
				valid := strings.Contains(value, "\u0085")
				if (err == nil) != valid || (!valid && !errors.Is(err, ErrCorruptSession)) {
					t.Fatalf("value=%q error=%v want valid=%t", value, err, valid)
				}
				if valid {
					log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
					if err != nil {
						t.Fatal(err)
					}
					if err := log.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					if kind == "todo" {
						if got := standingTodos(events)[0].Content; got != value {
							t.Fatalf("content=%q want=%q", got, value)
						}
					} else {
						state, err := session.ProjectGoal(events)
						if err != nil || state.Goal == nil {
							t.Fatalf("projection=%+v error=%v", state, err)
						}
					}
				}
				after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
				if err != nil || !bytes.Equal(after, fixture) {
					t.Fatalf("inspection changed bytes: %v", err)
				}
			})
		}
	}
}

package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
	coreskill "github.com/jinyule/nano-harness/internal/core/skill"
)

var fixtureSkillEntries = []coreskill.Entry{coreskill.NewEntry("demo", "Demo skill.")}

const fixtureSkillBody = "<skill_content name=\"demo\">\n<skill_resources>\nBase directory for this skill: /synthetic/workspace/.agents/skills/demo\nResolve relative paths mentioned by this skill against the base directory before using them. Load referenced resources only as needed.\n</skill_resources>\n\n<skill_instructions>\nFollow the demo.\n</skill_instructions>\n</skill_content>"

func skillContext(kind, text string) *coresession.Message {
	return &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: kind, Plugin: "skill-tools"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: text}}}
}

// appendSkillTurn writes the fixture turn from independently constructed
// records: a gesture, the initial catalog and the injected body before the
// step, then one skill tool call whose result is the same body.
func appendSkillTurn(t *testing.T, log *Log) {
	t.Helper()
	catalog, ok, err := coreskill.CatalogUpdate(nil, fixtureSkillEntries)
	if err != nil || !ok {
		t.Fatalf("catalog = %v, %v", ok, err)
	}
	gesture := &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "user"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "/demo check the fixture"}}}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: gesture})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: skillContext(coreskill.SourceCatalog, catalog)})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: skillContext(coreskill.SourceInvocation, fixtureSkillBody)})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("loading")})
	call := &coresession.ToolCall{ID: "call-skill", Name: "skill", Arguments: json.RawMessage(`{"name":"demo"}`)}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: call})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call-skill", Output: fixtureSkillBody}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted})
}

// TestSessionV2Skill_FrozenContract proves that skill context is ordinary
// v2 user input: the frozen fixture decodes, projects into the surface, and
// yields the catalog state a resumed process derives from disk.
func TestSessionV2Skill_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-skill.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(events) != 11 || len(surface) != 6 {
		t.Fatalf("events=%d surface=%d err=%v", len(events), len(surface), err)
	}
	if surface[1].Message.Source.Kind != coreskill.SourceCatalog || surface[2].Message.Source.Kind != coreskill.SourceInvocation || surface[5].Result.Output != fixtureSkillBody {
		t.Fatalf("surface = %+v", surface)
	}
	if _, changed, err := coreskill.CatalogUpdate(events, fixtureSkillEntries); err != nil || changed {
		t.Fatalf("resumed catalog republished: %v %v", changed, err)
	}
	if text, changed, err := coreskill.CatalogUpdate(events, nil); err != nil || !changed || !bytes.Contains([]byte(text), []byte("No skills are currently available")) {
		t.Fatalf("resumed removal = %v %v\n%s", changed, err, text)
	}
	if names := coreskill.InvokedNames(events); names != nil {
		t.Fatalf("consumed gesture is still pending: %v", names)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatal("read and closed resume changed a committed skill fixture")
	}

	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	appendSkillTurn(t, writer)
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen skill fixture:\n%s", actual)
	}
}

func TestSessionV2Skill_RejectsMisplacedContext(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-skill.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	invocation := string(bytes.Split(fixture, []byte("\n"))[4])
	for _, test := range []struct{ name, from, to string }{
		{"outside turn", `{"seq":3,"record":{"type":"user/message","turn":1,`, `{"seq":3,"record":{"type":"user/message","turn":2,`},
		{"inactive step", `{"seq":3,"record":{"type":"user/message","turn":1,`, `{"seq":3,"record":{"type":"user/message","turn":1,"step":1,`},
		{"undeclared source field", `"source":{"kind":"skill-catalog","plugin":"skill-tools"}`, `"source":{"kind":"skill-catalog","plugin":"skill-tools","entries":[]}`},
		{"empty context", invocation, `{"seq":4,"record":{"type":"user/message","turn":1,"message":{"role":"user","content":[],"source":{"kind":"skill-invocation","plugin":"skill-tools"}}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatal("mutation did not apply")
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("got %v, want %v", err, ErrCorruptSession)
			}
		})
	}
}

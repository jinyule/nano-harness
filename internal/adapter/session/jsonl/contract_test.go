package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

func TestSessionV2_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2.jsonl")
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
	header, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if header.SessionID != "fixture" || len(events) != 11 || events[10].Record.Outcome != coresession.OutcomeCompleted {
		t.Fatalf("header=%+v events=%v", header, events)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 4 || surface[0].Message.Content[0].Text != "hello" || surface[3].Result.Output != "ok" {
		t.Fatalf("surface=%+v err=%v", surface, err)
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
		t.Fatal("read and closed resume changed a committed fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	if _, err := writeHeader(output, coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	appendClosedTurn(t, writer, 1)
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 fixture:\n%s", actual)
	}
}

func TestSessionV2_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, from, to string
		want           error
	}{
		{"old-version", `"version":2`, `"version":1`, ErrUnsupported},
		{"future-version", `"version":2`, `"version":3`, ErrUnsupported},
		{"unknown-field", `"cwd":`, `"unexpected":true,"cwd":`, ErrCorruptSession},
		{"sequence-gap", `"seq":2`, `"seq":3`, ErrCorruptSession},
		{"unknown-record", `"type":"turn/start"`, `"type":"turn/unknown"`, ErrCorruptSession},
		{"causal-step", `"type":"step/start","turn":1,"step":1`, `"type":"step/start","turn":1,"step":2`, ErrCorruptSession},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := temporaryFile(t)
			if _, err := file.Write(bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

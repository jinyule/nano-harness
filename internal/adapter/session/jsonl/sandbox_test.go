package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func sandboxRecord(mode session.SandboxMode, source string) session.Record {
	return session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: mode, Source: source}}
}

func TestSessionV2Sandbox_FrozenContractAndResume(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-sandbox.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under this test manager private root
		t.Fatal(err)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := log.Events(t.Context())
	if err != nil || len(events) != 10 || session.EffectiveSandbox(events[:5]) != session.SandboxReadOnly || session.EffectiveSandbox(events[:6]) != session.SandboxWorkspaceWrite || session.EffectiveSandbox(events) != session.SandboxDangerFullAccess {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	surface, err := session.Surface(events)
	if err != nil || len(surface) != 2 {
		t.Fatalf("surface=%v err=%v", surface, err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under this test manager private root
	if err != nil || !bytes.Equal(fixture, after) {
		t.Fatalf("resume changed fixture: %v", err)
	}

	output := temporaryFile(t)
	header := session.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	size, err := writeHeader(output, header, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: size}
	for _, record := range []session.Record{
		sandboxRecord(session.SandboxReadOnly, ""),
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordUserMessage, Turn: 1, Message: userMessage("hello")},
		{Type: session.RecordStepStart, Turn: 1, Step: 1},
		{Type: session.RecordRequestHeader, Turn: 1, Step: 1, Header: &session.RequestHeader{Provider: "openai", Model: "model"}},
		sandboxRecord(session.SandboxWorkspaceWrite, ""),
		{Type: session.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("answer")},
		{Type: session.RecordStepEnd, Turn: 1, Step: 1},
		{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted},
		sandboxRecord(session.SandboxDangerFullAccess, ""),
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil || !bytes.Equal(fixture, actual) {
		t.Fatalf("writer differs from fixed fixture: %v\n%s", err, actual)
	}

	// Repair an interrupted step without rolling back the latest mode.
	appendRecord(t, logForRepair(t, manager), sandboxRecord(session.SandboxReadOnly, ""))
}

func logForRepair(t *testing.T, manager *Manager) *Log {
	t.Helper()
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "repair", Create: true, Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, session.Record{Type: session.RecordTurnStart, Turn: 1})
	appendRecord(t, log, session.Record{Type: session.RecordStepStart, Turn: 1, Step: 1})
	appendRecord(t, log, sandboxRecord(session.SandboxDangerFullAccess, ""))
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: "repair", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	events, _ := resumed.Events(t.Context())
	if session.EffectiveSandbox(events) != session.SandboxDangerFullAccess || events[len(events)-1].Record.Outcome != session.OutcomeInterrupted {
		t.Fatalf("repair: %v", events)
	}
	return resumed
}

func TestSessionV2Sandbox_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-sandbox.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{
		`"sandbox":{"mode":"unknown"}`, `"sandbox":null`, `"sandbox":{}`, `"sandbox":{"mode":null}`,
		`"sandbox":{"mode":"read-only","root":"/"}`, `"sandbox":{"mode":"read-only","source":"model"}`,
		`"sandbox":{"mode":"read-only","source":""}`, `"sandbox":{"mode":"read-only","source":null}`,
		`"sandbox":{"mode":"read-only","source":"delegation"}`, `"turn":1,"sandbox":{"mode":"read-only"}`,
		`"step":1,"sandbox":{"mode":"read-only"}`, `"message":{},"sandbox":{"mode":"read-only"}`,
	} {
		t.Run(replacement, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(`"sandbox":{"mode":"read-only"}`), []byte(replacement), 1)
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("accepted %s: %v", replacement, err)
			}
		})
	}
	file := temporaryFile(t)
	if _, err := file.Write(fixture); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readSession(file, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("old composition accepted: %v", err)
	}

	wrongOwner := bytes.Replace(fixture, []byte(`"cwd":"/synthetic/workspace"`), []byte(`"cwd":"/synthetic/workspace","parent_session_id":"parent","delegation_depth":1`), 1)
	file = temporaryFile(t)
	if _, err := file.Write(wrongOwner); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("delegated human policy accepted: %v", err)
	}
}

func TestSandboxOrder_DelegationOwnership(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	child, err := manager.Open(t.Context(), OpenOptions{SessionID: "child", Create: true, Cwd: "/work", ParentSessionID: "root", DelegationDepth: 1, Seed: []session.Event{{Sequence: 1, Record: sandboxRecord(session.SandboxDangerFullAccess, "")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close(context.Background()) })
	appendRecord(t, child, session.Record{Type: session.RecordSubagentDescriptor, Subagent: &session.SubagentDescriptor{Version: session.SubagentDescriptorVersion, Route: session.SubagentRoute{Provider: "openai", Model: "m"}, Provider: session.SubagentFork, Mode: session.SubagentContinuable, Label: "child", Inherited: 1}})
	if _, err := child.Append(t.Context(), sandboxRecord(session.SandboxReadOnly, "")); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("child human switch: %v", err)
	}
	appendRecord(t, child, sandboxRecord(session.SandboxReadOnly, "delegation"))
	if _, err := child.Append(t.Context(), sandboxRecord(session.SandboxWorkspaceWrite, "delegation")); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("late delegation: %v", err)
	}
	events, _ := child.Events(t.Context())
	if session.EffectiveSandbox(events) != session.SandboxReadOnly {
		t.Fatal("fork seed overrode captured mode")
	}
	if err := validateSandboxOwner(session.Header{}, events); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("root delegation accepted: %v", err)
	}
	if _, err := validateOrder([]session.Event{{Sequence: 1, Record: sandboxRecord(session.SandboxWorkspaceWrite, "delegation")}}, false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("initial delegation: %v", err)
	}
	bad := sandboxRecord(session.SandboxReadOnly, "")
	bad.Turn = 1
	if _, err := validateOrder([]session.Event{{Sequence: 1, Record: bad}}, false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("causal turn: %v", err)
	}
}

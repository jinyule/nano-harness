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
)

func textBlocks(texts ...string) []coresession.ContentBlock {
	blocks := make([]coresession.ContentBlock, len(texts))
	for index, text := range texts {
		blocks[index] = coresession.ContentBlock{Type: coresession.ContentText, Text: text}
	}
	return blocks
}

func TestSessionV2Subagent_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-subagent.jsonl")
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
	if header.ParentSessionID != "parent" || header.DelegationDepth != 1 || len(events) != 23 {
		t.Fatalf("header=%+v events=%d", header, len(events))
	}
	// The inherited catalog entry belongs to the parent, not to this child.
	own := coresession.OwnEvents(events)
	if len(own) != 14 || own[0].Sequence != 10 || coresession.Children(events) != nil {
		t.Fatalf("own=%d children=%v", len(own), coresession.Children(events))
	}
	if text := coresession.FinalAssistantText(own); text != "done" {
		t.Fatalf("closing text = %q", text)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 10 || coresession.Text(*surface[0].Message) != "delegate" || coresession.Text(*surface[8].Message) != "Agent parent sent a message: thanks" {
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
		t.Fatal("read and closed resume changed a committed subagent fixture")
	}

	// The writer uses independently constructed records: the inherited prefix
	// goes through the seed path, the child's own records through Append.
	inherited := []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("delegate")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("spawning")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call-spawn", Name: "subagent", Arguments: json.RawMessage(`{"description":"scan","prompt":"scan the tree"}`)}},
		{Type: coresession.RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &coresession.SubagentCatalog{SessionID: "sibling", Mode: coresession.SubagentContinuable, Label: "scan"}},
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call-spawn", Output: "started subagent sibling"}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	}
	seed := make([]coresession.Event, len(inherited))
	for index, record := range inherited {
		seed[index] = coresession.Event{Sequence: uint64(index + 1), Record: record}
	}
	output := temporaryFile(t)
	written := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace", ParentSessionID: "parent", DelegationDepth: 1}
	size, err := writeHeader(output, written, seed)
	if err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: written, active: true, size: size, events: seed}
	delegated := &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "delegation"}, Content: textBlocks("review the scan", "report back")}
	relayed := &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "agent-message", SenderSessionID: "parent"}, Content: textBlocks("Agent parent sent a message: ", "thanks")}
	for _, record := range []coresession.Record{
		{Type: coresession.RecordSubagentDescriptor, Subagent: &coresession.SubagentDescriptor{Version: 3, Route: coresession.SubagentRoute{Provider: "openai", Model: "model", Effort: coresession.EffortMax}, Provider: coresession.SubagentFork, Mode: coresession.SubagentContinuable, Label: "review", Inherited: 9}},
		{Type: coresession.RecordApprovalPolicy, Approval: &coresession.ApprovalData{Policy: coresession.ApprovalNever, Source: "delegation"}},
		{Type: coresession.RecordTurnStart, Turn: 2},
		{Type: coresession.RecordUserMessage, Turn: 2, Message: delegated},
		{Type: coresession.RecordStepStart, Turn: 2, Step: 1},
		{Type: coresession.RecordAssistantMessage, Turn: 2, Step: 1, Message: assistantMessage("reporting")},
		{Type: coresession.RecordToolCall, Turn: 2, Step: 1, Call: &coresession.ToolCall{ID: "call-send", Name: "send_message", Arguments: json.RawMessage(`{"agent_id":"parent","message":"looks fine"}`)}},
		{Type: coresession.RecordToolResult, Turn: 2, Step: 1, Result: &coresession.ToolResult{CallID: "call-send", Output: "message delivered to agent parent"}},
		{Type: coresession.RecordStepEnd, Turn: 2, Step: 1},
		{Type: coresession.RecordUserMessage, Turn: 2, Message: relayed},
		{Type: coresession.RecordStepStart, Turn: 2, Step: 2},
		{Type: coresession.RecordAssistantMessage, Turn: 2, Step: 2, Message: assistantMessage("done")},
		{Type: coresession.RecordStepEnd, Turn: 2, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 2, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 subagent fixture:\n%s", actual)
	}
}

func TestSessionV2Subagent_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-subagent.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"descriptor-version-1", `"subagent":{"version":3`, `"subagent":{"version":1`},
		{"descriptor-version-2", `"subagent":{"version":3`, `"subagent":{"version":2`},
		{"descriptor-missing-route", `"route":{"provider":"openai","model":"model","effort":"max"},`, ``},
		{"descriptor-route-effort", `"effort":"max"}`, `"effort":"extreme"}`},
		{"descriptor-route-field", `"effort":"max"}`, `"effort":"max","base_url":"x"}`},
		{"agent-message-missing-sender", `,"sender_session_id":"parent"`, ``},
		{"sender-on-delegation", `"source":{"kind":"delegation"}`, `"source":{"kind":"delegation","sender_session_id":"parent"}`},
		{"descriptor-old-provider", `"provider":"fork"`, `"provider":"in-process"`},
		{"descriptor-unknown-field", `"inherited":9}`, `"inherited":9,"pool":1}`},
		{"descriptor-inherited-short", `"inherited":9}`, `"inherited":8}`},
		{"descriptor-missing-inherited", `,"inherited":9}`, `}`},
		{"descriptor-unknown-mode", `"mode":"continuable","label":"review"`, `"mode":"resident","label":"review"`},
		{"catalog-unknown-field", `"label":"scan"}`, `"label":"scan","provider":"spawn"}`},
		{"catalog-unknown-mode", `"mode":"continuable","label":"scan"`, `"mode":"unknown","label":"scan"`},
		{"catalog-outside-step", `"type":"subagent/catalog","turn":1,"step":1`, `"type":"subagent/catalog","turn":1,"step":2`},
		{"catalog-without-step", `"type":"subagent/catalog","turn":1,"step":1`, `"type":"subagent/catalog","turn":1`},
		{"catalog-empty-session", `"session_id":"sibling"`, `"session_id":""`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatalf("replacement %q did not change the fixture", test.from)
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

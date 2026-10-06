package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func webRequest(index int, query string) session.Record {
	return session.Record{Type: session.RecordWebSearchRequest, Turn: 1, Step: 1, Search: &session.WebSearchRequest{
		CallID: "search", Index: index, Provider: "anthropic", Model: "search-model", Effort: session.EffortLow,
		Endpoint: "anthropic-messages", Query: query, TimeoutMS: 60000, MaxResults: 8, MaxUses: 5, MaxTokens: 4096,
	}}
}

func webFixtureRecords() []session.Record {
	return []session.Record{
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "research"}}}},
		{Type: session.RecordStepStart, Turn: 1, Step: 1},
		{Type: session.RecordRequestHeader, Turn: 1, Step: 1, Header: &session.RequestHeader{Provider: "openai", Model: "chat"}},
		{Type: session.RecordAssistantMessage, Turn: 1, Step: 1, Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "model"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "searching"}}}},
		{Type: session.RecordToolCall, Turn: 1, Step: 1, Call: &session.ToolCall{ID: "search", Name: "web_search", Arguments: json.RawMessage(`{"queries":["go","rust"]}`)}},
		webRequest(1, "go"), webRequest(2, "rust"),
		{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &session.ToolResult{CallID: "search", Output: "sources"}},
		{Type: session.RecordStepEnd, Turn: 1, Step: 1},
		{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted},
	}
}

func TestSessionV2WebSearch_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-web-search.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private session root
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil || len(events) != 11 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	surface, err := session.Surface(events)
	if err != nil || len(surface) != 4 {
		t.Fatalf("surface=%+v err=%v", surface, err)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
	if err != nil || !bytes.Equal(after, fixture) {
		t.Fatalf("resume changed committed fixture: %v", err)
	}

	output := temporaryFile(t)
	header := session.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	size, err := writeHeader(output, header, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: size}
	for _, record := range webFixtureRecords() {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil || !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from fixed fixture: %v\n%s", err, actual)
	}
}

func TestSessionV2WebSearch_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-web-search.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"unknown-field", `"timeout_ms":60000`, `"Authorization":"secret","timeout_ms":60000`},
		{"missing-payload", `,"search":{"call_id":"search","index":1,"provider":"anthropic","model":"search-model","effort":"low","endpoint":"anthropic-messages","query":"go","timeout_ms":60000,"max_results":8,"max_uses":5,"max_tokens":4096}`, ``},
		{"wrong-call", `"call_id":"search","index":1`, `"call_id":"missing","index":1`},
		{"wrong-tool", `"name":"web_search"`, `"name":"read"`},
		{"wrong-turn", `"type":"web/search-request","turn":1`, `"type":"web/search-request","turn":2`},
		{"wrong-step", `"type":"web/search-request","turn":1,"step":1`, `"type":"web/search-request","turn":1,"step":2`},
		{"duplicate-index", `"index":2`, `"index":1`},
		{"index-gap", `"index":2`, `"index":3`},
		{"duplicate-query", `"query":"rust"`, `"query":"go"`},
		{"query-outside-call", `"query":"rust"`, `"query":"python"`},
		{"query-order-differs-from-call", `"queries":["go","rust"]`, `"queries":["rust","go"]`},
		{"index-exceeds-distinct-queries", `"queries":["go","rust"]`, `"queries":["go","go"]`},
		{"missing-call-queries", `"queries":["go","rust"]`, `"other":["go","rust"]`},
		{"wrong-call-query-type", `"queries":["go","rust"]`, `"queries":"go"`},
		{"blank-call-query", `"queries":["go","rust"]`, `"queries":["go","\ufeff"]`},
		{"too-many-call-queries", `"queries":["go","rust"]`, `"queries":["go","rust","a","b","c"]`},
		{"wrong-provider", `"provider":"anthropic"`, `"provider":"openai"`},
		{"URL-endpoint", `"endpoint":"anthropic-messages"`, `"endpoint":"https://secret.example/key"`},
		{"unknown-effort", `"effort":"low"`, `"effort":"huge"`},
		{"zero-timeout", `"timeout_ms":60000`, `"timeout_ms":0`},
		{"missing-timeout", `,"timeout_ms":60000`, ``},
		{"missing-results", `,"max_results":8`, ``},
		{"missing-uses", `,"max_uses":5`, ``},
		{"wrong-tokens", `"max_tokens":4096`, `"max_tokens":4097`},
		{"blank-query", `"query":"go"`, `"query":" "`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatal("fixture replacement did not match")
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("changed contract accepted: %v", err)
			}
		})
	}
}

func TestLog_WebSearchAuditRepairPreservesIntentWithoutRedispatch(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	for _, count := range []int{0, 1, 2} {
		id := []string{"before-audit", "partial-audit", "all-audits"}[count]
		log, err := manager.Open(t.Context(), OpenOptions{SessionID: id, Create: true, Cwd: "/synthetic/workspace"})
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range webFixtureRecords()[:6+count] {
			appendRecord(t, log, record)
		}
		if count > 0 {
			events, _ := log.Events(t.Context())
			events[6].Record.Search.Query = "mutated"
			fresh, _ := log.Events(t.Context())
			if fresh[6].Record.Search.Query != "go" {
				t.Fatal("audit aliases journal")
			}
		}
		if err := log.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(manager.config.Root, id+".jsonl")
		before, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
		if err != nil {
			t.Fatal(err)
		}
		resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: id, Cwd: "/synthetic/workspace"})
		if err != nil {
			t.Fatal(err)
		}
		events, err := resumed.Events(t.Context())
		if err != nil || len(events) != 9+count {
			t.Fatalf("repair=%+v err=%v", events, err)
		}
		result := events[6+count].Record.Result
		if result == nil || !result.IsError || result.CallID != "search" || events[len(events)-1].Record.Outcome != session.OutcomeInterrupted {
			t.Fatalf("repair result=%+v", result)
		}
		if err := resumed.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
		if err != nil || !bytes.HasPrefix(after, before) {
			t.Fatalf("repair changed prefix: %v", err)
		}
		if !reflect.DeepEqual(events[6:6+count], eventsFromRecords(webFixtureRecords())[6:6+count]) {
			t.Fatal("repair changed intents")
		}
	}
}

func TestLog_WebSearchAuditMatchesDistinctCallQueries(t *testing.T) {
	for _, test := range []struct {
		name    string
		queries []string
	}{
		{"exact-deduplication", []string{"go", "go", "rust", "go"}},
		{"preserve-whitespace", []string{" go ", "rust"}},
		{"NEL-is-not-blank", []string{"\u0085", "\u0085", "rust"}},
		{"query-above-128-KiB", []string{strings.Repeat("q", (128<<10)+1), "rust"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, scope := startManager(t)
			t.Cleanup(func() { _ = scope.Close(context.Background()) })
			log, err := manager.Open(t.Context(), OpenOptions{SessionID: "bound-query", Create: true, Cwd: "/synthetic/workspace"})
			if err != nil {
				t.Fatal(err)
			}
			prefix := webFixtureRecords()[:6]
			path := filepath.Join(manager.config.Root, "bound-query.jsonl")
			arguments, err := json.Marshal(map[string][]string{"queries": test.queries})
			if err != nil {
				t.Fatal(err)
			}
			prefix[5].Call.Arguments = arguments
			for _, record := range prefix {
				appendRecord(t, log, record)
			}
			for index, query := range []string{test.queries[0], "rust"} {
				before, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
				if err != nil {
					t.Fatal(err)
				}
				if _, err := log.Append(t.Context(), webRequest(index+1, "python")); !errors.Is(err, ErrCorruptSession) {
					t.Fatalf("unrequested query appended: %v", err)
				}
				after, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("rejected audit changed transcript: %v", err)
				}
				appendRecord(t, log, webRequest(index+1, query))
			}
			if _, err := log.Append(t.Context(), webRequest(3, "other")); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("audit exceeded distinct query count: %v", err)
			}
			if err := log.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := manager.Inspect(t.Context(), "bound-query"); err != nil {
				t.Fatalf("valid query order was not recoverable: %v", err)
			}
		})
	}
}

func TestLog_WebSearchAuditRejectsOmittedArguments(t *testing.T) {
	const reason = "web/search-request does not name a pending web_search call"
	requireRejected := func(t *testing.T, err error) {
		t.Helper()
		// Omitted calls must fail at the pending-call boundary, before parsing
		// their empty arguments; a later parse error would hide a missing guard.
		if !errors.Is(err, ErrCorruptSession) || !strings.Contains(err.Error(), reason) {
			t.Fatalf("omitted call was not rejected at the call boundary: %v", err)
		}
	}
	t.Run("read", func(t *testing.T) {
		fixture, err := os.ReadFile("testdata/session-v2-web-search.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		prefix, _, found := bytes.Cut(fixture, []byte(`{"seq":8,`))
		if !found {
			t.Fatal("fixture lacks the second audit")
		}
		// Keep the first audit in an otherwise valid interrupted prefix, so a
		// subsequent successful result cannot mask acceptance of this audit.
		changed := bytes.Replace(prefix, []byte(`"arguments":{"queries":["go","rust"]}`), []byte(`"arguments":{},"arguments_omitted":true`), 1)
		if bytes.Equal(changed, prefix) {
			t.Fatal("omitted-call replacement did not match")
		}
		file := temporaryFile(t)
		if _, err := file.Write(changed); err != nil {
			t.Fatal(err)
		}
		_, _, _, err = readSession(file, testCompositionID)
		requireRejected(t, err)
	})
	t.Run("append", func(t *testing.T) {
		manager, scope := startManager(t)
		t.Cleanup(func() { _ = scope.Close(context.Background()) })
		log, err := manager.Open(t.Context(), OpenOptions{SessionID: "omitted-search", Create: true, Cwd: "/synthetic/workspace"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = log.Close(context.Background()) })
		prefix := webFixtureRecords()[:6]
		prefix[5].Call.Arguments, prefix[5].Call.ArgumentsOmitted = json.RawMessage(`{}`), true
		for _, record := range prefix {
			appendRecord(t, log, record)
		}
		path := filepath.Join(manager.config.Root, "omitted-search.jsonl")
		before, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
		if err != nil {
			t.Fatal(err)
		}
		_, err = log.Append(t.Context(), webRequest(1, "go"))
		requireRejected(t, err)
		after, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned private session root
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("rejected omitted-call audit changed transcript: %v", err)
		}
	})
}

func eventsFromRecords(records []session.Record) []session.Event {
	events := make([]session.Event, len(records))
	for i, record := range records {
		events[i] = session.Event{Sequence: uint64(i + 1), Record: record}
	}
	return events
}

func TestValidateOrder_WebSearchIntentRequiresPendingCall(t *testing.T) {
	records := webFixtureRecords()
	for name, prefix := range map[string][]session.Record{
		"before-call": records[:5], "after-result": records[:9], "after-step": records[:10], "after-turn": records,
	} {
		events := eventsFromRecords(append(append([]session.Record(nil), prefix...), webRequest(1, "new")))
		if _, err := validateOrder(events, false); !errors.Is(err, ErrCorruptSession) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

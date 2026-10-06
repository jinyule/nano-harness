package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// testRoute is a valid inherited route for children created directly.
var testRoute = session.SubagentRoute{Provider: "openai", Model: "gpt-5.6-luna", Effort: session.EffortMax}

// headers returns the request headers in events.
func headers(events []session.Event) []session.RequestHeader {
	var found []session.RequestHeader
	for _, event := range events {
		if event.Record.Type == session.RecordRequestHeader {
			found = append(found, *event.Record.Header)
		}
	}
	return found
}

// memorySettings is a settings backend that keeps the document in memory.
type memorySettings struct{}

func (memorySettings) Load(context.Context) (settings.Document, error) {
	return settings.Defaults(), nil
}
func (memorySettings) Persist(context.Context, settings.Document) error { return nil }
func (memorySettings) Watch(ctx context.Context, _ func(settings.Document, error)) error {
	<-ctx.Done()
	return nil
}

// switchRoute hot-switches the global route to gpt-5.4 and lowers the
// effort of the model the children inherited.
func switchRoute(t *testing.T, h *harness) {
	t.Helper()
	scope := &plugin.Scope{}
	if err := h.settings.Mount(context.Background(), memorySettings{}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	_, revision, err := h.settings.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.settings.Update(context.Background(), revision, func(document *settings.Document) error {
		document.Route = settings.Route{Provider: "openai", Model: "gpt-5.4"}
		provider := document.Providers["openai"]
		provider.Models[0].Effort = session.EffortLow
		document.Providers["openai"] = provider
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestService_ChildKeepsTheRouteItInherited(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{hold: true}, then: reply{text: "child done"}},
		rule{match: "sent a message: AGAIN", first: reply{text: "again"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	// The global route changes after delegation: the child keeps the route
	// and effort of the parent request that delegated it.
	switchRoute(t, h)
	childCall.release <- "held"
	receive(t, h.done(id))
	if err := h.service.SendMessage(context.Background(), "root", id, "AGAIN"); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	childEvents := h.events(id)
	// Cold resume keeps the committed runtime context instead of repeating it.
	if contexts := messages(childEvents, SourceRuntimeContext); len(contexts) != 1 {
		t.Fatalf("runtime contexts after resume = %d", len(contexts))
	}
	if descriptor := childEvents[0].Record.Subagent; descriptor.Route != (session.SubagentRoute{Provider: "openai", Model: "gpt-5.6-luna", Effort: session.EffortMax}) {
		t.Fatalf("descriptor route = %#v", descriptor.Route)
	}
	childHeaders := headers(childEvents)
	if len(childHeaders) != 3 {
		t.Fatalf("child requests = %d", len(childHeaders))
	}
	for index, header := range childHeaders {
		if header.Provider != "openai" || header.Model != "gpt-5.6-luna" || header.Effort != session.EffortMax {
			t.Errorf("child request %d route = %s/%s effort %q", index, header.Provider, header.Model, header.Effort)
		}
	}
	// The parent itself still follows the hot-switched route.
	rootCall.release <- "released"
	receive(t, results)
	rootHeaders := headers(h.events("root"))
	if last := rootHeaders[len(rootHeaders)-1]; last.Model != "gpt-5.4" || last.Effort != "" {
		t.Fatalf("root request after the switch = %s effort %q", last.Model, last.Effort)
	}
}

func TestService_DelegationContextKeepsTheSystemPromptUniform(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "FORK_TASK", first: reply{text: "forked"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	if _, err := h.service.Run(context.Background(), start(rootCall, "review", "FORK_TASK", true)); err != nil {
		t.Fatal(err)
	}
	rootHeaders := headers(h.events("root"))
	id := session.Children(h.events("root"))[0].SessionID
	childEvents := h.events(id)
	childHeaders := headers(childEvents)
	if len(childHeaders) != 1 || childHeaders[0].System != rootHeaders[len(rootHeaders)-1].System {
		t.Fatalf("child system prompt differs from its parent's:\n%s\n---\n%s", childHeaders[0].System, rootHeaders[len(rootHeaders)-1].System)
	}
	// The delegation scope arrives as runtime context before the step opens.
	contexts := messages(childEvents, "runtime-context")
	if len(contexts) != 1 || contexts[0] != "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n"+
		"You are a delegated subagent: your permission scope was fixed when you were started and cannot be widened from inside this session — operations that require approval are rejected automatically. "+
		"When the task needs access beyond that scope, do not retry the denied operation; state the limitation in your reply so the delegating agent can handle it." {
		t.Fatalf("runtime context = %q", contexts)
	}
	rootCall.release <- "released"
	receive(t, results)
}

func TestService_PersistsTheSenderOfRelayedMessages(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{hold: true}, then: reply{text: "child done"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	if err := h.service.SendMessage(context.Background(), "root", id, "PING"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SendMessage(context.Background(), id, "root", "UP"); err != nil {
		t.Fatal(err)
	}
	childCall.release <- "held"
	receive(t, h.done(id))
	rootCall.release <- "released"
	receive(t, results)
	h.idle(h.root)
	child, err := os.ReadFile(filepath.Join(h.sessionRoot, id+".jsonl")) //nolint:gosec // the child ID names a transcript in this test's private session root
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.ReadFile(filepath.Join(h.sessionRoot, "root.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		transcript []byte
		want       string
	}{
		"parent message": {child, `"source":{"kind":"agent-message","sender_session_id":"root"}`},
		"child message":  {root, `"source":{"kind":"agent-message","sender_session_id":"` + id + `"}`},
		"settlement":     {root, `"source":{"kind":"subagent-settled","sender_session_id":"` + id + `"}`},
	} {
		if !strings.Contains(string(test.transcript), test.want) {
			t.Errorf("%s: transcript lacks %s", name, test.want)
		}
	}
}

func TestService_DelegationContextReappearsOnlyWhenHidden(t *testing.T) {
	h := startHarness(t)
	descriptor := session.Event{Sequence: 1, Record: session.Record{Type: session.RecordSubagentDescriptor, Subagent: &session.SubagentDescriptor{Version: session.SubagentDescriptorVersion, Provider: session.SubagentSpawn, Mode: session.SubagentOneShot, Route: testRoute}}}
	task := session.Event{Sequence: 2, Record: session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: SourceDelegation}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "task"}}}}}
	committed := textMessage(SourceRuntimeContext, "", delegationContext)
	contextEvent := session.Event{Sequence: 3, Record: session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &committed}}
	policy := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: SourceRuntimeContext, Plugin: "sandbox:policy"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: session.SandboxPolicyText(session.SandboxReadOnly, "/work")}}}
	policyEvent := session.Event{Sequence: 3, Record: session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &policy}}
	summary := session.Event{Sequence: 4, Record: session.Record{Type: session.RecordCompactionSummary, Turn: 1, Compaction: &session.CompactionData{ID: "c", ShadowedSeqs: []uint64{2, 3}, ShadowedTokenCount: 1, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "summary"}}, Provider: "openai", Model: "m"}}}
	broken := session.Event{Sequence: 4, Record: session.Record{Type: session.RecordCompactionSummary, Turn: 1, Compaction: &session.CompactionData{ID: "c", ShadowedSeqs: []uint64{99}, ShadowedTokenCount: 1, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "summary"}}, Provider: "openai", Model: "m"}}}
	for name, test := range map[string]struct {
		events []session.Event
		want   int
	}{
		"root session":                    {[]session.Event{task}, 0},
		"first step":                      {[]session.Event{descriptor, task}, 1},
		"sandbox snapshot is independent": {[]session.Event{descriptor, task, policyEvent}, 1},
		"visible after resume":            {[]session.Event{descriptor, task, contextEvent}, 0},
		"hidden by compaction":            {[]session.Event{descriptor, task, contextEvent, summary}, 1},
	} {
		messages, err := h.service.StepContext(context.Background(), agent.ContextRequest{Events: test.events})
		if err != nil || len(messages) != test.want {
			t.Errorf("%s: StepContext = %d messages, %v", name, len(messages), err)
			continue
		}
		if test.want == 1 && (messages[0].Source.Kind != SourceRuntimeContext || session.Text(messages[0]) != delegationContext) {
			t.Errorf("%s: message = %#v", name, messages[0])
		}
	}
	if _, err := h.service.StepContext(context.Background(), agent.ContextRequest{Events: []session.Event{descriptor, task, broken}}); err == nil {
		t.Fatal("an invalid surface contributed context")
	}
}

func TestService_DelegationNeedsAParentRequestRoute(t *testing.T) {
	h := startHarness(t)
	// A parent that never sent a request has no route to hand down.
	idle, err := h.registry.Create(context.Background(), agent.CreateRequest{ParentID: "root", Mode: session.SubagentContinuable, Provider: session.SubagentSpawn, Route: testRoute, Label: "idle", Depth: 1, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	journal := &memoryJournal{}
	request := StartRequest{ParentID: idle.Status().SessionID, Journal: journal, Turn: 1, Step: 1, Description: "d", Prompt: "p"}
	if _, err := h.service.Run(context.Background(), request); code(err) != CodeInvalidRequest || !strings.Contains(err.Error(), "inherit its route") {
		t.Fatalf("Run without a parent request = %v", err)
	}
	if len(journal.records) != 0 {
		t.Fatalf("rejected delegation recorded %#v", journal.records)
	}
}

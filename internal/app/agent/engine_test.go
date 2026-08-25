package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type memoryCredentialStore struct{}

func (memoryCredentialStore) Resolve(context.Context, string, string) (llm.Credential, error) {
	return llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "test"}, nil
}
func (memoryCredentialStore) Modify(_ context.Context, _ string, update func(*llm.Credential) (*llm.Credential, error)) (llm.Credential, error) {
	value, err := update(nil)
	if value == nil {
		return llm.Credential{}, err
	}
	return *value, err
}
func (memoryCredentialStore) Delete(context.Context, string) error { return nil }
func (memoryCredentialStore) List(context.Context) ([]llm.AccountInfo, error) {
	return nil, nil
}

type modelAction struct {
	completion llm.Completion
	chunks     []session.AssistantChunk
	err        error
	wait       <-chan struct{}
	started    chan<- struct{}
	panic      bool
}

type scriptedModel struct {
	mu      sync.Mutex
	actions []modelAction
	seen    []llm.Request
}

func (model *scriptedModel) Info() llm.ModelInfo {
	return llm.ModelInfo{Provider: "openai", ID: "gpt-5.6-luna", ContextWindow: 200_000, Vision: true, Tools: true}
}
func (*scriptedModel) CredentialEnv() string { return "OPENAI_API_KEY" }
func (*scriptedModel) Refresh(_ context.Context, credential llm.Credential) (llm.Credential, error) {
	return credential, nil
}
func (model *scriptedModel) Stream(ctx context.Context, _ llm.Credential, request llm.Request, emit llm.Emit) (llm.Completion, error) {
	model.mu.Lock()
	model.seen = append(model.seen, request)
	if len(model.actions) == 0 {
		model.mu.Unlock()
		return llm.Completion{}, errors.New("script exhausted")
	}
	action := model.actions[0]
	model.actions = model.actions[1:]
	model.mu.Unlock()
	if action.panic {
		panic("scripted panic")
	}
	if action.started != nil {
		action.started <- struct{}{}
	}
	if action.wait != nil {
		select {
		case <-ctx.Done():
			return llm.Completion{}, ctx.Err()
		case <-action.wait:
		}
	}
	for _, chunk := range action.chunks {
		if err := emit(chunk); err != nil {
			return llm.Completion{}, err
		}
	}
	return action.completion, action.err
}

type scriptedProvider struct {
	model      *scriptedModel
	prepareErr error
}

func (*scriptedProvider) ID() string { return "openai" }
func (provider *scriptedProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{provider.model.Info()}
}
func (provider *scriptedProvider) Prepare(model string) (llm.PreparedModel, error) {
	if provider.prepareErr != nil {
		return nil, provider.prepareErr
	}
	if model != "gpt-5.6-luna" {
		return nil, llm.ErrUnknownModel
	}
	return provider.model, nil
}
func (*scriptedProvider) AuthMethods() []llm.AuthMethod { return nil }
func (*scriptedProvider) Login(context.Context, string, llm.AuthInteraction) (llm.Credential, error) {
	return llm.Credential{}, llm.ErrInvalidConfig
}

type engineApprover struct{}

func (engineApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

type engineTool struct {
	name   string
	output string
	err    error
	seen   []appTool.Execution
}

func (tool *engineTool) Definition() session.ToolDefinition {
	return session.ToolDefinition{Name: tool.name, Description: "test tool", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (*engineTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyExclusive }
func (*engineTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool *engineTool) Execute(_ context.Context, execution appTool.Execution) (string, error) {
	tool.seen = append(tool.seen, execution)
	return tool.output, tool.err
}

type engineHarness struct {
	engine        *Engine
	model         *scriptedModel
	provider      *scriptedProvider
	llm           *llm.Runtime
	tools         *appTool.Runtime
	retry         *retry.Service
	compaction    *compaction.Service
	prompt        *prompt.Assembler
	settings      *settings.Service
	settingsScope *plugin.Scope
	toolScope     *plugin.Scope
	promptScope   *plugin.Scope
	scopes        []*plugin.Scope
}

func startEngineHarness(t *testing.T, maxSteps int, actions ...modelAction) *engineHarness {
	t.Helper()
	harness := &engineHarness{model: &scriptedModel{actions: slices.Clone(actions)}}
	harness.provider = &scriptedProvider{model: harness.model}
	harness.settings = settings.New()
	harness.settingsScope = &plugin.Scope{}
	if err := harness.settings.Start(context.Background(), harness.settingsScope); err != nil {
		t.Fatal(err)
	}
	harness.llm, _ = llm.New(memoryCredentialStore{})
	llmScope, providerScope := &plugin.Scope{}, &plugin.Scope{}
	if err := harness.llm.Start(context.Background(), llmScope); err != nil {
		t.Fatal(err)
	}
	if err := harness.llm.Register(harness.provider, providerScope); err != nil {
		t.Fatal(err)
	}
	harness.tools, _ = appTool.New(engineApprover{})
	harness.toolScope = &plugin.Scope{}
	if err := harness.tools.Start(context.Background(), harness.toolScope); err != nil {
		t.Fatal(err)
	}
	harness.retry, _ = retry.New(harness.settings)
	retryScope := &plugin.Scope{}
	if err := harness.retry.Start(context.Background(), retryScope); err != nil {
		t.Fatal(err)
	}
	harness.compaction, _ = compaction.New(harness.llm, harness.settings)
	compactionScope := &plugin.Scope{}
	if err := harness.compaction.Start(context.Background(), compactionScope); err != nil {
		t.Fatal(err)
	}
	harness.prompt = prompt.New()
	harness.promptScope = &plugin.Scope{}
	if err := harness.prompt.Start(context.Background(), harness.promptScope); err != nil {
		t.Fatal(err)
	}
	var err error
	harness.engine, err = NewEngine(harness.llm, harness.tools, harness.retry, harness.compaction, harness.prompt, harness.settings, EngineConfig{MaxSteps: maxSteps})
	if err != nil {
		t.Fatal(err)
	}
	engineScope := &plugin.Scope{}
	if err := harness.engine.Start(context.Background(), engineScope); err != nil {
		t.Fatal(err)
	}
	harness.scopes = []*plugin.Scope{engineScope, harness.promptScope, compactionScope, retryScope, harness.toolScope, providerScope, llmScope, harness.settingsScope}
	t.Cleanup(func() {
		for _, scope := range harness.scopes {
			_ = scope.Close(context.Background())
		}
	})
	return harness
}

func agentMessage(role session.MessageRole, text string) session.Message {
	return session.Message{Role: role, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

func assistantCompletion(text string, calls ...session.ToolCall) llm.Completion {
	return llm.Completion{Message: session.Message{Role: session.RoleAssistant, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}, Calls: calls, Usage: &session.TokenUsage{InputTokens: 3, OutputTokens: 2}}
}

func turnJournal() (*journal, *memoryLog) {
	log := &memoryLog{header: session.Header{SessionID: "session", Cwd: "/workspace"}, path: "/session.jsonl"}
	return newJournal(log), log
}

func TestEngine_ValidatesLifecycleAndDefaults(t *testing.T) {
	harness := startEngineHarness(t, 0, modelAction{completion: assistantCompletion("done")})
	if harness.engine.ID() != "agent-engine" || harness.engine.maxSteps != 32 {
		t.Fatalf("engine identity/defaults = %q/%d", harness.engine.ID(), harness.engine.maxSteps)
	}
	if err := harness.engine.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double start error = %v", err)
	}
	for _, config := range []EngineConfig{{MaxSteps: -1}, {MaxSteps: 257}} {
		if _, err := NewEngine(harness.llm, harness.tools, harness.retry, harness.compaction, harness.prompt, harness.settings, config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid max steps error = %v", err)
		}
	}
	if _, err := NewEngine(nil, harness.tools, harness.retry, harness.compaction, harness.prompt, harness.settings, EngineConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	inactive, err := NewEngine(harness.llm, harness.tools, harness.retry, harness.compaction, harness.prompt, harness.settings, EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := inactive.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope error = %v", err)
	}
	journal, _ := turnJournal()
	result := inactive.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "hello")})
	if !errors.Is(result.Err, ErrNotRunning) || result.Outcome != session.OutcomeError {
		t.Fatalf("inactive result = %+v", result)
	}
}

func TestEngine_CompletesStreamingTurnWithDurableOrder(t *testing.T) {
	harness := startEngineHarness(t, 3, modelAction{
		chunks:     []session.AssistantChunk{{Kind: session.ChunkText, Text: "stream"}},
		completion: assistantCompletion("final"),
	})
	journal, log := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "hello"), persona: "tester", drain: func() []session.Message { return nil }})
	if result.SessionID != "session" || result.Turn != 1 || result.Outcome != session.OutcomeCompleted || result.Text != "final" || result.Err != nil {
		t.Fatalf("result = %+v", result)
	}
	want := []session.RecordType{
		session.RecordTurnStart, session.RecordUserMessage, session.RecordStepStart, session.RecordRequestHeader,
		session.RecordAssistantChunk, session.RecordAssistantMessage, session.RecordStepEnd, session.RecordTurnEnd,
	}
	if got := recordTypes(log.events); !slices.Equal(got, want) {
		t.Fatalf("record order = %#v", got)
	}
	message := log.events[5].Record.Message
	if message.Source.Kind != "provider" || message.Source.Plugin != "openai" || log.events[6].Record.Usage.InputTokens != 3 {
		t.Fatalf("assistant/usage = %#v / %#v", message, log.events[6].Record.Usage)
	}
	if len(harness.model.seen) != 1 || harness.model.seen[0].Purpose != "agent" || !strings.Contains(harness.model.seen[0].System, "Assigned role") {
		t.Fatalf("model request = %#v", harness.model.seen)
	}
}

func TestEngine_ExecutesToolsSteersAndNextTurn(t *testing.T) {
	call := session.ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{"value":1}`)}
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("using tool", call)},
		modelAction{completion: assistantCompletion("after tool")},
		modelAction{completion: assistantCompletion("second turn")},
	)
	candidate := &engineTool{name: "inspect", output: "tool output"}
	providerScope := &plugin.Scope{}
	if err := harness.tools.Register(candidate, providerScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = providerScope.Close(context.Background()) })
	journal, log := turnJournal()
	steer := agentMessage(session.RoleUser, "new direction")
	drains := 0
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "first"), tools: []string{"inspect"}, delegated: true, drain: func() []session.Message {
		drains++
		if drains == 1 {
			return []session.Message{steer}
		}
		return nil
	}})
	if result.Outcome != session.OutcomeCompleted || result.Text != "after tool" || len(candidate.seen) != 1 || !candidate.seen[0].Delegated {
		t.Fatalf("tool turn = %+v, seen = %#v", result, candidate.seen)
	}
	types := recordTypes(log.events)
	for _, required := range []session.RecordType{session.RecordToolCall, session.RecordToolResult} {
		if !slices.Contains(types, required) {
			t.Fatalf("records missing %s: %#v", required, types)
		}
	}
	if countType(types, session.RecordStepStart) != 2 || countType(types, session.RecordUserMessage) != 2 {
		t.Fatalf("tool/steer records = %#v", types)
	}
	second := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "later"), drain: func() []session.Message { return nil }})
	if second.Turn != 2 || second.Outcome != session.OutcomeCompleted || second.Text != "second turn" {
		t.Fatalf("second turn = %+v", second)
	}
}

func TestEngine_StopsAtStepLimitAndClosesOpenScopes(t *testing.T) {
	call := session.ToolCall{ID: "call", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 1, modelAction{completion: assistantCompletion("again", call)})
	candidate := &engineTool{name: "inspect", output: "ok"}
	scope := &plugin.Scope{}
	if err := harness.tools.Register(candidate, scope); err != nil {
		t.Fatal(err)
	}
	journal, log := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "loop"), drain: func() []session.Message { return nil }})
	if result.Outcome != session.OutcomeStepLimit || result.Err != nil || recordTypes(log.events)[len(log.events)-1] != session.RecordTurnEnd {
		t.Fatalf("step limit result = %+v records=%#v", result, recordTypes(log.events))
	}
}

func TestEngine_ClassifiesCancellationProviderFailureAndPanic(t *testing.T) {
	for _, test := range []struct {
		name    string
		action  modelAction
		outcome session.TurnOutcome
		match   string
	}{
		{name: "cancelled", action: modelAction{err: context.Canceled}, outcome: session.OutcomeCanceled, match: "canceled"},
		{name: "provider", action: modelAction{err: &llm.Error{Code: llm.ErrorInvalid, Provider: "openai"}}, outcome: session.OutcomeError, match: "invalid_request"},
		{name: "panic", action: modelAction{panic: true}, outcome: session.OutcomeError, match: "panicked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := startEngineHarness(t, 1, test.action)
			journal, log := turnJournal()
			result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
			if result.Outcome != test.outcome || result.Err == nil || !strings.Contains(result.Err.Error(), test.match) {
				t.Fatalf("result = %+v", result)
			}
			types := recordTypes(log.events)
			if countType(types, session.RecordStepEnd) != 1 || countType(types, session.RecordTurnEnd) != 1 {
				t.Fatalf("unclosed records = %#v", types)
			}
		})
	}
}

func TestEngine_ValidatesMessagesAndHelperCopies(t *testing.T) {
	data := []byte("image")
	digest := sha256.Sum256(data)
	image := &session.Image{ID: "image-1", Name: "image.png", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(data), SHA256: fmt.Sprintf("%x", digest), Width: 1, Height: 1}
	message := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentImage, Image: image}}}
	if !validUserMessage(message) {
		t.Fatal("valid image-only message rejected")
	}
	copyMessage := cloneMessage(message)
	copyMessage.Content[0].Image.Data = "changed"
	if message.Content[0].Image.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatal("cloneMessage aliased image")
	}
	for _, invalid := range []session.Message{
		{}, agentMessage(session.RoleAssistant, "wrong role"),
		{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "  "}}},
	} {
		if validUserMessage(invalid) {
			t.Fatalf("accepted invalid message %#v", invalid)
		}
	}
	if got := nextTurn([]session.Event{{Record: session.Record{Type: session.RecordTurnStart, Turn: 3}}, {Record: session.Record{Type: session.RecordTurnStart, Turn: 2}}}); got != 4 {
		t.Fatalf("nextTurn = %d", got)
	}
	document := settings.Defaults()
	if got := findModel(document, "openai", "gpt-5.6-luna"); got.ContextWindow == 0 {
		t.Fatal("known model not found")
	}
	if got := findModel(document, "openai", "missing"); got.ID != "missing" || got.ContextWindow != 0 {
		t.Fatalf("unknown model = %#v", got)
	}
	if outcomeFor(context.DeadlineExceeded) != session.OutcomeCanceled || outcomeFor(errors.New("x")) != session.OutcomeError {
		t.Fatal("outcomeFor misclassified error")
	}
}

func recordTypes(events []session.Event) []session.RecordType {
	types := make([]session.RecordType, len(events))
	for index, event := range events {
		types[index] = event.Record.Type
	}
	return types
}

func countType(types []session.RecordType, target session.RecordType) int {
	count := 0
	for _, candidate := range types {
		if candidate == target {
			count++
		}
	}
	return count
}

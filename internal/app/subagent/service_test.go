package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	jsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type testStore struct{}

func (testStore) Resolve(context.Context, string, string) (llm.Credential, error) {
	return llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "test"}, nil
}
func (testStore) Modify(_ context.Context, _ string, update func(*llm.Credential) (*llm.Credential, error)) (llm.Credential, error) {
	value, err := update(nil)
	if value == nil {
		return llm.Credential{}, err
	}
	return *value, err
}
func (testStore) Delete(context.Context, string) error            { return nil }
func (testStore) List(context.Context) ([]llm.AccountInfo, error) { return nil, nil }

type testAction struct {
	text    string
	calls   []session.ToolCall
	err     error
	wait    <-chan struct{}
	started chan<- struct{}
}

type testModel struct {
	mu      sync.Mutex
	actions []testAction
	seen    []llm.Request
}

func (*testModel) Info() llm.ModelInfo {
	return llm.ModelInfo{Provider: "openai", ID: "gpt-5.6-luna", ContextWindow: 200_000, Vision: true, Tools: true}
}
func (*testModel) CredentialEnv() string { return "OPENAI_API_KEY" }
func (*testModel) Refresh(_ context.Context, credential llm.Credential) (llm.Credential, error) {
	return credential, nil
}
func (model *testModel) Stream(ctx context.Context, _ llm.Credential, request llm.Request, _ llm.Emit) (llm.Completion, error) {
	model.mu.Lock()
	model.seen = append(model.seen, request)
	if len(model.actions) == 0 {
		model.mu.Unlock()
		return llm.Completion{}, &llm.Error{Code: llm.ErrorInvalid, Provider: "openai"}
	}
	action := model.actions[0]
	model.actions = model.actions[1:]
	model.mu.Unlock()
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
	message := session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider", Plugin: "openai"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: action.text}}}
	return llm.Completion{Message: message, Calls: slices.Clone(action.calls)}, action.err
}

type testProvider struct{ model *testModel }

func (*testProvider) ID() string                       { return "openai" }
func (provider *testProvider) Models() []llm.ModelInfo { return []llm.ModelInfo{provider.model.Info()} }
func (provider *testProvider) Prepare(id string) (llm.PreparedModel, error) {
	if id != "gpt-5.6-luna" {
		return nil, llm.ErrUnknownModel
	}
	return provider.model, nil
}
func (*testProvider) AuthMethods() []llm.AuthMethod { return nil }
func (*testProvider) Login(context.Context, string, llm.AuthInteraction) (llm.Credential, error) {
	return llm.Credential{}, llm.ErrInvalidConfig
}

type testApprover struct{}

func (testApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

type largeResultTool struct{ output string }

func (largeResultTool) Definition() session.ToolDefinition {
	return session.ToolDefinition{Name: "test_tool", Description: "return test output", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (largeResultTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyExclusive }
func (largeResultTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool largeResultTool) Execute(context.Context, appTool.Execution) (string, error) {
	return tool.output, nil
}

type serviceHarness struct {
	registry     *agent.Registry
	root         *agent.Agent
	model        *testModel
	tools        *appTool.Runtime
	managerScope *plugin.Scope
	scopes       []*plugin.Scope
}

func startServiceHarness(t *testing.T, actions ...testAction) *serviceHarness {
	t.Helper()
	harness := &serviceHarness{model: &testModel{actions: slices.Clone(actions)}}
	start := func(component plugin.Plugin) *plugin.Scope {
		t.Helper()
		scope := &plugin.Scope{}
		if err := component.Start(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		harness.scopes = append([]*plugin.Scope{scope}, harness.scopes...)
		return scope
	}
	configuration := settings.New()
	start(configuration)
	modelRuntime, _ := llm.New(testStore{})
	start(modelRuntime)
	providerScope := &plugin.Scope{}
	if err := modelRuntime.Register(&testProvider{model: harness.model}, providerScope); err != nil {
		t.Fatal(err)
	}
	harness.scopes = append([]*plugin.Scope{providerScope}, harness.scopes...)
	harness.tools, _ = appTool.New(testApprover{})
	start(harness.tools)
	retries, _ := retry.New(configuration)
	start(retries)
	compactor, _ := compaction.New(modelRuntime, configuration)
	start(compactor)
	assembler := prompt.New()
	start(assembler)
	engine, err := agent.NewEngine(modelRuntime, harness.tools, retries, compactor, assembler, configuration, agent.EngineConfig{MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	start(engine)
	manager, err := jsonl.New(jsonl.Config{Root: filepath.Join(t.TempDir(), "sessions"), CompositionID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	harness.managerScope = start(manager)
	policies := approval.New()
	start(policies)
	harness.registry, err = agent.NewRegistry(manager, engine, policies, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start(harness.registry)
	harness.root, err = harness.registry.Create(context.Background(), agent.CreateRequest{SessionID: "root", Tools: []string{"test_tool"}, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, scope := range harness.scopes {
			_ = scope.Close(context.Background())
		}
	})
	return harness
}

func startService(t *testing.T, registry *agent.Registry) (*Service, *plugin.Scope) {
	t.Helper()
	service, err := New(registry)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, scope
}

func TestService_ValidatesLifecycleAndRequests(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil registry error = %v", err)
	}
	harness := startServiceHarness(t)
	inactive, _ := New(harness.registry)
	if _, err := inactive.Spawn(context.Background(), validSpawn("root")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive Spawn() error = %v", err)
	}
	if _, err := inactive.List(""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive List() error = %v", err)
	}
	if _, err := inactive.Report("missing"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive Report() error = %v", err)
	}
	service, scope := startService(t, harness.registry)
	if service.ID() != "subagents" {
		t.Fatalf("ID = %q", service.ID())
	}
	if err := service.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("double start error = %v", err)
	}
	for _, request := range []SpawnRequest{
		{}, {ParentSessionID: "root", Label: " ", Mode: "one-shot", Task: "task"},
		{ParentSessionID: "root", Label: strings.Repeat("x", 129), Mode: "one-shot", Task: "task"},
		{ParentSessionID: "root", Label: "label", Mode: "one-shot", Task: " "},
		{ParentSessionID: "root", Label: "label", Mode: "one-shot", Task: strings.Repeat("x", session.MaxTextBytes+1)},
		{ParentSessionID: "root", Label: "label", Mode: "bad", Task: "task"},
		{ParentSessionID: "root", Label: "label", Mode: "one-shot", Task: "task", Persona: strings.Repeat("x", 4097)},
		{ParentSessionID: "root", Label: "label", Mode: "one-shot", Task: "task", Tools: make([]string, 33)},
	} {
		if _, err := service.Spawn(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("request %+v error = %v", request, err)
		}
	}
	request := validSpawn("missing")
	if _, err := service.Spawn(context.Background(), request); !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("missing parent error = %v", err)
	}
	if _, err := service.Wait(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Wait() error = %v", err)
	}
	if err := service.Interrupt("root", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Interrupt() error = %v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped List() error = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	failed, _ := New(harness.registry)
	if err := failed.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope error = %v", err)
	}
}

func TestService_SpawnsWaitsReportsListsAndFollowsUp(t *testing.T) {
	harness := startServiceHarness(t,
		testAction{text: "one-shot report"},
		testAction{text: "initial report"},
		testAction{text: "followup report"},
	)
	service, _ := startService(t, harness.registry)
	if _, err := service.Followup(context.Background(), "root", "missing", "task"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing followup error = %v", err)
	}
	one, err := service.Spawn(context.Background(), SpawnRequest{ParentSessionID: "root", Label: "one", Mode: "one-shot", Task: "do one", Persona: "focused", Tools: []string{"read_file"}})
	if err != nil || one.ParentID != "root" || one.Mode != "one-shot" || one.Depth != 1 {
		t.Fatalf("one-shot spawn = %+v, %v", one, err)
	}
	one, err = service.Wait(context.Background(), one.SessionID)
	if err != nil || one.Last.Text != "one-shot report" || one.Last.Outcome != session.OutcomeCompleted {
		t.Fatalf("one-shot wait = %+v, %v", one, err)
	}
	if _, err := service.Followup(context.Background(), "root", one.SessionID, "again"); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("one-shot followup error = %v", err)
	}

	continuable, err := service.Spawn(context.Background(), SpawnRequest{ParentSessionID: "root", Label: "continue", Mode: "continuable", Task: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	continuable, err = service.Wait(context.Background(), continuable.SessionID)
	if err != nil || continuable.Last.Text != "initial report" {
		t.Fatalf("continuable wait = %+v, %v", continuable, err)
	}
	if _, err := service.Followup(context.Background(), "wrong", continuable.SessionID, "next"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("wrong caller error = %v", err)
	}
	for _, task := range []string{" ", strings.Repeat("x", session.MaxTextBytes+1)} {
		if _, err := service.Followup(context.Background(), "root", continuable.SessionID, task); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid task error = %v", err)
		}
	}
	continuable, err = service.Followup(context.Background(), "root", continuable.SessionID, "next")
	if err != nil || continuable.Last.Text != "followup report" || continuable.Last.Turn != 2 {
		t.Fatalf("followup = %+v, %v", continuable, err)
	}
	reported, err := service.Report(continuable.SessionID)
	if err != nil || reported.Last.Text != "followup report" {
		t.Fatalf("report = %+v, %v", reported, err)
	}
	all, err := service.List("")
	if err != nil || len(all) != 2 || all[0].SessionID > all[1].SessionID {
		t.Fatalf("all = %#v, %v", all, err)
	}
	children, err := service.List("root")
	if err != nil || len(children) != 2 {
		t.Fatalf("children = %#v, %v", children, err)
	}
	other, err := service.List("other")
	if err != nil || len(other) != 0 {
		t.Fatalf("other = %#v, %v", other, err)
	}
}

func TestService_FollowupCancellationAfterSubmission(t *testing.T) {
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	harness := startServiceHarness(t,
		testAction{text: "initial"},
		testAction{text: "late followup", started: started, wait: gate},
	)
	service, _ := startService(t, harness.registry)
	info, err := service.Spawn(context.Background(), SpawnRequest{ParentSessionID: "root", Label: "continue", Mode: "continuable", Task: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Wait(context.Background(), info.SessionID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := service.Followup(ctx, "root", info.SessionID, "followup")
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Followup() error = %v", err)
	}
	close(gate)
	child, err := harness.registry.Find(info.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestService_ContainsForkCreateSubmitAndPublicationFailures(t *testing.T) {
	previousSubmit, previousPublish := beforeSubmit, beforePublish
	t.Cleanup(func() { beforeSubmit, beforePublish = previousSubmit, previousPublish })

	t.Run("fork surface", func(t *testing.T) {
		harness := startServiceHarness(t)
		service, _ := startService(t, harness.registry)
		if err := harness.managerScope.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		request := validSpawn("root")
		request.Fork = true
		if _, err := service.Spawn(context.Background(), request); err == nil {
			t.Fatal("closed parent surface was accepted")
		}
	})

	t.Run("child create", func(t *testing.T) {
		harness := startServiceHarness(t)
		service, _ := startService(t, harness.registry)
		request := validSpawn("root")
		request.Tools = []string{strings.Repeat("x", 257)}
		if _, err := service.Spawn(context.Background(), request); err == nil {
			t.Fatal("invalid child descriptor was accepted")
		}
	})

	t.Run("child submit", func(t *testing.T) {
		harness := startServiceHarness(t)
		service, _ := startService(t, harness.registry)
		beforeSubmit = func(child *agent.Agent) { _ = harness.registry.Close(context.Background(), child.Status().SessionID) }
		if _, err := service.Spawn(context.Background(), validSpawn("root")); !errors.Is(err, agent.ErrNotRunning) {
			t.Fatalf("submit rollback error = %v", err)
		}
		beforeSubmit = previousSubmit
	})

	t.Run("service stops before publication", func(t *testing.T) {
		harness := startServiceHarness(t, testAction{text: "report"})
		service, scope := startService(t, harness.registry)
		beforePublish = func() { _ = scope.Close(context.Background()) }
		if _, err := service.Spawn(context.Background(), validSpawn("root")); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("publication rollback error = %v", err)
		}
		beforePublish = previousPublish
	})
}

func TestService_StopCancelsMonitorAndJoinsChildCloseFailure(t *testing.T) {
	previousClose := closeAgent
	t.Cleanup(func() { closeAgent = previousClose })
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	harness := startServiceHarness(t, testAction{text: "late", started: started, wait: gate})
	service, scope := startService(t, harness.registry)
	info, err := service.Spawn(context.Background(), validSpawn("root"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	closeFailure := errors.New("close child")
	closeAgent = func(*agent.Registry, context.Context, string) error { return closeFailure }
	if err := scope.Close(context.Background()); !errors.Is(err, closeFailure) {
		t.Fatalf("stop error = %v", err)
	}
	if _, err := service.Report(info.SessionID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped report error = %v", err)
	}
	closeAgent = previousClose
	if err := harness.registry.Close(context.Background(), info.SessionID); err != nil {
		t.Fatal(err)
	}
}

func TestService_WaitCancellationInterruptAndStop(t *testing.T) {
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	harness := startServiceHarness(t, testAction{text: "late", started: started, wait: gate})
	service, scope := startService(t, harness.registry)
	info, err := service.Spawn(context.Background(), validSpawn("root"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Wait(ctx, info.SessionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Wait() error = %v", err)
	}
	if _, err := service.Followup(ctx, "root", info.SessionID, "queued"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Followup() error = %v", err)
	}
	if err := service.Interrupt("wrong", info.SessionID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("wrong interrupt error = %v", err)
	}
	if err := service.Interrupt("root", info.SessionID); err != nil {
		t.Fatal(err)
	}
	waited, err := service.Wait(context.Background(), info.SessionID)
	if err != nil || waited.Last.Outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted wait = %+v, %v", waited, err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.registry.Find(info.SessionID); !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("child remained mounted: %v", err)
	}
}

func TestService_ForkMessageAndMaximumDepth(t *testing.T) {
	harness := startServiceHarness(t, testAction{text: "parent answer"}, testAction{text: "fork report"})
	result, err := harness.root.Submit(context.Background(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "parent question"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-result
	service, _ := startService(t, harness.registry)
	info, err := service.Spawn(context.Background(), SpawnRequest{ParentSessionID: "root", Label: "fork", Mode: "one-shot", Task: "use context", Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Wait(context.Background(), info.SessionID); err != nil {
		t.Fatal(err)
	}
	if len(harness.model.seen) < 2 {
		t.Fatalf("model requests = %#v", harness.model.seen)
	}
	surface := harness.model.seen[1].Surface
	if len(surface) == 0 || !strings.Contains(session.Text(*surface[len(surface)-1].Message), "Forked parent context") || !strings.Contains(session.Text(*surface[len(surface)-1].Message), "parent question") {
		t.Fatalf("fork surface = %#v", surface)
	}

	parentID := "root"
	for depth := 1; depth <= maxDelegationDepth; depth++ {
		child, createErr := harness.registry.Create(context.Background(), agent.CreateRequest{SessionID: "depth-" + string(rune('0'+depth)), ParentID: parentID, Label: "depth", Depth: depth, Mode: "continuable", Create: true})
		if createErr != nil {
			t.Fatal(createErr)
		}
		parentID = child.Status().SessionID
	}
	request := validSpawn(parentID)
	if _, err := service.Spawn(context.Background(), request); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "maximum delegation depth") {
		t.Fatalf("depth error = %v", err)
	}
}

func TestDelegatedMessage_IncludesToolFactsAndBoundsForkContext(t *testing.T) {
	call := session.ToolCall{ID: "call", Name: "test_tool", Arguments: json.RawMessage(`{}`)}
	harness := startServiceHarness(t,
		testAction{text: "calling", calls: []session.ToolCall{call}},
		testAction{text: "finished"},
	)
	toolScope := &plugin.Scope{}
	if err := harness.tools.Register(largeResultTool{output: strings.Repeat("r", session.MaxTextBytes/2+1024)}, toolScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = toolScope.Close(context.Background()) })
	result, err := harness.root.Submit(context.Background(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "run tool"}}})
	if err != nil {
		t.Fatal(err)
	}
	if turn := <-result; turn.Outcome != session.OutcomeCompleted {
		t.Fatalf("parent turn = %+v", turn)
	}
	message, err := delegatedMessage(context.Background(), harness.root, SpawnRequest{Task: strings.Repeat("t", 200_000), Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	text := session.Text(message)
	if len(text) != session.MaxTextBytes || !strings.Contains(text, "Assigned task") {
		t.Fatalf("bounded fork text length=%d prefix=%q", len(text), text[:min(len(text), 80)])
	}
}

func validSpawn(parent string) SpawnRequest {
	return SpawnRequest{ParentSessionID: parent, Label: "worker", Mode: "continuable", Task: "task"}
}

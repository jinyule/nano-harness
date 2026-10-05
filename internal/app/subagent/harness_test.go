package subagent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const waitLimit = 20 * time.Second

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

// rule answers requests whose latest user message contains match: first
// while no tool result follows that message, then afterwards.
type rule struct {
	match string
	first reply
	then  reply
}

// reply is one scripted completion. hold makes the agent call the hold
// tool; block makes the stream wait until its request is cancelled.
type reply struct {
	text  string
	hold  bool
	block bool
}

type testModel struct {
	mu    sync.Mutex
	rules []rule
	seen  []llm.Request
	calls int
	// blocked receives one value each time a blocking reply starts.
	blocked chan struct{}
}

func (*testModel) Info() llm.ModelInfo {
	return llm.ModelInfo{Provider: "openai", ID: "gpt-5.6-luna", ContextWindow: 200_000, Vision: true, Tools: true}
}
func (*testModel) CredentialEnv() string { return "OPENAI_API_KEY" }
func (*testModel) Refresh(_ context.Context, credential llm.Credential) (llm.Credential, error) {
	return credential, nil
}
func (*testModel) Search(context.Context, llm.Credential, llm.SearchRequest) (llm.SearchResult, error) {
	return llm.SearchResult{}, nil
}

// latestUser returns the text of the last user message and whether a tool
// result follows it.
func latestUser(request llm.Request) (string, bool) {
	answered := false
	for _, node := range slices.Backward(request.Surface) {
		if node.Result != nil {
			answered = true
		}
		if node.Message != nil && node.Message.Role == session.RoleUser {
			return session.Text(*node.Message), answered
		}
	}
	return "", answered
}

func (model *testModel) Stream(ctx context.Context, _ llm.Credential, request llm.Request, _ llm.Emit) (llm.Completion, error) {
	model.mu.Lock()
	model.seen = append(model.seen, request)
	text, answered := latestUser(request)
	chosen := reply{text: "ok"}
	for _, candidate := range model.rules {
		if strings.Contains(text, candidate.match) {
			chosen = candidate.first
			if answered {
				chosen = candidate.then
			}
			break
		}
	}
	model.calls++
	callID := "call-" + strconv.Itoa(model.calls)
	model.mu.Unlock()
	if chosen.block {
		model.blocked <- struct{}{}
		<-ctx.Done()
		return llm.Completion{}, ctx.Err()
	}
	message := session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider", Plugin: "openai"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: chosen.text}}}
	completion := llm.Completion{Message: message}
	if chosen.hold {
		completion.Calls = []session.ToolCall{{ID: callID, Name: "hold", Arguments: json.RawMessage(`{}`)}}
	}
	return completion, nil
}

// requests returns the requests whose latest user message contains match.
func (model *testModel) requests(match string) []llm.Request {
	model.mu.Lock()
	defer model.mu.Unlock()
	var matched []llm.Request
	for _, request := range model.seen {
		if text, _ := latestUser(request); strings.Contains(text, match) {
			matched = append(matched, request)
		}
	}
	return matched
}

type testProvider struct{ model *testModel }

func (*testProvider) ID() string                       { return "openai" }
func (provider *testProvider) Models() []llm.ModelInfo { return []llm.ModelInfo{provider.model.Info()} }
func (provider *testProvider) Prepare(string) (llm.PreparedModel, error) {
	return provider.model, nil
}
func (*testProvider) AuthMethods() []llm.AuthMethod { return nil }
func (*testProvider) Login(context.Context, string, llm.AuthInteraction) (llm.Credential, error) {
	return llm.Credential{}, llm.ErrInvalidConfig
}

type allowAll struct{}

func (allowAll) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

// held is one blocked hold call: the caller's invocation and the channel
// that ends it with a result text.
type held struct {
	invocation appTool.Invocation
	release    chan string
}

type noArguments struct{}

type harness struct {
	t            *testing.T
	model        *testModel
	manager      *jsonl.Manager
	registry     *agent.Registry
	root         *agent.Agent
	jobs         *job.Service
	jobScope     *plugin.Scope
	service      *Service
	serviceScope *plugin.Scope
	held         chan held
	scopes       []*plugin.Scope
}

func startHarness(t *testing.T, rules ...rule) *harness {
	t.Helper()
	h := &harness{t: t, model: &testModel{rules: rules, blocked: make(chan struct{}, 16)}, held: make(chan held, 16)}
	start := func(component plugin.Plugin) *plugin.Scope {
		t.Helper()
		scope := &plugin.Scope{}
		if err := component.Start(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		h.scopes = append([]*plugin.Scope{scope}, h.scopes...)
		return scope
	}
	t.Cleanup(func() {
		for _, scope := range h.scopes {
			_ = scope.Close(context.Background())
		}
	})
	configuration := settings.New()
	start(configuration)
	models, _ := llm.New(testStore{}, noImages{})
	start(models)
	providerScope := &plugin.Scope{}
	if err := models.Register(&testProvider{model: h.model}, providerScope); err != nil {
		t.Fatal(err)
	}
	h.scopes = append([]*plugin.Scope{providerScope}, h.scopes...)
	tools, _ := appTool.New(allowAll{})
	start(tools)
	hold := appTool.Define(appTool.Spec[noArguments]{
		Name: "hold", Description: "block until the test releases the call",
		Execute: func(ctx context.Context, invocation appTool.Invocation, _ noArguments) (appTool.Result, error) {
			call := held{invocation: invocation, release: make(chan string, 1)}
			h.held <- call
			select {
			case text := <-call.release:
				return appTool.Text(text), nil
			case <-ctx.Done():
				return appTool.Result{}, ctx.Err()
			}
		},
	})
	toolScope := &plugin.Scope{}
	if err := tools.Register(hold, toolScope); err != nil {
		t.Fatal(err)
	}
	h.scopes = append([]*plugin.Scope{toolScope}, h.scopes...)
	retries, _ := retry.New(configuration)
	start(retries)
	compactor, _ := compaction.New(models, configuration)
	start(compactor)
	assembler := prompt.New()
	start(assembler)
	planMode := plan.New()
	start(planMode)
	engine, err := agent.NewEngine(models, tools, retries, compactor, assembler, planMode, configuration, agent.EngineConfig{MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	start(engine)
	h.manager, err = jsonl.New(jsonl.Config{Root: filepath.Join(t.TempDir(), "sessions"), CompositionID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	start(h.manager)
	policies := approval.New()
	start(policies)
	h.registry, err = agent.NewRegistry(h.manager, engine, policies, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start(h.registry)
	h.root, err = h.registry.Create(context.Background(), agent.CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	h.jobs, _ = job.New(h.registry)
	h.service, err = New(h.registry, h.jobs, h.manager)
	if err != nil {
		t.Fatal(err)
	}
	h.serviceScope = start(h.service)
	h.jobScope = start(h.jobs)
	return h
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for an event")
		var zero T
		return zero
	}
}

// submit starts a root turn whose model holds; it returns the held root
// invocation and the turn result channel.
func (h *harness) submit(text string) (held, <-chan agent.TurnResult) {
	h.t.Helper()
	results, err := h.root.Submit(context.Background(), userText(text))
	if err != nil {
		h.t.Fatal(err)
	}
	return receive(h.t, h.held), results
}

func userText(text string) session.Message {
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

func start(call held, description, prompt string, fork bool) StartRequest {
	return StartRequest{ParentID: call.invocation.SessionID, Journal: call.invocation.Journal, Turn: call.invocation.Turn, Step: call.invocation.Step, Description: description, Prompt: prompt, Fork: fork}
}

// done returns a live child's release channel, or a closed channel when the
// child is already gone.
func (h *harness) done(id string) <-chan struct{} {
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	if current := h.service.children[id]; current != nil {
		return current.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

func (h *harness) events(id string) []session.Event {
	h.t.Helper()
	_, events, err := h.manager.Inspect(context.Background(), id)
	if err != nil {
		live, findErr := h.registry.Find(id)
		if findErr != nil {
			h.t.Fatal(err)
		}
		events, err = live.Events(context.Background())
		if err != nil {
			h.t.Fatal(err)
		}
	}
	return events
}

// messages returns the texts of user messages in events with source kind.
func messages(events []session.Event, kind string) []string {
	var texts []string
	for _, event := range events {
		if event.Record.Type == session.RecordUserMessage && event.Record.Message.Source.Kind == kind {
			texts = append(texts, session.Text(*event.Record.Message))
		}
	}
	return texts
}

func (h *harness) idle(current *agent.Agent) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	if err := current.WhenIdle(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func jsonlOpen(id string) jsonl.OpenOptions {
	return jsonl.OpenOptions{SessionID: id, Cwd: "/workspace"}
}

// noImages is an attachment store that holds no image.
type noImages struct{}

func (noImages) ReadImage(context.Context, session.Image) ([]byte, error) {
	return nil, session.ErrAttachmentMissing
}

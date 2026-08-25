package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type memoryRepository struct {
	mu      sync.Mutex
	logs    map[string]*memoryLog
	options []transcript.OpenOptions
	openErr error
	newLog  func(transcript.OpenOptions) *memoryLog
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{logs: map[string]*memoryLog{}}
}

func (repository *memoryRepository) OpenSession(_ context.Context, options transcript.OpenOptions) (transcript.Log, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.options = append(repository.options, options)
	if repository.openErr != nil {
		return nil, repository.openErr
	}
	if options.Create {
		if _, exists := repository.logs[options.SessionID]; exists {
			return nil, errors.New("session exists")
		}
		var log *memoryLog
		if repository.newLog != nil {
			log = repository.newLog(options)
		} else {
			log = &memoryLog{header: session.Header{
				SessionID: options.SessionID, Cwd: options.Cwd, ParentSessionID: options.ParentSessionID, DelegationDepth: options.DelegationDepth,
			}, path: "/sessions/" + options.SessionID + ".jsonl"}
		}
		repository.logs[options.SessionID] = log
		return log, nil
	}
	log := repository.logs[options.SessionID]
	if log == nil {
		return nil, errors.New("session missing")
	}
	return log, nil
}

func (repository *memoryRepository) Inspect(_ context.Context, id string) (session.Header, []session.Event, error) {
	repository.mu.Lock()
	log := repository.logs[id]
	repository.mu.Unlock()
	if log == nil {
		return session.Header{}, nil, errors.New("session missing")
	}
	events, err := log.Events(context.Background())
	return log.Header(), events, err
}

func (repository *memoryRepository) List(context.Context) ([]session.Header, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	headers := make([]session.Header, 0, len(repository.logs))
	for _, log := range repository.logs {
		headers = append(headers, log.header)
	}
	return headers, nil
}

type memoryPolicy struct {
	mu          sync.Mutex
	restored    map[string][]session.Event
	policies    map[string]session.ApprovalPolicy
	restoreErr  error
	setErr      error
	restoreHook func()
}

func newMemoryPolicy() *memoryPolicy {
	return &memoryPolicy{restored: map[string][]session.Event{}, policies: map[string]session.ApprovalPolicy{}}
}

func (policy *memoryPolicy) Restore(id string, events []session.Event) error {
	policy.mu.Lock()
	policy.restored[id] = slices.Clone(events)
	hook, err := policy.restoreHook, policy.restoreErr
	policy.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (policy *memoryPolicy) SetPolicy(_ context.Context, id string, _ appTool.Journal, value session.ApprovalPolicy) error {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	policy.policies[id] = value
	return policy.setErr
}

func startRegistry(t *testing.T, harness *engineHarness, repository *memoryRepository, policy *memoryPolicy) (*Registry, *plugin.Scope) {
	t.Helper()
	registry, err := NewRegistry(repository, harness.engine, policy, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := registry.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return registry, scope
}

func TestRegistry_ValidatesLifecycleAndLookup(t *testing.T) {
	harness := startEngineHarness(t, 1)
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	for _, test := range []struct {
		repository transcript.Repository
		engine     *Engine
		policy     PolicyService
		workspace  string
	}{
		{engine: harness.engine, policy: policy, workspace: "/workspace"},
		{repository: repository, policy: policy, workspace: "/workspace"},
		{repository: repository, engine: harness.engine, workspace: "/workspace"},
		{repository: repository, engine: harness.engine, policy: policy, workspace: " "},
	} {
		if _, err := NewRegistry(test.repository, test.engine, test.policy, test.workspace); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewRegistry() error = %v", err)
		}
	}
	registry, scope := startRegistry(t, harness, repository, policy)
	if registry.ID() != "agents" {
		t.Fatalf("ID = %q", registry.ID())
	}
	if err := registry.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double start error = %v", err)
	}
	if _, err := registry.Find("missing"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing Find() error = %v", err)
	}
	if err := registry.Close(context.Background(), "missing"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing Close() error = %v", err)
	}
	if statuses, err := registry.Statuses(); err != nil || len(statuses) != 0 {
		t.Fatalf("empty Statuses() = %#v, %v", statuses, err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Find("missing"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Find() error = %v", err)
	}
	if _, err := registry.Statuses(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Statuses() error = %v", err)
	}
	if _, err := registry.Create(context.Background(), CreateRequest{Create: true}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Create() error = %v", err)
	}

	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	inactive, _ := NewRegistry(repository, harness.engine, policy, "/workspace")
	if err := inactive.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope error = %v", err)
	}
}

func TestRegistry_CreatesRootDelegatedAndRestoredAgents(t *testing.T) {
	harness := startEngineHarness(t, 1)
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)

	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true, Label: "root"})
	if err != nil {
		t.Fatal(err)
	}
	rootStatus := root.Status()
	if rootStatus.SessionID != "root" || rootStatus.Mode != "continuable" || rootStatus.Depth != 0 {
		t.Fatalf("root status = %+v", rootStatus)
	}
	rootEvents, _ := root.Events(context.Background())
	if len(rootEvents) != 1 || rootEvents[0].Record.Type != session.RecordApprovalPolicy || rootEvents[0].Record.Approval.Policy != session.ApprovalAsk {
		t.Fatalf("root events = %#v", rootEvents)
	}

	child, err := registry.Create(context.Background(), CreateRequest{
		SessionID: "child", ParentID: "root", Label: "research", Mode: "one-shot", Persona: "focus", Tools: []string{"read_file"}, Depth: 1, Create: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	childEvents, _ := child.Events(context.Background())
	if len(childEvents) != 2 || childEvents[0].Record.Subagent.Mode != "one-shot" || childEvents[1].Record.Approval.Policy != session.ApprovalNever {
		t.Fatalf("child events = %#v", childEvents)
	}
	if err := registry.SetPolicy(context.Background(), "child", session.ApprovalAsk); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("delegated policy error = %v", err)
	}
	if err := registry.SetPolicy(context.Background(), "root", session.ApprovalNever); err != nil || policy.policies["root"] != session.ApprovalNever {
		t.Fatalf("root policy error = %v, policies = %#v", err, policy.policies)
	}
	if err := registry.SetPolicy(context.Background(), "missing", session.ApprovalAsk); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing policy error = %v", err)
	}

	statuses, err := registry.Statuses()
	if err != nil || len(statuses) != 2 || statuses[0].SessionID != "child" || statuses[1].SessionID != "root" {
		t.Fatalf("statuses = %#v, %v", statuses, err)
	}
	if _, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true}); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := registry.Close(context.Background(), "child"); err != nil || !repository.logs["child"].closed {
		t.Fatalf("child close error = %v", err)
	}

	// A resume restores immutable delegation metadata from the header and descriptor.
	restored, err := registry.Create(context.Background(), CreateRequest{SessionID: "child", Create: false, Mode: "continuable", Label: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	status := restored.Status()
	if status.ParentID != "root" || status.Depth != 1 || status.Mode != "one-shot" || status.Label != "research" {
		t.Fatalf("restored status = %+v", status)
	}
	if err := registry.Close(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistry_GeneratesIDsAndRejectsInvalidRequests(t *testing.T) {
	harness := startEngineHarness(t, 1)
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)

	generated, err := registry.Create(context.Background(), CreateRequest{Create: true})
	if err != nil || !strings.HasPrefix(generated.Status().SessionID, "session-") {
		t.Fatalf("generated agent = %+v, %v", generated, err)
	}
	for _, request := range []CreateRequest{
		{SessionID: "bad-mode", Mode: "bad", Create: true},
		{SessionID: "negative", Depth: -1, Create: true},
		{SessionID: "deep", ParentID: "root", Depth: 17, Create: true},
		{SessionID: "parent-zero", ParentID: "root", Depth: 0, Create: true},
		{SessionID: "depth-no-parent", Depth: 1, Create: true},
	} {
		if _, err := registry.Create(context.Background(), request); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("request %+v error = %v", request, err)
		}
	}
	repository.openErr = errors.New("open")
	if _, err := registry.Create(context.Background(), CreateRequest{SessionID: "open-error", Create: true}); !errors.Is(err, repository.openErr) {
		t.Fatalf("open error = %v", err)
	}
}

func TestRegistry_ContainsTranscriptAndPolicyFailures(t *testing.T) {
	harness := startEngineHarness(t, 1)
	for _, test := range []struct {
		name      string
		configure func(*memoryRepository, *memoryPolicy, string)
		create    bool
	}{
		{name: "events", configure: func(repository *memoryRepository, _ *memoryPolicy, id string) {
			repository.logs[id] = &memoryLog{header: session.Header{SessionID: id, Cwd: "/workspace"}, eventsErr: errors.New("events")}
		}},
		{name: "root policy append", create: true, configure: func(repository *memoryRepository, _ *memoryPolicy, _ string) {
			repository.newLog = func(options transcript.OpenOptions) *memoryLog {
				return &memoryLog{header: session.Header{SessionID: options.SessionID, Cwd: options.Cwd}, appendErr: errors.New("append")}
			}
		}},
		{name: "restore policy", configure: func(repository *memoryRepository, policy *memoryPolicy, id string) {
			repository.logs[id] = &memoryLog{header: session.Header{SessionID: id, Cwd: "/workspace"}}
			policy.restoreErr = errors.New("restore")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, policy := newMemoryRepository(), newMemoryPolicy()
			id := "existing"
			test.configure(repository, policy, id)
			registry, _ := startRegistry(t, harness, repository, policy)
			_, err := registry.Create(context.Background(), CreateRequest{SessionID: id, Create: test.create})
			if err == nil || !repository.logs[id].closed {
				t.Fatalf("Create() error = %v, closed = %v", err, repository.logs[id].closed)
			}
		})
	}
}

func TestRegistry_ContainsDelegationCreationAndPostOpenFailures(t *testing.T) {
	harness := startEngineHarness(t, 1)
	for _, test := range []struct {
		name      string
		failType  session.RecordType
		eventsTwo bool
	}{
		{name: "descriptor append", failType: session.RecordSubagentDescriptor},
		{name: "delegated policy append", failType: session.RecordApprovalPolicy},
		{name: "post-create events", eventsTwo: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, policy := newMemoryRepository(), newMemoryPolicy()
			repository.newLog = func(options transcript.OpenOptions) *memoryLog {
				log := &memoryLog{header: session.Header{SessionID: options.SessionID, Cwd: options.Cwd, ParentSessionID: options.ParentSessionID, DelegationDepth: options.DelegationDepth}}
				if test.failType != "" {
					log.appendHook = appendFailure(test.failType, 1, errors.New("append"))
				}
				if test.eventsTwo {
					log.eventsHook = func(call int) error {
						if call == 2 {
							return errors.New("events")
						}
						return nil
					}
				}
				return log
			}
			registry, _ := startRegistry(t, harness, repository, policy)
			request := CreateRequest{SessionID: "agent", Create: true}
			if !test.eventsTwo {
				request.ParentID, request.Depth = "root", 1
			}
			_, err := registry.Create(context.Background(), request)
			if err == nil || !repository.logs["agent"].closed {
				t.Fatalf("Create() error = %v, closed = %v", err, repository.logs["agent"].closed)
			}
		})
	}
}

func TestRegistry_ContainsScopePublicationRacesAndIDEntropyFailure(t *testing.T) {
	previousScope, previousRandom := newAgentScope, sessionRandomRead
	t.Cleanup(func() { newAgentScope, sessionRandomRead = previousScope, previousRandom })
	harness := startEngineHarness(t, 1)

	t.Run("session ID entropy", func(t *testing.T) {
		repository, policy := newMemoryRepository(), newMemoryPolicy()
		registry, _ := startRegistry(t, harness, repository, policy)
		sessionRandomRead = func([]byte) (int, error) { return 0, errors.New("entropy") }
		if _, err := registry.Create(context.Background(), CreateRequest{Create: true}); err == nil || !strings.Contains(err.Error(), "generate session ID") {
			t.Fatalf("entropy error = %v", err)
		}
		sessionRandomRead = previousRandom
	})

	t.Run("agent start scope", func(t *testing.T) {
		repository, policy := newMemoryRepository(), newMemoryPolicy()
		registry, _ := startRegistry(t, harness, repository, policy)
		newAgentScope = func() *plugin.Scope {
			scope := &plugin.Scope{}
			_ = scope.Close(context.Background())
			return scope
		}
		if _, err := registry.Create(context.Background(), CreateRequest{SessionID: "closed-scope", Create: true}); !errors.Is(err, plugin.ErrScopeClosed) {
			t.Fatalf("agent start error = %v", err)
		}
		newAgentScope = previousScope
	})

	t.Run("registry stopped during create", func(t *testing.T) {
		repository, policy := newMemoryRepository(), newMemoryPolicy()
		registry, scope := startRegistry(t, harness, repository, policy)
		policy.restoreHook = func() { _ = scope.Close(context.Background()) }
		if _, err := registry.Create(context.Background(), CreateRequest{SessionID: "stopped", Create: true}); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("stopped publication error = %v", err)
		}
	})

	t.Run("duplicate mounted during create", func(t *testing.T) {
		repository, policy := newMemoryRepository(), newMemoryPolicy()
		registry, _ := startRegistry(t, harness, repository, policy)
		policy.restoreHook = func() {
			registry.mu.Lock()
			registry.agents["raced"] = mountedAgent{}
			registry.mu.Unlock()
		}
		if _, err := registry.Create(context.Background(), CreateRequest{SessionID: "raced", Create: true}); !errors.Is(err, ErrAgentExists) {
			t.Fatalf("duplicate publication error = %v", err)
		}
		registry.mu.Lock()
		delete(registry.agents, "raced")
		registry.mu.Unlock()
	})
}

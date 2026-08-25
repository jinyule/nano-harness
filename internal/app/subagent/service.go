// Package subagent coordinates full in-process delegated agents.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidRequest identifies a delegation operation that violates service invariants.
	ErrInvalidRequest = errors.New("invalid subagent request")
	// ErrNotRunning indicates the subagent service has not started or has stopped.
	ErrNotRunning = errors.New("subagent service is not running")
	// ErrNotFound indicates no live child exists for the requested session ID.
	ErrNotFound   = errors.New("subagent not found")
	closeAgent    = func(registry *agent.Registry, ctx context.Context, id string) error { return registry.Close(ctx, id) }
	beforeSubmit  = func(*agent.Agent) {}
	beforePublish = func() {}
)

const maxDelegationDepth = 4

// SpawnRequest defines one delegated child and its initial task.
type SpawnRequest struct {
	ParentSessionID string
	Label           string
	Mode            string
	Persona         string
	Tools           []string
	Task            string
	Fork            bool
}

// Info is a secret-free child status and latest report.
type Info struct {
	SessionID string
	ParentID  string
	Label     string
	Mode      string
	Depth     int
	Busy      bool
	Pending   int
	Last      agent.TurnResult
}

type handle struct {
	agent *agent.Agent
	done  chan struct{}

	mu   sync.Mutex
	last agent.TurnResult
}

// Service owns delegated handles while the registry owns their worker scopes.
type Service struct {
	registry *agent.Registry

	mu      sync.RWMutex
	started bool
	active  bool
	ctx     context.Context
	cancel  context.CancelFunc
	handles map[string]*handle
	wait    sync.WaitGroup
}

// New constructs an inactive in-process delegation service.
func New(registry *agent.Registry) (*Service, error) {
	if registry == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{registry: registry, handles: map[string]*handle{}}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "subagents" }

// Start owns result monitors and every child created through this service.
func (service *Service) Start(ctx context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidRequest
	}
	service.ctx, service.cancel = context.WithCancel(ctx)
	if err := scope.Defer(service.stop); err != nil {
		service.cancel()
		return err
	}
	service.started, service.active = true, true
	return nil
}

func (service *Service) stop(ctx context.Context) error {
	service.mu.Lock()
	service.active = false
	service.cancel()
	ids := make([]string, 0, len(service.handles))
	for id := range service.handles {
		ids = append(ids, id)
	}
	service.mu.Unlock()
	service.wait.Wait()
	var failures []error
	for _, id := range ids {
		if err := closeAgent(service.registry, ctx, id); err != nil && !errors.Is(err, agent.ErrAgentNotFound) {
			failures = append(failures, err)
		}
	}
	service.mu.Lock()
	service.handles = map[string]*handle{}
	service.mu.Unlock()
	return errors.Join(failures...)
}

// Spawn creates, forks if requested, submits the initial task, and begins monitoring.
func (service *Service) Spawn(ctx context.Context, request SpawnRequest) (Info, error) {
	if request.ParentSessionID == "" || strings.TrimSpace(request.Label) == "" || len(request.Label) > 128 || strings.TrimSpace(request.Task) == "" || len(request.Task) > session.MaxTextBytes || request.Mode != "one-shot" && request.Mode != "continuable" || len(request.Persona) > 4096 || len(request.Tools) > 32 {
		return Info{}, ErrInvalidRequest
	}
	service.mu.RLock()
	active := service.active
	service.mu.RUnlock()
	if !active {
		return Info{}, ErrNotRunning
	}
	parent, err := service.registry.Find(request.ParentSessionID)
	if err != nil {
		return Info{}, err
	}
	parentStatus := parent.Status()
	if parentStatus.Depth >= maxDelegationDepth {
		return Info{}, fmt.Errorf("%w: maximum delegation depth %d reached", ErrInvalidRequest, maxDelegationDepth)
	}
	message, err := delegatedMessage(ctx, parent, request)
	if err != nil {
		return Info{}, err
	}
	child, err := service.registry.Create(ctx, agent.CreateRequest{
		ParentID: request.ParentSessionID, Label: request.Label, Mode: request.Mode,
		Persona: request.Persona, Tools: slices.Clone(request.Tools), Depth: parentStatus.Depth + 1, Create: true,
	})
	if err != nil {
		return Info{}, err
	}
	beforeSubmit(child)
	results, err := child.Submit(ctx, message)
	if err != nil {
		_ = closeAgent(service.registry, context.WithoutCancel(ctx), child.Status().SessionID)
		return Info{}, err
	}
	owned := &handle{agent: child, done: make(chan struct{})}
	id := child.Status().SessionID
	beforePublish()
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		_ = closeAgent(service.registry, context.WithoutCancel(ctx), id)
		return Info{}, ErrNotRunning
	}
	service.handles[id] = owned
	service.wait.Add(1)
	service.mu.Unlock()
	go service.monitor(owned, results)
	return service.info(owned), nil
}

func (service *Service) monitor(owned *handle, results <-chan agent.TurnResult) {
	defer service.wait.Done()
	defer close(owned.done)
	select {
	case result, ok := <-results:
		if ok {
			owned.mu.Lock()
			owned.last = result
			owned.mu.Unlock()
		}
	case <-service.ctx.Done():
	}
}

// Wait blocks for a child's initial report.
func (service *Service) Wait(ctx context.Context, sessionID string) (Info, error) {
	owned, err := service.handle(sessionID)
	if err != nil {
		return Info{}, err
	}
	select {
	case <-ctx.Done():
		return Info{}, ctx.Err()
	case <-owned.done:
		return service.info(owned), nil
	}
}

// Followup submits and waits for one continuable-child turn.
func (service *Service) Followup(ctx context.Context, callerID, sessionID, task string) (Info, error) {
	owned, err := service.handle(sessionID)
	if err != nil {
		return Info{}, err
	}
	if strings.TrimSpace(task) == "" || len(task) > session.MaxTextBytes || owned.agent.Status().ParentID != callerID {
		return Info{}, ErrInvalidRequest
	}
	message := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "delegation"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: task}}}
	results, err := owned.agent.Followup(ctx, message)
	if err != nil {
		return Info{}, err
	}
	select {
	case <-ctx.Done():
		return Info{}, ctx.Err()
	case result := <-results:
		owned.mu.Lock()
		owned.last = result
		owned.mu.Unlock()
		return service.info(owned), nil
	}
}

// Interrupt cancels a child's active turn.
func (service *Service) Interrupt(callerID, sessionID string) error {
	owned, err := service.handle(sessionID)
	if err != nil {
		return err
	}
	if owned.agent.Status().ParentID != callerID {
		return ErrInvalidRequest
	}
	owned.agent.Interrupt()
	return nil
}

// Report returns one current child snapshot.
func (service *Service) Report(sessionID string) (Info, error) {
	owned, err := service.handle(sessionID)
	if err != nil {
		return Info{}, err
	}
	return service.info(owned), nil
}

// List returns lexical children, optionally restricted to one parent.
func (service *Service) List(parentID string) ([]Info, error) {
	service.mu.RLock()
	if !service.active {
		service.mu.RUnlock()
		return nil, ErrNotRunning
	}
	handles := make([]*handle, 0, len(service.handles))
	for _, owned := range service.handles {
		if parentID == "" || owned.agent.Status().ParentID == parentID {
			handles = append(handles, owned)
		}
	}
	service.mu.RUnlock()
	infos := make([]Info, len(handles))
	for index, owned := range handles {
		infos[index] = service.info(owned)
	}
	slices.SortFunc(infos, func(left, right Info) int { return strings.Compare(left.SessionID, right.SessionID) })
	return infos, nil
}

func (service *Service) handle(sessionID string) (*handle, error) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	if !service.active {
		return nil, ErrNotRunning
	}
	owned := service.handles[sessionID]
	if owned == nil {
		return nil, ErrNotFound
	}
	return owned, nil
}

func (service *Service) info(owned *handle) Info {
	status := owned.agent.Status()
	owned.mu.Lock()
	last := owned.last
	owned.mu.Unlock()
	return Info{SessionID: status.SessionID, ParentID: status.ParentID, Label: status.Label, Mode: status.Mode, Depth: status.Depth, Busy: status.Busy, Pending: status.Pending, Last: last}
}

func delegatedMessage(ctx context.Context, parent *agent.Agent, request SpawnRequest) (session.Message, error) {
	text := "Assigned task:\n" + strings.TrimSpace(request.Task)
	if request.Fork {
		surface, err := parent.Surface(ctx)
		if err != nil {
			return session.Message{}, err
		}
		var contextText strings.Builder
		for _, node := range surface {
			switch {
			case node.Message != nil:
				fmt.Fprintf(&contextText, "[%s] %s\n", node.Message.Role, session.Text(*node.Message))
			case node.Call != nil:
				fmt.Fprintf(&contextText, "[tool call %s] %s\n", node.Call.Name, node.Call.Arguments)
			case node.Result != nil:
				fmt.Fprintf(&contextText, "[tool result %s] %s\n", node.Result.CallID, node.Result.Output)
			}
			if contextText.Len() > session.MaxTextBytes/2 {
				break
			}
		}
		text = "Forked parent context:\n" + contextText.String() + "\n" + text
	}
	if len(text) > session.MaxTextBytes {
		text = strings.ToValidUTF8(text[len(text)-session.MaxTextBytes:], "")
	}
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "delegation"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}, nil
}

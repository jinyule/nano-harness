package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	newAgentScope     = func() *plugin.Scope { return &plugin.Scope{} }
	sessionRandomRead = rand.Read
)

// PolicyService reconstructs and changes per-session approval policy.
type PolicyService interface {
	Restore(string, []session.Event) error
	SetPolicy(context.Context, string, appTool.Journal, session.ApprovalPolicy) error
}

type mountedAgent struct {
	agent *Agent
	scope *plugin.Scope
}

// Registry owns every live in-process agent and its nested scope.
type Registry struct {
	repository transcript.Repository
	engine     *Engine
	approval   PolicyService
	workspace  string

	mu      sync.RWMutex
	started bool
	active  bool
	ctx     context.Context
	agents  map[string]mountedAgent
}

// NewRegistry validates the live-agent composition.
func NewRegistry(repository transcript.Repository, engine *Engine, approval PolicyService, workspace string) (*Registry, error) {
	if repository == nil || engine == nil || approval == nil || strings.TrimSpace(workspace) == "" {
		return nil, ErrInvalidConfig
	}
	return &Registry{repository: repository, engine: engine, approval: approval, workspace: workspace, agents: map[string]mountedAgent{}}, nil
}

// SetPolicy durably changes one live root agent's approval policy.
func (registry *Registry) SetPolicy(ctx context.Context, sessionID string, policy session.ApprovalPolicy) error {
	current, err := registry.Find(sessionID)
	if err != nil {
		return err
	}
	if current.delegated {
		return ErrInvalidConfig
	}
	return registry.approval.SetPolicy(ctx, sessionID, current.journal, policy)
}

// ID returns the stable registry plugin identity.
func (*Registry) ID() string { return "agents" }

// Start activates dynamic agent scopes and owns their reverse cleanup.
func (registry *Registry) Start(ctx context.Context, scope *plugin.Scope) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.started {
		return ErrInvalidConfig
	}
	registry.ctx = ctx
	if err := scope.Defer(registry.stop); err != nil {
		return err
	}
	registry.started, registry.active = true, true
	return nil
}

func (registry *Registry) stop(ctx context.Context) error {
	registry.mu.Lock()
	registry.active = false
	mounted := make([]mountedAgent, 0, len(registry.agents))
	for _, candidate := range registry.agents {
		mounted = append(mounted, candidate)
	}
	registry.agents = map[string]mountedAgent{}
	registry.mu.Unlock()
	var failures []error
	for _, candidate := range mounted {
		candidate.agent.Interrupt()
		failures = append(failures, candidate.scope.Close(ctx))
	}
	return errors.Join(failures...)
}

// Create opens a new or existing transcript and starts one sequential worker.
func (registry *Registry) Create(ctx context.Context, request CreateRequest) (*Agent, error) {
	if request.SessionID == "" {
		generated, err := newSessionID()
		if err != nil {
			return nil, err
		}
		request.SessionID = generated
	}
	if request.Mode == "" {
		request.Mode = "continuable"
	}
	if request.Mode != "one-shot" && request.Mode != "continuable" || request.Depth < 0 || request.Depth > 16 || request.ParentID == "" && request.Depth != 0 || request.ParentID != "" && request.Depth == 0 {
		return nil, ErrInvalidConfig
	}
	registry.mu.Lock()
	if !registry.active {
		registry.mu.Unlock()
		return nil, ErrNotRunning
	}
	if _, exists := registry.agents[request.SessionID]; exists {
		registry.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrAgentExists, request.SessionID)
	}
	registry.mu.Unlock()
	log, err := registry.repository.OpenSession(ctx, transcript.OpenOptions{
		SessionID: request.SessionID, Create: request.Create, Cwd: registry.workspace,
		ParentSessionID: request.ParentID, DelegationDepth: request.Depth,
	})
	if err != nil {
		return nil, err
	}
	ownedJournal := newJournal(log)
	events, err := ownedJournal.Events(ctx)
	if err != nil {
		_ = ownedJournal.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	if request.Create {
		if request.ParentID != "" {
			descriptor := &session.SubagentDescriptor{Version: 1, Provider: "in-process", Mode: request.Mode, Label: request.Label, Persona: request.Persona, Tools: slices.Clone(request.Tools)}
			if _, err := ownedJournal.Append(ctx, session.Record{Type: session.RecordSubagentDescriptor, Subagent: descriptor}); err != nil {
				_ = ownedJournal.Close(context.WithoutCancel(ctx))
				return nil, err
			}
			if _, err := ownedJournal.Append(ctx, session.Record{Type: session.RecordApprovalPolicy, Approval: &session.ApprovalData{Policy: session.ApprovalNever, Source: "delegation"}}); err != nil {
				_ = ownedJournal.Close(context.WithoutCancel(ctx))
				return nil, err
			}
		} else if _, err := ownedJournal.Append(ctx, session.Record{Type: session.RecordApprovalPolicy, Approval: &session.ApprovalData{Policy: session.ApprovalAsk}}); err != nil {
			_ = ownedJournal.Close(context.WithoutCancel(ctx))
			return nil, err
		}
		events, err = ownedJournal.Events(ctx)
		if err != nil {
			_ = ownedJournal.Close(context.WithoutCancel(ctx))
			return nil, err
		}
	} else {
		request = restoreRequest(request, log.Header(), events)
	}
	if err := registry.approval.Restore(request.SessionID, events); err != nil {
		_ = ownedJournal.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	agent := &Agent{
		engine: registry.engine, journal: ownedJournal, parentID: request.ParentID,
		label: request.Label, mode: request.Mode, persona: request.Persona,
		tools: slices.Clone(request.Tools), depth: request.Depth, delegated: request.ParentID != "",
		turns: make(chan turnRequest, 32), steers: make(chan session.Message, 32), done: make(chan struct{}),
	}
	agentScope := newAgentScope()
	if err := agent.start(registry.ctx, agentScope); err != nil {
		scopeErr := agentScope.Close(context.WithoutCancel(ctx))
		journalErr := ownedJournal.Close(context.WithoutCancel(ctx))
		return nil, errors.Join(err, scopeErr, journalErr)
	}
	registry.mu.Lock()
	if !registry.active {
		registry.mu.Unlock()
		_ = agentScope.Close(context.WithoutCancel(ctx))
		return nil, ErrNotRunning
	}
	if _, exists := registry.agents[request.SessionID]; exists {
		registry.mu.Unlock()
		_ = agentScope.Close(context.WithoutCancel(ctx))
		return nil, ErrAgentExists
	}
	registry.agents[request.SessionID] = mountedAgent{agent: agent, scope: agentScope}
	registry.mu.Unlock()
	return agent, nil
}

func restoreRequest(request CreateRequest, header session.Header, events []session.Event) CreateRequest {
	request.ParentID, request.Depth = header.ParentSessionID, header.DelegationDepth
	for _, event := range events {
		if event.Record.Type == session.RecordSubagentDescriptor {
			request.Label = event.Record.Subagent.Label
			request.Mode = event.Record.Subagent.Mode
			request.Persona = event.Record.Subagent.Persona
			request.Tools = slices.Clone(event.Record.Subagent.Tools)
		}
	}
	return request
}

// Find returns one live agent by durable session ID.
func (registry *Registry) Find(sessionID string) (*Agent, error) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if !registry.active {
		return nil, ErrNotRunning
	}
	mounted, exists := registry.agents[sessionID]
	if !exists {
		return nil, ErrAgentNotFound
	}
	return mounted.agent, nil
}

// Statuses returns lexical live snapshots.
func (registry *Registry) Statuses() ([]Status, error) {
	registry.mu.RLock()
	if !registry.active {
		registry.mu.RUnlock()
		return nil, ErrNotRunning
	}
	agents := make([]*Agent, 0, len(registry.agents))
	for _, mounted := range registry.agents {
		agents = append(agents, mounted.agent)
	}
	registry.mu.RUnlock()
	statuses := make([]Status, len(agents))
	for index, agent := range agents {
		statuses[index] = agent.Status()
	}
	slices.SortFunc(statuses, func(left, right Status) int { return strings.Compare(left.SessionID, right.SessionID) })
	return statuses, nil
}

// Close stops one live agent and releases its transcript lock.
func (registry *Registry) Close(ctx context.Context, sessionID string) error {
	registry.mu.Lock()
	mounted, exists := registry.agents[sessionID]
	if exists {
		delete(registry.agents, sessionID)
	}
	registry.mu.Unlock()
	if !exists {
		return ErrAgentNotFound
	}
	return mounted.agent.close(ctx, mounted.scope)
}

func newSessionID() (string, error) {
	var random [12]byte
	if _, err := sessionRandomRead(random[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return "session-" + hex.EncodeToString(random[:]), nil
}

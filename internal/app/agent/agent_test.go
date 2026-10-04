package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type observedDoneContext struct {
	context.Context
	observed chan struct{}
}

func (ctx observedDoneContext) Done() <-chan struct{} {
	select {
	case <-ctx.observed:
	default:
		close(ctx.observed)
	}
	return ctx.Context.Done()
}

func TestAgent_SubmitFollowupSubscribeAndSnapshots(t *testing.T) {
	harness := startEngineHarness(t, 2,
		modelAction{completion: assistantCompletion("first")},
		modelAction{completion: assistantCompletion("followup")},
		modelAction{completion: assistantCompletion("summary")},
		modelAction{completion: assistantCompletion("one shot")},
	)
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	updates, dispose, err := root.Subscribe(64)
	if err != nil {
		t.Fatal(err)
	}
	defer dispose()
	if _, err := root.Submit(context.Background(), session.Message{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid Submit() error = %v", err)
	}
	resultChannel, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	result := <-resultChannel
	if result.Outcome != session.OutcomeCompleted || result.Text != "first" {
		t.Fatalf("first result = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	followup, err := root.Followup(context.Background(), agentMessage(session.RoleUser, "again"))
	if err != nil {
		t.Fatal(err)
	}
	if result := <-followup; result.Turn != 2 || result.Text != "followup" {
		t.Fatalf("followup result = %+v", result)
	}
	select {
	case event := <-updates:
		if event.Record.Type == "" {
			t.Fatal("empty subscribed event")
		}
	default:
		t.Fatal("no subscribed event")
	}
	status := root.Status()
	if status.SessionID != "root" || status.Busy || status.Pending != 0 || status.Last.Turn != 2 {
		t.Fatalf("status = %+v", status)
	}
	events, err := root.Events(context.Background())
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %#v, %v", events, err)
	}
	surface, err := root.Surface(context.Background())
	if err != nil || len(surface) != 4 {
		t.Fatalf("surface = %#v, %v", surface, err)
	}
	compacted, err := root.Compact(context.Background())
	if err != nil || !compacted {
		t.Fatalf("manual compaction = %v, %v", compacted, err)
	}

	oneShot, err := registry.Create(context.Background(), CreateRequest{SessionID: "one", ParentID: "root", Depth: 1, Mode: "one-shot", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oneShot.Followup(context.Background(), agentMessage(session.RoleUser, "not allowed")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("one-shot Followup() error = %v", err)
	}
	completed, err := oneShot.Submit(context.Background(), agentMessage(session.RoleUser, "once"))
	if err != nil {
		t.Fatal(err)
	}
	if result := <-completed; result.Outcome != session.OutcomeCompleted {
		t.Fatalf("one-shot result = %+v", result)
	}
	if _, err := oneShot.Submit(context.Background(), agentMessage(session.RoleUser, "twice")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("second one-shot Submit() error = %v", err)
	}
}

func TestAgent_SteersAtToolBoundaryAndWaitsForIdle(t *testing.T) {
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	call := session.ToolCall{ID: "call", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 2,
		modelAction{started: started, wait: gate, completion: assistantCompletion("tool", call)},
		modelAction{completion: assistantCompletion("done")},
	)
	candidate := &engineTool{name: "inspect", output: "ok"}
	toolScope := &plugin.Scope{}
	if err := harness.tools.Register(candidate, toolScope); err != nil {
		t.Fatal(err)
	}
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)
	agent, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Tools: []string{"inspect"}, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	resultChannel, err := agent.Submit(context.Background(), agentMessage(session.RoleUser, "start"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if status := agent.Status(); !status.Busy || status.Pending != 1 {
		t.Fatalf("busy status = %+v", status)
	}
	if err := agent.Steer(context.Background(), agentMessage(session.RoleUser, "steer")); err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := agent.WhenIdle(waitContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy WhenIdle() error = %v", err)
	}
	idleSelecting := make(chan struct{})
	idleResult := make(chan error, 1)
	go func() {
		idleResult <- agent.WhenIdle(observedDoneContext{
			Context:  context.Background(),
			observed: idleSelecting,
		})
	}()
	<-idleSelecting
	close(gate)
	if err := <-idleResult; err != nil {
		t.Fatal(err)
	}
	if result := <-resultChannel; result.Outcome != session.OutcomeCompleted || result.Text != "done" {
		t.Fatalf("steered result = %+v", result)
	}
	events, _ := agent.Events(context.Background())
	found := false
	for _, event := range events {
		if event.Record.Type == session.RecordUserMessage && session.Text(*event.Record.Message) == "steer" {
			found = true
		}
	}
	if !found {
		t.Fatal("steer was not durably injected")
	}
	if err := agent.Steer(context.Background(), agentMessage(session.RoleUser, "idle")); !errors.Is(err, ErrAgentIdle) {
		t.Fatalf("idle Steer() error = %v", err)
	}
}

func TestAgent_InterruptCancelsOnlyActiveTurn(t *testing.T) {
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	harness := startEngineHarness(t, 1, modelAction{started: started, wait: gate})
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)
	agent, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	resultChannel, err := agent.Submit(context.Background(), agentMessage(session.RoleUser, "block"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	agent.Interrupt()
	result := <-resultChannel
	if result.Outcome != session.OutcomeCanceled || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("interrupted result = %+v", result)
	}
	agent.Interrupt()
}

func TestAgent_ControlMethodsContainInactiveCanceledAndClosedStates(t *testing.T) {
	harness := startEngineHarness(t, 1)
	journal, log := turnJournal()
	inactive := &Agent{engine: harness.engine, journal: journal, mode: "continuable", turns: make(chan turnRequest), steers: make(chan session.Message), done: make(chan struct{})}
	message := agentMessage(session.RoleUser, "message")
	if _, err := inactive.Submit(context.Background(), message); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive Submit() error = %v", err)
	}
	if err := inactive.WhenIdle(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive WhenIdle() error = %v", err)
	}
	if _, _, err := inactive.Subscribe(1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive Subscribe() error = %v", err)
	}
	if _, err := inactive.Compact(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive Compact() error = %v", err)
	}

	canceled := &Agent{engine: harness.engine, journal: journal, active: true, mode: "continuable", turns: make(chan turnRequest), steers: make(chan session.Message), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := canceled.Submit(ctx, message); !errors.Is(err, context.Canceled) || canceled.pending != 0 {
		t.Fatalf("canceled Submit() error = %v pending=%d", err, canceled.pending)
	}
	canceled.busy = true
	if err := canceled.Steer(ctx, message); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Steer() error = %v", err)
	}
	canceled.busy = false
	canceled.pending = 1
	if err := canceled.WhenIdle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled WhenIdle() error = %v", err)
	}
	canceled.pending = 0
	if err := canceled.WhenIdle(context.Background()); err != nil {
		t.Fatalf("idle WhenIdle() error = %v", err)
	}

	closed := &Agent{engine: harness.engine, journal: journal, active: true, busy: true, mode: "continuable", turns: make(chan turnRequest), steers: make(chan session.Message), done: make(chan struct{})}
	close(closed.done)
	if _, err := closed.Submit(context.Background(), message); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("closed Submit() error = %v", err)
	}
	if err := closed.Steer(context.Background(), message); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("closed Steer() error = %v", err)
	}
	if err := closed.Steer(context.Background(), session.Message{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid Steer() error = %v", err)
	}

	closed.steers = make(chan session.Message, 2)
	closed.steers <- agentMessage(session.RoleUser, "one")
	closed.steers <- agentMessage(session.RoleUser, "two")
	if drained := closed.drainSteers(); len(drained) != 2 || closed.drainSteers() == nil {
		t.Fatalf("drained = %#v", drained)
	}

	log.eventsErr = errors.New("events")
	if _, err := closed.Events(context.Background()); !errors.Is(err, log.eventsErr) {
		t.Fatalf("Events() error = %v", err)
	}
	if _, err := closed.Surface(context.Background()); !errors.Is(err, log.eventsErr) {
		t.Fatalf("Surface() error = %v", err)
	}
	log.eventsErr = nil
	closed.busy, closed.pending = false, 1
	if _, err := closed.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "must be idle") {
		t.Fatalf("busy Compact() error = %v", err)
	}
}

func TestAgent_StartShutdownDrainsQueuedTurnsAndClosesJournal(t *testing.T) {
	harness := startEngineHarness(t, 1)
	journal, log := turnJournal()
	agent := &Agent{
		engine: harness.engine, journal: journal, mode: "continuable",
		turns: make(chan turnRequest, 4), steers: make(chan session.Message, 1), done: make(chan struct{}),
	}
	closedScope := &plugin.Scope{}
	_ = closedScope.Close(context.Background())
	if err := agent.start(context.Background(), closedScope); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed start error = %v", err)
	}

	previousDrain := beforeAgentDrain
	reachedDrain, releaseDrain := make(chan struct{}), make(chan struct{})
	beforeAgentDrain = func() {
		close(reachedDrain)
		<-releaseDrain
	}
	t.Cleanup(func() { beforeAgentDrain = previousDrain })
	ctx, cancel := context.WithCancel(context.Background())
	scope := &plugin.Scope{}
	if err := agent.start(ctx, scope); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-reachedDrain
	queued := make([]chan TurnResult, 3)
	for index := range queued {
		queued[index] = make(chan TurnResult, 1)
		agent.turns <- turnRequest{message: agentMessage(session.RoleUser, "queued"), result: queued[index]}
	}
	close(releaseDrain)
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, resultChannel := range queued {
		result := <-resultChannel
		if result.Outcome != session.OutcomeCanceled || result.Err == nil {
			t.Fatalf("drained result = %+v", result)
		}
	}
	if !log.closed || agent.Status().Pending != 0 {
		t.Fatalf("shutdown state = closed=%v status=%+v", log.closed, agent.Status())
	}
}

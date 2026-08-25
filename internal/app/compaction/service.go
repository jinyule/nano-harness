// Package compaction replaces old model-visible nodes while preserving the raw log.
package compaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidRequest identifies an invalid compaction dependency or operation.
	ErrInvalidRequest = errors.New("invalid compaction request")
	// ErrNotRunning indicates the compaction service has not started or has stopped.
	ErrNotRunning  = errors.New("compaction service is not running")
	compactionWait = wait
)

// Journal supplies a durable snapshot and append boundary.
type Journal interface {
	Events(context.Context) ([]session.Event, error)
	Append(context.Context, session.Record) (session.Event, error)
}

// Request selects proactive or forced compaction.
type Request struct {
	Journal Journal
	Turn    uint64
	Force   bool
}

// Service owns compaction model calls and hot policy reads.
type Service struct {
	llm      *llm.Runtime
	settings *settings.Service

	mu      sync.RWMutex
	started bool
	active  bool
	nextID  atomic.Uint64
}

// New constructs an inactive service.
func New(runtime *llm.Runtime, configuration *settings.Service) (*Service, error) {
	if runtime == nil || configuration == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{llm: runtime, settings: configuration}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "compaction" }

// Start activates compaction until scope cleanup.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidRequest
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		service.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active = true, true
	return nil
}

// Maybe summarizes the oldest visible prefix when pressure crosses the hot threshold.
func (service *Service) Maybe(ctx context.Context, request Request) (bool, error) {
	if request.Journal == nil {
		return false, ErrInvalidRequest
	}
	service.mu.RLock()
	active := service.active
	service.mu.RUnlock()
	if !active {
		return false, ErrNotRunning
	}
	document, _, err := service.settings.Snapshot()
	if err != nil {
		return false, err
	}
	configured := document.Providers[document.Route.Provider]
	var model settings.Model
	for _, candidate := range configured.Models {
		if candidate.ID == document.Route.Model {
			model = candidate
			break
		}
	}
	events, err := request.Journal.Events(ctx)
	if err != nil {
		return false, err
	}
	surface, err := session.Surface(events)
	if err != nil {
		return false, err
	}
	total := estimateSurface(surface)
	threshold := int(float64(model.ContextWindow) * document.Compaction.ThresholdRatio)
	if !request.Force && total < threshold {
		return false, nil
	}
	shadowed, count := selectPrefix(surface, int(float64(model.ContextWindow)*document.Compaction.RetainRatio), request.Force)
	if len(shadowed) == 0 {
		return false, nil
	}
	id := fmt.Sprintf("compact-%d", service.nextID.Add(1))
	if _, err := request.Journal.Append(ctx, session.Record{Type: session.RecordCompactionStart, Turn: request.Turn, Compaction: &session.CompactionData{ID: id}}); err != nil {
		return false, err
	}
	call, err := service.llm.PrepareCall(ctx, document.Route.Provider, document.Route.Model)
	if err != nil {
		return false, service.finishError(ctx, request, id, err)
	}
	var completion llm.Completion
	for attempt := 0; ; attempt++ {
		completion, err = call.Stream(ctx, llm.Request{
			Purpose: "compaction", System: compactionPrompt,
			Surface: shadowed, MaxTokens: document.Compaction.MaxTokens,
		}, func(session.AssistantChunk) error { return nil })
		if err == nil || attempt >= document.Compaction.Retries {
			break
		}
		if waitErr := compactionWait(ctx, time.Duration(attempt+1)*250*time.Millisecond); waitErr != nil {
			err = waitErr
			break
		}
	}
	if err != nil {
		return false, service.finishError(ctx, request, id, err)
	}
	text := session.Text(completion.Message)
	if text == "" || len(completion.Calls) != 0 {
		return false, service.finishError(ctx, request, id, errors.New("summary response was empty or attempted a tool"))
	}
	sequences := make([]uint64, len(shadowed))
	for index, node := range shadowed {
		sequences[index] = node.Sequence
	}
	data := &session.CompactionData{
		ID: id, ShadowedSeqs: sequences, ShadowedTokenCount: count,
		Summary:  []session.ContentBlock{{Type: session.ContentText, Text: text}},
		Provider: document.Route.Provider, Model: document.Route.Model,
	}
	if _, err := request.Journal.Append(ctx, session.Record{Type: session.RecordCompactionSummary, Turn: request.Turn, Compaction: data}); err != nil {
		return false, service.finishError(ctx, request, id, err)
	}
	if _, err := request.Journal.Append(ctx, session.Record{Type: session.RecordCompactionEnd, Turn: request.Turn, Compaction: &session.CompactionData{ID: id}}); err != nil {
		return false, err
	}
	return true, nil
}

func (service *Service) finishError(ctx context.Context, request Request, id string, cause error) error {
	message := safeFailure(cause)
	_, appendErr := request.Journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordCompactionEnd, Turn: request.Turn, Compaction: &session.CompactionData{ID: id, Error: message}})
	return errors.Join(cause, appendErr)
}

func safeFailure(err error) string {
	var failure *llm.Error
	if errors.As(err, &failure) {
		return string(failure.Code)
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "compaction_failed"
}

func estimateSurface(surface []session.SurfaceNode) int {
	total := 0
	for _, node := range surface {
		switch {
		case node.Message != nil:
			for _, block := range node.Message.Content {
				if block.Type == session.ContentImage {
					total += 1024
				} else {
					total += max(1, len(block.Text)/4)
				}
			}
		case node.Call != nil:
			total += max(1, (len(node.Call.Name)+len(node.Call.Arguments))/4)
		case node.Result != nil:
			total += max(1, len(node.Result.Output)/4)
		}
	}
	return total
}

func selectPrefix(surface []session.SurfaceNode, retainTokens int, force bool) ([]session.SurfaceNode, int) {
	if len(surface) < 3 {
		return nil, 0
	}
	retained := 0
	boundary := len(surface)
	for boundary > 0 && retained < retainTokens {
		boundary--
		retained += estimateSurface(surface[boundary : boundary+1])
	}
	if force && boundary == 0 {
		boundary = len(surface) / 2
	}
	if boundary > 0 && boundary < len(surface) && surface[boundary].Result != nil && surface[boundary-1].Call != nil && surface[boundary-1].Call.ID == surface[boundary].Result.CallID {
		boundary--
	}
	if boundary == 0 {
		return nil, 0
	}
	shadowed := append([]session.SurfaceNode(nil), surface[:boundary]...)
	return shadowed, estimateSurface(shadowed)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

const compactionPrompt = "Summarize the supplied conversation prefix for a coding agent. Preserve user intent, decisions, file paths, tool facts, unresolved work, failures, and safety constraints. Do not add facts and do not call tools. Return only the compact replacement context."

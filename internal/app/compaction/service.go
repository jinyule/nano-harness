// Package compaction reduces the model surface while preserving the raw log.
// Under pressure it first prunes oversized tool results without a model
// call, as upstream's tool-result pruner does, and summarizes the oldest
// prefix only when the pruned surface is still above the threshold.
package compaction

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	ErrNotRunning       = errors.New("compaction service is not running")
	errSummaryTruncated = errors.New("summary truncated at the output token cap (incomplete checkpoint)")
	compactionWait      = wait
)

// Journal supplies a durable snapshot and append boundary.
type Journal interface {
	Events(context.Context) ([]session.Event, error)
	Append(context.Context, session.Record) (session.Event, error)
}

// Request selects proactive or forced compaction. Force skips the pressure
// check and always summarizes, for context-window recovery and manual
// requests; Manual also skips the model-free pruning pass, as upstream's
// manual compaction does.
type Request struct {
	Journal Journal
	Turn    uint64
	Force   bool
	Manual  bool
	// Route is a delegated agent's inherited route: its model's context
	// window sets the thresholds and it carries the summary request,
	// including its effort. The zero value uses the hot settings route and
	// that model's catalog effort.
	Route session.SubagentRoute
}

// Service owns compaction model calls and hot policy reads.
type Service struct {
	llm      *llm.Runtime
	settings *settings.Service

	mu      sync.Mutex
	started bool
	active  bool
	nextID  atomic.Uint64
	// calls cancels each Maybe in flight; group joins them.
	nextCall uint64
	calls    map[uint64]context.CancelFunc
	group    sync.WaitGroup
}

// New constructs an inactive service.
func New(runtime *llm.Runtime, configuration *settings.Service) (*Service, error) {
	if runtime == nil || configuration == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{llm: runtime, settings: configuration, calls: map[uint64]context.CancelFunc{}}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "compaction" }

// Start activates compaction until scope cleanup. Cleanup rejects new
// requests, cancels each one in flight, including its summary request, and
// returns only after each has returned; an open compaction transaction is
// closed first, so nothing is appended once cleanup returns.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidRequest
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		for _, cancel := range service.calls {
			cancel()
		}
		service.mu.Unlock()
		// The wait is bounded by the provider's and journal's cancellation
		// latency plus the uncancellable append that closes a transaction.
		service.group.Wait()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active = true, true
	return nil
}

// Maybe reduces the surface when pressure crosses the hot threshold or the
// request is forced. It first records a compaction/prune for every visible
// tool result over the pruning budget; a pressure request that the pruned
// surface relieves stops there, and otherwise the oldest prefix is
// summarized. It reports whether the surface changed. Prunes recorded before
// a failure stay in the log. Once compaction/start is committed the
// transaction is always closed: a failure, including cancellation, records
// an error compaction/end, and a committed summary records its end even if
// the context is cancelled meanwhile.
func (service *Service) Maybe(ctx context.Context, request Request) (bool, error) {
	if request.Journal == nil {
		return false, ErrInvalidRequest
	}
	ctx, done, err := service.begin(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	document, _, err := service.settings.Snapshot()
	if err != nil {
		return false, err
	}
	route := request.Route
	model := findModel(document, document.Route.Provider, document.Route.Model)
	if route == (session.SubagentRoute{}) {
		route = session.SubagentRoute{Provider: document.Route.Provider, Model: document.Route.Model, Effort: model.Effort}
	} else {
		model = findModel(document, route.Provider, route.Model)
	}
	events, err := request.Journal.Events(ctx)
	if err != nil {
		return false, err
	}
	surface, err := session.Surface(events)
	if err != nil {
		return false, err
	}
	threshold := int(float64(model.ContextWindow) * document.Compaction.ThresholdRatio)
	if !request.Force && (model.ContextWindow == 0 || estimateSurface(surface) < threshold) {
		return false, nil
	}
	pruned := false
	if !request.Manual {
		if pruned, err = prune(ctx, request, surface); err != nil {
			return pruned, err
		}
		if pruned && !request.Force && estimateSurface(surface) < threshold {
			return true, nil
		}
	}
	shadowed, count := selectPrefix(surface, int(float64(model.ContextWindow)*document.Compaction.RetainRatio), request.Force)
	if len(shadowed) == 0 {
		return pruned, nil
	}
	id := fmt.Sprintf("compact-%d", service.nextID.Add(1))
	if _, err := request.Journal.Append(ctx, session.Record{Type: session.RecordCompactionStart, Turn: request.Turn, Compaction: &session.CompactionData{ID: id}}); err != nil {
		return false, err
	}
	call, err := service.llm.PrepareCall(ctx, route.Provider, route.Model)
	if err != nil {
		return false, service.finishError(ctx, request, id, err)
	}
	modelInfo := call.Info()
	var completion llm.Completion
	for attempt := 0; ; attempt++ {
		completion, err = call.Stream(ctx, llm.Request{
			Purpose: "compaction", System: compactionPrompt,
			Surface: shadowed, MaxTokens: document.Compaction.MaxTokens, Effort: &route.Effort,
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
	if completion.Stop == llm.StopMaxTokens {
		return pruned, service.finishError(ctx, request, id, fmt.Errorf("compaction %s: %w", id, errSummaryTruncated))
	}
	text := session.Text(completion.Message)
	if text == "" || len(completion.Calls) != 0 {
		return false, service.finishError(ctx, request, id, errors.New("summary response was empty or attempted a tool"))
	}
	sequences := make([]uint64, len(shadowed))
	for index, node := range shadowed {
		sequences[index] = node.Sequence
	}
	// An earlier summary sits in front of the older messages it retained but
	// carries a later sequence; the record lists sequences in order.
	slices.Sort(sequences)
	data := &session.CompactionData{
		ID: id, ShadowedSeqs: sequences, ShadowedTokenCount: count,
		Summary:  []session.ContentBlock{{Type: session.ContentText, Text: text}},
		Provider: modelInfo.Provider, Model: modelInfo.ID, Effort: route.Effort,
	}
	if _, err := request.Journal.Append(ctx, session.Record{Type: session.RecordCompactionSummary, Turn: request.Turn, Compaction: data}); err != nil {
		return false, service.finishError(ctx, request, id, err)
	}
	if _, err := request.Journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordCompactionEnd, Turn: request.Turn, Compaction: &session.CompactionData{ID: id}}); err != nil {
		return false, err
	}
	return true, nil
}

// findModel returns the settings catalog entry of provider/model, or the
// zero model, whose window disables pressure-triggered compaction, when the
// catalog no longer lists it.
func findModel(document settings.Document, provider, model string) settings.Model {
	for _, candidate := range document.Providers[provider].Models {
		if candidate.ID == model {
			return candidate
		}
	}
	return settings.Model{}
}

// begin admits one request while the service runs and returns its context,
// which cleanup cancels; done must run once the request returns.
func (service *Service) begin(ctx context.Context) (context.Context, func(), error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return nil, nil, ErrNotRunning
	}
	call, cancel := context.WithCancel(ctx)
	service.nextCall++
	key := service.nextCall
	service.calls[key] = cancel
	service.group.Add(1)
	return call, func() {
		cancel()
		service.mu.Lock()
		delete(service.calls, key)
		service.mu.Unlock()
		service.group.Done()
	}, nil
}

// prune records the bounded replacement of every visible tool result over
// the pruning budget and applies it to surface in place, so the caller can
// remeasure the surface the log now folds to.
func prune(ctx context.Context, request Request, surface []session.SurfaceNode) (bool, error) {
	pruned := false
	for _, node := range surface {
		if node.Result == nil {
			continue
		}
		output, ok := session.PruneToolOutput(node.Result.Output)
		if !ok {
			continue
		}
		record := session.Record{Type: session.RecordCompactionPrune, Turn: request.Turn, Prune: &session.ToolResultPrune{Seq: node.Sequence, Output: output}}
		if _, err := request.Journal.Append(ctx, record); err != nil {
			return pruned, fmt.Errorf("prune tool result %d: %w", node.Sequence, err)
		}
		node.Result.Output = output
		pruned = true
	}
	return pruned, nil
}

func (service *Service) finishError(ctx context.Context, request Request, id string, cause error) error {
	message := safeFailure(cause)
	_, appendErr := request.Journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordCompactionEnd, Turn: request.Turn, Compaction: &session.CompactionData{ID: id, Error: message}})
	return errors.Join(cause, appendErr)
}

func safeFailure(err error) string {
	if errors.Is(err, errSummaryTruncated) {
		return llm.StopMaxTokens
	}
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
			if node.Result.Image != nil {
				total += 1024
			}
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

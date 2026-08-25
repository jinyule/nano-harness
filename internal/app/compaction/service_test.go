package compaction

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type compactionStore struct{ credential llm.Credential }

func (store *compactionStore) Resolve(context.Context, string, string) (llm.Credential, error) {
	return store.credential, nil
}
func (store *compactionStore) Modify(_ context.Context, _ string, mutate func(*llm.Credential) (*llm.Credential, error)) (llm.Credential, error) {
	next, err := mutate(&store.credential)
	if next != nil {
		store.credential = *next
	}
	return store.credential, err
}
func (*compactionStore) Delete(context.Context, string) error            { return nil }
func (*compactionStore) List(context.Context) ([]llm.AccountInfo, error) { return nil, nil }

type compactionPrepared struct {
	results []llm.Completion
	errors  []error
	calls   int
}

func (*compactionPrepared) Info() llm.ModelInfo {
	return llm.ModelInfo{Provider: "openai", ID: "gpt-5.6-luna", ContextWindow: 200_000}
}
func (*compactionPrepared) CredentialEnv() string { return "OPENAI_API_KEY" }
func (*compactionPrepared) Refresh(_ context.Context, credential llm.Credential) (llm.Credential, error) {
	return credential, nil
}
func (prepared *compactionPrepared) Stream(_ context.Context, _ llm.Credential, request llm.Request, emit llm.Emit) (llm.Completion, error) {
	if request.Purpose != "compaction" || request.MaxTokens == 0 || emit == nil {
		return llm.Completion{}, errors.New("bad compaction request")
	}
	index := prepared.calls
	prepared.calls++
	_ = emit(session.AssistantChunk{Kind: session.ChunkText, Text: "summary"})
	if index < len(prepared.errors) && prepared.errors[index] != nil {
		return llm.Completion{}, prepared.errors[index]
	}
	if index < len(prepared.results) {
		return prepared.results[index], nil
	}
	return llm.Completion{Message: assistantSummary("summary")}, nil
}

type compactionProvider struct {
	prepared *compactionPrepared
	err      error
}

func (*compactionProvider) ID() string { return "openai" }
func (*compactionProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{Provider: "openai", ID: "gpt-5.6-luna"}}
}
func (provider *compactionProvider) Prepare(string) (llm.PreparedModel, error) {
	return provider.prepared, provider.err
}
func (*compactionProvider) AuthMethods() []llm.AuthMethod { return nil }
func (*compactionProvider) Login(context.Context, string, llm.AuthInteraction) (llm.Credential, error) {
	return llm.Credential{}, nil
}

type compactionJournal struct {
	events    []session.Event
	eventsErr error
	records   []session.Record
	failures  map[int]error
}

func (journal *compactionJournal) Events(context.Context) ([]session.Event, error) {
	return journal.events, journal.eventsErr
}
func (journal *compactionJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	position := len(journal.records) + 1
	journal.records = append(journal.records, record)
	if err := journal.failures[position]; err != nil {
		return session.Event{}, err
	}
	return session.Event{Sequence: uint64(position), Record: record}, nil
}

func assistantSummary(text string) session.Message {
	content := []session.ContentBlock(nil)
	if text != "" {
		content = []session.ContentBlock{{Type: session.ContentText, Text: text}}
	}
	return session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}, Content: content}
}

func visibleEvents(count int) []session.Event {
	events := make([]session.Event, count)
	for index := range events {
		events[index] = session.Event{Sequence: uint64(index + 1), Record: session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{
			Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "message"}},
		}}}
	}
	return events
}

type compactionHarness struct {
	service       *Service
	prepared      *compactionPrepared
	provider      *compactionProvider
	serviceScope  *plugin.Scope
	providerScope *plugin.Scope
	llmScope      *plugin.Scope
	settingsScope *plugin.Scope
}

func newCompactionHarness(t *testing.T) *compactionHarness {
	t.Helper()
	configuration := settings.New()
	settingsScope := &plugin.Scope{}
	if err := configuration.Start(context.Background(), settingsScope); err != nil {
		t.Fatal(err)
	}
	runtime, _ := llm.New(&compactionStore{credential: llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}})
	llmScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), llmScope); err != nil {
		t.Fatal(err)
	}
	prepared := &compactionPrepared{}
	provider := &compactionProvider{prepared: prepared}
	providerScope := &plugin.Scope{}
	if err := runtime.Register(provider, providerScope); err != nil {
		t.Fatal(err)
	}
	service, _ := New(runtime, configuration)
	serviceScope := &plugin.Scope{}
	if err := service.Start(context.Background(), serviceScope); err != nil {
		t.Fatal(err)
	}
	harness := &compactionHarness{service: service, prepared: prepared, provider: provider, serviceScope: serviceScope, providerScope: providerScope, llmScope: llmScope, settingsScope: settingsScope}
	t.Cleanup(func() {
		_ = serviceScope.Close(context.Background())
		_ = providerScope.Close(context.Background())
		_ = llmScope.Close(context.Background())
		_ = settingsScope.Close(context.Background())
	})
	return harness
}

func TestLifecyclePressureAndSuccessfulCompaction(t *testing.T) {
	if _, err := New(nil, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil construction=%v", err)
	}
	harness := newCompactionHarness(t)
	if harness.service.ID() != "compaction" {
		t.Fatal("wrong service ID")
	}
	if err := harness.service.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("second start=%v", err)
	}
	if _, err := harness.service.Maybe(context.Background(), Request{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil journal=%v", err)
	}
	journal := &compactionJournal{events: visibleEvents(4), failures: map[int]error{}}
	compacted, err := harness.service.Maybe(context.Background(), Request{Journal: journal})
	if err != nil || compacted {
		t.Fatalf("below threshold compacted=%t err=%v", compacted, err)
	}
	compacted, err = harness.service.Maybe(context.Background(), Request{Journal: journal, Turn: 1, Force: true})
	if err != nil || !compacted || len(journal.records) != 3 || journal.records[1].Type != session.RecordCompactionSummary || journal.records[2].Type != session.RecordCompactionEnd {
		t.Fatalf("compacted=%t records=%#v err=%v", compacted, journal.records, err)
	}
	if err := harness.serviceScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.Maybe(context.Background(), Request{Journal: journal}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive maybe=%v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	service, _ := New(&llm.Runtime{}, settings.New())
	if err := service.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
}

func TestMaybeFailurePathsAndRetry(t *testing.T) {
	originalWait := compactionWait
	t.Cleanup(func() { compactionWait = originalWait })
	compactionWait = func(context.Context, time.Duration) error { return nil }

	t.Run("settings", func(t *testing.T) {
		harness := newCompactionHarness(t)
		_ = harness.settingsScope.Close(context.Background())
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{failures: map[int]error{}}, Force: true})
		if !errors.Is(err, settings.ErrNotRunning) {
			t.Fatalf("settings error=%v", err)
		}
	})
	t.Run("events", func(t *testing.T) {
		harness := newCompactionHarness(t)
		failure := errors.New("events")
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{eventsErr: failure, failures: map[int]error{}}, Force: true})
		if !errors.Is(err, failure) {
			t.Fatalf("events error=%v", err)
		}
	})
	t.Run("surface", func(t *testing.T) {
		harness := newCompactionHarness(t)
		bad := []session.Event{{Sequence: 1, Record: session.Record{Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "c", ShadowedSeqs: []uint64{99}, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "s"}}}}}}
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: bad, failures: map[int]error{}}, Force: true})
		if err == nil {
			t.Fatal("surface error missing")
		}
	})
	t.Run("no prefix", func(t *testing.T) {
		harness := newCompactionHarness(t)
		compacted, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(2), failures: map[int]error{}}, Force: true})
		if err != nil || compacted {
			t.Fatalf("short surface compacted=%t err=%v", compacted, err)
		}
	})
	t.Run("start append", func(t *testing.T) {
		harness := newCompactionHarness(t)
		failure := errors.New("append start")
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{1: failure}}, Force: true})
		if !errors.Is(err, failure) {
			t.Fatalf("start append=%v", err)
		}
	})
	t.Run("prepare and finish append", func(t *testing.T) {
		harness := newCompactionHarness(t)
		harness.provider.err = errors.New("prepare")
		finish := errors.New("finish")
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{2: finish}}, Force: true})
		if !errors.Is(err, harness.provider.err) || !errors.Is(err, finish) {
			t.Fatalf("prepare/finish errors=%v", err)
		}
	})
	t.Run("retry succeeds", func(t *testing.T) {
		harness := newCompactionHarness(t)
		harness.prepared.errors = []error{&llm.Error{Code: llm.ErrorServer, Provider: "openai"}}
		harness.prepared.results = []llm.Completion{{}, {Message: assistantSummary("summary")}}
		compacted, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{}}, Force: true})
		if err != nil || !compacted || harness.prepared.calls != 2 {
			t.Fatalf("retry compacted=%t calls=%d err=%v", compacted, harness.prepared.calls, err)
		}
	})
	t.Run("retry wait", func(t *testing.T) {
		harness := newCompactionHarness(t)
		harness.prepared.errors = []error{errors.New("stream")}
		compactionWait = func(context.Context, time.Duration) error { return context.Canceled }
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{}}, Force: true})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error=%v", err)
		}
		compactionWait = func(context.Context, time.Duration) error { return nil }
	})
	t.Run("stream fails", func(t *testing.T) {
		harness := newCompactionHarness(t)
		harness.prepared.errors = []error{errors.New("one"), errors.New("two")}
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{}}, Force: true})
		if err == nil {
			t.Fatal("stream failure missing")
		}
	})
	for _, completion := range []llm.Completion{{Message: assistantSummary("")}, {Message: assistantSummary("summary"), Calls: []session.ToolCall{{ID: "c"}}}} {
		t.Run("invalid summary", func(t *testing.T) {
			harness := newCompactionHarness(t)
			harness.prepared.results = []llm.Completion{completion}
			if _, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{}}, Force: true}); err == nil {
				t.Fatal("invalid summary accepted")
			}
		})
	}
	t.Run("summary append", func(t *testing.T) {
		harness := newCompactionHarness(t)
		failure := errors.New("summary append")
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{2: failure}}, Force: true})
		if !errors.Is(err, failure) {
			t.Fatalf("summary append=%v", err)
		}
	})
	t.Run("end append", func(t *testing.T) {
		harness := newCompactionHarness(t)
		failure := errors.New("end append")
		_, err := harness.service.Maybe(context.Background(), Request{Journal: &compactionJournal{events: visibleEvents(4), failures: map[int]error{3: failure}}, Force: true})
		if !errors.Is(err, failure) {
			t.Fatalf("end append=%v", err)
		}
	})
}

func TestFailureClassificationEstimationSelectionAndWait(t *testing.T) {
	for err, expected := range map[error]string{
		&llm.Error{Code: llm.ErrorRateLimit}: "rate_limit",
		context.Canceled:                     "cancelled",
		context.DeadlineExceeded:             "timeout",
		errors.New("plain"):                  "compaction_failed",
	} {
		if got := safeFailure(err); got != expected {
			t.Fatalf("safeFailure(%v)=%q", err, got)
		}
	}
	surface := []session.SurfaceNode{
		{Message: &session.Message{Content: []session.ContentBlock{{Type: session.ContentText, Text: "12345678"}, {Type: session.ContentImage}}}},
		{Call: &session.ToolCall{ID: "call", Name: "tool", Arguments: []byte(`{}`)}},
		{Result: &session.ToolResult{CallID: "call", Output: "result"}},
		{Message: &session.Message{Content: []session.ContentBlock{{Type: session.ContentText, Text: "tail"}}}},
	}
	if estimateSurface(surface) <= 1024 {
		t.Fatal("surface estimate omitted nodes")
	}
	if prefix, _ := selectPrefix(surface[:2], 1, true); prefix != nil {
		t.Fatal("short surface selected")
	}
	if prefix, _ := selectPrefix(surface, 100_000, false); prefix != nil {
		t.Fatal("fully retained surface selected")
	}
	if prefix, count := selectPrefix(surface, 100_000, true); len(prefix) != 1 || count == 0 {
		t.Fatalf("forced prefix=%#v count=%d", prefix, count)
	}
	if prefix, _ := selectPrefix(surface, 2, false); len(prefix) != 1 {
		t.Fatalf("tool pair split: %#v", prefix)
	}
	if prefix, _ := selectPrefix(surface, 0, false); len(prefix) != len(surface) {
		t.Fatalf("zero retain prefix=%#v", prefix)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(wait(ctx, time.Hour), context.Canceled) {
		t.Fatal("wait cancellation")
	}
	if err := wait(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

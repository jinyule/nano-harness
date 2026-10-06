package web

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestParseQueries_ECMAScriptWhitespace(t *testing.T) {
	for _, query := range []string{"\ufeff", "\ufeff\t\u2028\u3000"} {
		if _, err := ParseQueries([]string{query}); err == nil || err.Error() != "each query must be a non-empty string" {
			t.Errorf("blank query %q: %v", query, err)
		}
	}
	for _, query := range []string{"\u0085", "\ufeff query \ufeff", "\u200b"} {
		got, err := ParseQueries([]string{query, query})
		if err != nil || len(got) != 1 || got[0] != query {
			t.Errorf("query spelling/dedup %q: %q, %v", query, got, err)
		}
	}
}

type webStore struct{ err error }

func (store webStore) Resolve(context.Context, string, string) (llm.Credential, error) {
	return llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, store.err
}
func (webStore) Modify(context.Context, string, func(*llm.Credential) (*llm.Credential, error)) (llm.Credential, error) {
	return llm.Credential{}, nil
}
func (webStore) Delete(context.Context, string) error            { return nil }
func (webStore) List(context.Context) ([]llm.AccountInfo, error) { return nil, nil }

// searchModel is the remote-model boundary; everything above it is real.
type searchModel struct {
	prepareErr error
	search     func(context.Context, llm.SearchRequest) (llm.SearchResult, error)

	mu       sync.Mutex
	requests []llm.SearchRequest
}

func (*searchModel) ID() string                    { return "openai" }
func (*searchModel) Models() []llm.ModelInfo       { return nil }
func (*searchModel) AuthMethods() []llm.AuthMethod { return nil }
func (*searchModel) Login(context.Context, string, llm.AuthInteraction) (llm.Credential, error) {
	return llm.Credential{}, nil
}
func (model *searchModel) Prepare(string) (llm.PreparedModel, error) {
	return model, model.prepareErr
}
func (*searchModel) Info() llm.ModelInfo   { return llm.ModelInfo{Provider: "openai", ID: "gpt-5.4"} }
func (*searchModel) CredentialEnv() string { return "OPENAI_API_KEY" }
func (*searchModel) Refresh(_ context.Context, credential llm.Credential) (llm.Credential, error) {
	return credential, nil
}
func (*searchModel) Stream(context.Context, llm.Credential, llm.Request, llm.Emit) (llm.Completion, error) {
	return llm.Completion{}, nil
}
func (model *searchModel) Search(ctx context.Context, _ llm.Credential, request llm.SearchRequest) (llm.SearchResult, error) {
	model.mu.Lock()
	model.requests = append(model.requests, request)
	model.mu.Unlock()
	if err := request.RecordRequest(ctx, session.WebSearchRequest{Provider: "openai", Model: "gpt-5.4", Endpoint: "openai-responses", Query: request.Query, TimeoutMS: request.TimeoutMS, MaxResults: request.MaxResults}); err != nil {
		return llm.SearchResult{}, fmt.Errorf("%w: %w", llm.ErrSearchAudit, err)
	}
	return model.search(ctx, request)
}

type staticBackend struct{ document settings.Document }

func (backend staticBackend) Load(context.Context) (settings.Document, error) {
	return backend.document, nil
}
func (staticBackend) Persist(context.Context, settings.Document) error { return nil }
func (staticBackend) Watch(ctx context.Context, _ func(settings.Document, error)) error {
	<-ctx.Done()
	return nil
}

type fetcherFunc func(context.Context, string) (FetchResult, error)

func (function fetcherFunc) Fetch(ctx context.Context, url string) (FetchResult, error) {
	return function(ctx, url)
}

type fixture struct {
	service       *Service
	model         *searchModel
	settingsScope *plugin.Scope
	scope         *plugin.Scope
}

func newFixture(t *testing.T, search settings.WebSearch, store webStore, fetcher Fetcher) *fixture {
	t.Helper()
	configuration := settings.New()
	settingsScope := &plugin.Scope{}
	if err := configuration.Start(context.Background(), settingsScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsScope.Close(context.Background()) })
	if err := configuration.Mount(context.Background(), &staticBackend{document: settings.Document{Web: settings.Web{Search: search}}}, settingsScope); err != nil {
		t.Fatal(err)
	}
	models, err := llm.New(store, noImages{})
	if err != nil {
		t.Fatal(err)
	}
	modelScope := &plugin.Scope{}
	if err := models.Start(context.Background(), modelScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelScope.Close(context.Background()) })
	model := &searchModel{search: func(context.Context, llm.SearchRequest) (llm.SearchResult, error) { return llm.SearchResult{}, nil }}
	if err := models.Register(model, modelScope); err != nil {
		t.Fatal(err)
	}
	if fetcher == nil {
		fetcher = fetcherFunc(func(context.Context, string) (FetchResult, error) { return FetchResult{}, nil })
	}
	service, err := New(models, configuration, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return &fixture{service: service, model: model, settingsScope: settingsScope, scope: scope}
}

var configured = settings.WebSearch{Provider: "openai", Model: "gpt-5.4"}

func sources(prefix string, count int) []llm.SearchSource {
	list := make([]llm.SearchSource, count)
	for index := range list {
		list[index] = llm.SearchSource{URL: fmt.Sprintf("https://%s.example/%d", prefix, index)}
	}
	return list
}

// expectCode asserts the structured code and returns the model-visible message.
func expectCode(t *testing.T, err error, code Code) string {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error=%v, want %s", err, code)
	}
	return failure.Message
}

func TestService_LifecycleRejectsWorkOutsideScope(t *testing.T) {
	if _, err := New(nil, nil, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil dependencies=%v", err)
	}
	current := newFixture(t, configured, webStore{}, nil)
	if current.service.ID() != "web" {
		t.Fatal("plugin ID")
	}
	if err := current.service.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double start=%v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	inactive, _ := New(current.service.models, current.service.settings, current.service.fetcher)
	if err := inactive.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
	if _, err := inactive.Search(context.Background(), []string{"go"}, searchOwner()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("search before start=%v", err)
	}
	if _, err := inactive.Fetch(context.Background(), "https://go.dev"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("fetch before start=%v", err)
	}
	if err := current.scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := current.service.Search(context.Background(), []string{"go"}, searchOwner()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("search after close=%v", err)
	}
	failure := &Error{Code: CodeAborted, Message: "stopped", Cause: context.Canceled}
	if failure.Error() != "WEB_ABORTED: stopped" || !errors.Is(failure, context.Canceled) {
		t.Fatalf("error contract=%q", failure.Error())
	}
}

// blockingOperation is a provider call that reports its start, reports when it
// observes cancellation, then holds until released before reporting its return.
type blockingOperation struct {
	started, cancelled, release, returned chan struct{}
	once                                  sync.Once
}

func newBlockingOperation() *blockingOperation {
	return &blockingOperation{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
}

// run holds the provider call after cancellation until release. A call that
// is never cancelled also returns on release, so a failed test still lets
// cleanup finish instead of hanging until the package timeout.
func (operation *blockingOperation) run(ctx context.Context) error {
	close(operation.started)
	select {
	case <-ctx.Done():
		close(operation.cancelled)
		<-operation.release
	case <-operation.release:
	}
	close(operation.returned)
	return ctx.Err()
}

func (operation *blockingOperation) unblock() { operation.once.Do(func() { close(operation.release) }) }

// awaitCancelled bounds the wait for cleanup to cancel an in-flight call.
func (operation *blockingOperation) awaitCancelled(t *testing.T, name string) {
	t.Helper()
	select {
	case <-operation.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatalf("cleanup did not cancel the in-flight %s", name)
	}
}

// Quiescence covers the service's own work: Close returns only after every
// provider call has returned and its operation is unregistered. Callers observe
// their results after that point, on their own goroutines, so the test waits for
// those results instead of expecting them to be ready when Close returns.
func TestService_ShutdownCancelsAndWaitsForInFlightOperations(t *testing.T) {
	fetch, search := newBlockingOperation(), newBlockingOperation()
	current := newFixture(t, configured, webStore{}, fetcherFunc(func(ctx context.Context, _ string) (FetchResult, error) {
		return FetchResult{}, fetch.run(ctx)
	}))
	// Registered after the fixture, so it runs before the fixture's own
	// cleanup and releases calls a failed assertion left blocked.
	t.Cleanup(func() { search.unblock(); fetch.unblock() })
	current.model.search = func(ctx context.Context, _ llm.SearchRequest) (llm.SearchResult, error) {
		return llm.SearchResult{}, search.run(ctx)
	}
	searchDone := make(chan error, 1)
	fetchDone := make(chan error, 1)
	go func() {
		_, err := current.service.Search(context.Background(), []string{"go"}, searchOwner())
		searchDone <- err
	}()
	go func() {
		_, err := current.service.Fetch(context.Background(), "https://go.dev")
		fetchDone <- err
	}()
	<-search.started
	<-fetch.started
	closed := make(chan error, 1)
	go func() { closed <- current.scope.Close(context.Background()) }()
	search.awaitCancelled(t, "search")
	fetch.awaitCancelled(t, "fetch")
	// Both provider calls are cancelled but held, so their operations are still
	// registered and Close must still be waiting.
	select {
	case <-closed:
		t.Fatal("shutdown returned while provider calls were still running")
	default:
	}
	if _, err := current.service.Search(context.Background(), []string{"late"}, searchOwner()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("search during shutdown=%v", err)
	}
	search.unblock()
	fetch.unblock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]*blockingOperation{"search": search, "fetch": fetch} {
		select {
		case <-operation.returned:
		default:
			t.Fatalf("shutdown returned before the %s provider call returned", name)
		}
	}
	expectCode(t, <-searchDone, CodeAborted)
	if err := <-fetchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch after shutdown=%v", err)
	}
}

func TestService_SearchValidatesQueriesBeforeProviderCalls(t *testing.T) {
	current := newFixture(t, configured, webStore{}, nil)
	for _, test := range []struct {
		queries []string
		message string
	}{
		{nil, "at least one query"},
		{[]string{"a", "b", "c", "d", "e"}, "at most 4 queries"},
		{[]string{"go", " \t"}, "non-empty string"},
	} {
		if _, err := current.service.Search(context.Background(), test.queries, searchOwner()); err == nil || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("queries %q error=%v", test.queries, err)
		}
	}
	if len(current.model.requests) != 0 {
		t.Fatal("invalid queries reached the provider")
	}
	accepted, err := ParseQueries([]string{"go", "rust", "go", "Go"})
	if err != nil || !reflect.DeepEqual(accepted, []string{"go", "rust", "Go"}) {
		t.Fatalf("deduplicated=%q err=%v", accepted, err)
	}
}

func TestService_SearchRequiresConfiguredRoute(t *testing.T) {
	current := newFixture(t, settings.WebSearch{}, webStore{}, nil)
	message := expectCode(t, func() error {
		_, err := current.service.Search(context.Background(), []string{"go"}, searchOwner())
		return err
	}(), CodeProviderUnavailable)
	if !strings.Contains(message, "web.search.provider") {
		t.Fatalf("message=%q", message)
	}
	if err := current.settingsScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := current.service.Search(context.Background(), []string{"go"}, searchOwner()); !errors.Is(err, settings.ErrNotRunning) {
		t.Fatalf("stopped settings=%v", err)
	}
}

func TestService_SearchCapsSingleQueryResults(t *testing.T) {
	current := newFixture(t, configured, webStore{}, nil)
	current.model.search = func(context.Context, llm.SearchRequest) (llm.SearchResult, error) {
		return llm.SearchResult{Content: "answer", Sources: sources("a", MaxResults+2)}, nil
	}
	result, err := current.service.Search(context.Background(), []string{"go release"}, searchOwner())
	if err != nil || result.Content != "answer" || !result.Truncated || !reflect.DeepEqual(result.Sources, sources("a", MaxResults)) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(current.model.requests) != 1 || current.model.requests[0].Query != "go release" || current.model.requests[0].MaxResults != MaxResults || current.model.requests[0].TimeoutMS != 60000 || current.model.requests[0].RecordRequest == nil {
		t.Fatalf("requests=%#v", current.model.requests)
	}
	current.model.search = func(context.Context, llm.SearchRequest) (llm.SearchResult, error) {
		return llm.SearchResult{Sources: sources("a", MaxResults)}, nil
	}
	exact, err := current.service.Search(context.Background(), []string{"go"}, searchOwner())
	if err != nil || exact.Truncated || len(exact.Sources) != MaxResults {
		t.Fatalf("exact=%#v err=%v", exact, err)
	}
}

func TestService_SearchRunsQueriesConcurrentlyAndMerges(t *testing.T) {
	current := newFixture(t, configured, webStore{}, nil)
	var arrived sync.WaitGroup
	arrived.Add(2)
	current.model.search = func(_ context.Context, request llm.SearchRequest) (llm.SearchResult, error) {
		// Each query waits for the other, so a serial implementation would deadlock.
		arrived.Done()
		arrived.Wait()
		if request.Query == "alpha" {
			return llm.SearchResult{Content: "first", Sources: []llm.SearchSource{{URL: "https://shared.example"}, {URL: "https://a.example/1"}, {URL: "https://a.example/2"}}}, nil
		}
		return llm.SearchResult{Sources: []llm.SearchSource{{URL: "https://shared.example", Title: "later duplicate"}, {URL: "https://b.example/1"}}}, nil
	}
	result, err := current.service.Search(context.Background(), []string{"alpha", "beta", "alpha"}, searchOwner())
	if err != nil {
		t.Fatal(err)
	}
	want := SearchResult{Content: "### alpha\n\nfirst", Sources: []llm.SearchSource{
		{URL: "https://shared.example"}, {URL: "https://a.example/1"}, {URL: "https://b.example/1"}, {URL: "https://a.example/2"},
	}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("merged=%#v", result)
	}
}

func TestMergeResults_CapsAndPropagatesTruncation(t *testing.T) {
	merged := mergeResults([]string{"a", "b"}, []SearchResult{
		{Content: "one", Sources: sources("a", 5)},
		{Content: "two", Sources: sources("b", 5)},
	})
	if len(merged.Sources) != MaxResults || !merged.Truncated || merged.Content != "### a\n\none\n\n### b\n\ntwo" {
		t.Fatalf("merged=%#v", merged)
	}
	if merged.Sources[0].URL != "https://a.example/0" || merged.Sources[1].URL != "https://b.example/0" {
		t.Fatalf("round robin order=%#v", merged.Sources[:2])
	}
	exact := mergeResults([]string{"a", "b"}, []SearchResult{{Sources: sources("a", 4)}, {Sources: sources("b", 4)}})
	if len(exact.Sources) != MaxResults || exact.Truncated {
		t.Fatalf("exact cap flagged truncation: %#v", exact)
	}
	flagged := mergeResults([]string{"a", "b"}, []SearchResult{{Sources: sources("a", 1), Truncated: true}, {}})
	if !flagged.Truncated || len(flagged.Sources) != 1 {
		t.Fatalf("provider truncation lost: %#v", flagged)
	}
}

func TestService_SearchFirstFailureCancelsSiblings(t *testing.T) {
	current := newFixture(t, configured, webStore{}, nil)
	failure := &llm.Error{Code: llm.ErrorRateLimit, Provider: "openai", HTTPStatus: 429}
	siblingCancelled := make(chan struct{})
	current.model.search = func(ctx context.Context, request llm.SearchRequest) (llm.SearchResult, error) {
		if request.Query == "fails" {
			return llm.SearchResult{}, failure
		}
		<-ctx.Done()
		close(siblingCancelled)
		return llm.SearchResult{}, ctx.Err()
	}
	_, err := current.service.Search(context.Background(), []string{"waits", "fails"}, searchOwner())
	message := expectCode(t, err, CodeProviderError)
	if !errors.Is(err, failure) || !strings.Contains(message, "rate_limit") || !strings.Contains(message, "openai/gpt-5.4") {
		t.Fatalf("failure=%v", err)
	}
	select {
	case <-siblingCancelled:
	default:
		t.Fatal("search returned before the cancelled sibling settled")
	}
}

func TestService_SearchClassifiesDeadlineCancellationAndAccountFailures(t *testing.T) {
	current := newFixture(t, configured, webStore{}, nil)
	current.service.searchTimeout = 20 * time.Millisecond
	current.model.search = func(ctx context.Context, _ llm.SearchRequest) (llm.SearchResult, error) {
		<-ctx.Done()
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorTimeout, Provider: "openai", Cause: ctx.Err()}
	}
	_, err := current.service.Search(context.Background(), []string{"slow"}, searchOwner())
	expectCode(t, err, CodeSearchTimeout)

	current.service.searchTimeout = SearchTimeout
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	current.model.search = func(ctx context.Context, _ llm.SearchRequest) (llm.SearchResult, error) {
		close(started)
		<-ctx.Done()
		return llm.SearchResult{}, ctx.Err()
	}
	go func() {
		<-started
		cancel()
	}()
	_, err = current.service.Search(ctx, []string{"cancelled"}, searchOwner())
	expectCode(t, err, CodeAborted)

	missing := newFixture(t, configured, webStore{err: llm.ErrNoCredential}, nil)
	_, err = missing.service.Search(context.Background(), []string{"go"}, searchOwner())
	if message := expectCode(t, err, CodeCredentialMissing); !strings.Contains(message, `"openai"`) {
		t.Fatalf("message=%q", message)
	}

	unknown := newFixture(t, configured, webStore{}, nil)
	unknown.model.prepareErr = fmt.Errorf("%w: openai/gpt-5.4", llm.ErrUnknownModel)
	_, err = unknown.service.Search(context.Background(), []string{"go"}, searchOwner())
	if !errors.Is(err, llm.ErrUnknownModel) {
		t.Fatalf("prepare failure=%v", err)
	}
	expectCode(t, err, CodeProviderError)
}

func TestService_FetchDelegatesWithinOperationScope(t *testing.T) {
	want := FetchResult{URL: "https://go.dev/", StatusCode: 404, Kind: FetchText, Content: "missing"}
	var received string
	current := newFixture(t, configured, webStore{}, fetcherFunc(func(ctx context.Context, url string) (FetchResult, error) {
		if ctx.Err() != nil {
			t.Error("fetch received a cancelled context")
		}
		received = url
		return want, nil
	}))
	if _, err := current.service.Fetch(context.Background(), "  "); err == nil || !strings.Contains(err.Error(), "url must be") {
		t.Fatalf("blank URL=%v", err)
	}
	result, err := current.service.Fetch(context.Background(), "https://go.dev")
	if err != nil || result != want || received != "https://go.dev" {
		t.Fatalf("fetch=%#v err=%v received=%q", result, err, received)
	}
}

// noImages is an attachment store that holds no image.
type noImages struct{}

func (noImages) ReadImage(context.Context, session.Image) ([]byte, error) {
	return nil, session.ErrAttachmentMissing
}

type acceptingJournal struct{}

func (acceptingJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	return session.Event{Record: record}, nil
}
func searchOwner() SearchInvocation {
	return SearchInvocation{Journal: acceptingJournal{}, Turn: 1, Step: 1, CallID: "search"}
}

func TestError_ToolErrorKeepsTheCodeAndText(t *testing.T) {
	failure := &Error{Code: CodeBlockedURL, Message: "blocked", Cause: errors.New("private")}
	var classified interface{ ToolError() session.ToolError }
	if !errors.As(fmt.Errorf("fetch: %w", failure), &classified) || classified.ToolError() != (session.ToolError{Name: "WebError", Code: "WEB_BLOCKED_URL"}) || failure.Error() != "WEB_BLOCKED_URL: blocked" {
		t.Fatalf("classification=%+v text=%q", classified, failure.Error())
	}
	if err := classified.ToolError().Validate(); err != nil {
		t.Fatal(err)
	}
}

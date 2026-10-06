// Package web owns the provider-neutral web search and public fetch use cases.
package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	"github.com/jinyule/nano-harness/internal/core/text"
)

const (
	// MaxQueries bounds the queries accepted by one search call; the tool schema states it.
	MaxQueries = 4
	// MaxResults bounds the sources returned by one search call.
	MaxResults = 8
	// SearchTimeout bounds one search call, including every concurrent query and
	// the account preparation, because each query is a full model request.
	SearchTimeout = 60 * time.Second
)

var (
	// ErrInvalidConfig identifies a web service composition that cannot run.
	ErrInvalidConfig = errors.New("invalid web configuration")
	// ErrNotRunning indicates the web service has not started or has stopped.
	ErrNotRunning = errors.New("web service is not running")

	errSearchTimeout = errors.New("web search deadline")
)

// Code is a stable, model-visible failure class for web operations.
type Code string

const (
	// CodeProviderUnavailable means no search route is configured.
	CodeProviderUnavailable Code = "WEB_PROVIDER_UNAVAILABLE"
	// CodeCredentialMissing means the configured search provider has no account.
	CodeCredentialMissing Code = "WEB_PROVIDER_CREDENTIAL_MISSING" //nolint:gosec // an error code name, not a credential
	// CodeProviderError covers provider, transport, and protocol failures.
	CodeProviderError Code = "WEB_PROVIDER_ERROR"
	// CodeRequestRecordFailed means the request intent could not be committed.
	CodeRequestRecordFailed Code = "WEB_REQUEST_RECORD_FAILED"
	// CodeAborted means the caller or service shutdown cancelled the operation.
	CodeAborted Code = "WEB_ABORTED"
	// CodeSearchTimeout means one search call exceeded SearchTimeout.
	CodeSearchTimeout Code = "WEB_SEARCH_TIMEOUT"
	// CodeInvalidURL means a fetch URL is malformed, too long, or not HTTP(S).
	CodeInvalidURL Code = "WEB_INVALID_URL"
	// CodeBlockedURL means a fetch URL carries credentials or reaches a non-public address.
	CodeBlockedURL Code = "WEB_BLOCKED_URL"
	// CodeRedirectBlocked means a redirect crossed origins or exceeded the hop limit.
	CodeRedirectBlocked Code = "WEB_REDIRECT_BLOCKED"
	// CodeFetchTooLarge means a response exceeds a declared, encoded, or intermediate fetch limit.
	CodeFetchTooLarge Code = "WEB_FETCH_TOO_LARGE"
	// CodeFetchTimeout means one fetch exceeded its deadline.
	CodeFetchTimeout Code = "WEB_FETCH_TIMEOUT"
	// CodeUnsupportedContent means the response type or charset cannot be decoded as text.
	CodeUnsupportedContent Code = "WEB_UNSUPPORTED_CONTENT_TYPE"
)

// Error is a structured web failure. Message is safe to show the model; it never
// contains credentials or remote error bodies.
type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (failure *Error) Error() string { return string(failure.Code) + ": " + failure.Message }

func (failure *Error) Unwrap() error { return failure.Cause }

// SearchResult is the merged outcome of one search call.
type SearchResult struct {
	Content   string
	Sources   []llm.SearchSource
	Truncated bool
}

// FetchKind classifies decoded fetch content for presentation.
type FetchKind string

const (
	// FetchHTML is decoded HTML or XHTML markup.
	FetchHTML FetchKind = "html"
	// FetchText is decoded plain text, JSON, or XML.
	FetchText FetchKind = "text"
)

// FetchResult is one retrieved resource. A non-2xx status is a result, not an
// error; URL is the final URL after allowed redirects.
type FetchResult struct {
	URL        string
	StatusCode int
	Kind       FetchKind
	Content    string
	Truncated  bool
}

// Fetcher retrieves one public HTTP(S) URL and owns its network policy, limits,
// and Error codes.
type Fetcher interface {
	Fetch(context.Context, string) (FetchResult, error)
}

// Service runs web operations until scope cleanup, which cancels and waits for
// every operation still in flight. The wait ends once each operation's provider
// calls have returned and it has unregistered; the caller then receives the
// result on its own goroutine, after cleanup may already have returned.
type Service struct {
	models        *llm.Runtime
	settings      *settings.Service
	fetcher       Fetcher
	searchTimeout time.Duration

	mu         sync.Mutex
	started    bool
	active     bool
	nextID     uint64
	operations map[uint64]context.CancelFunc
	group      sync.WaitGroup
}

// New constructs an inactive web service.
func New(models *llm.Runtime, configuration *settings.Service, fetcher Fetcher) (*Service, error) {
	if models == nil || configuration == nil || fetcher == nil {
		return nil, ErrInvalidConfig
	}
	return &Service{models: models, settings: configuration, fetcher: fetcher, searchTimeout: SearchTimeout, operations: map[uint64]context.CancelFunc{}}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "web" }

// Start accepts operations until scope cleanup.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidConfig
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		for _, cancel := range service.operations {
			cancel()
		}
		service.mu.Unlock()
		// Every operation observes its cancelled context, so the wait is bounded
		// by the providers' cancellation latency.
		service.group.Wait()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active = true, true
	return nil
}

// begin registers one cancellable operation; done must be called exactly once.
func (service *Service) begin(ctx context.Context) (context.Context, func(), error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return nil, nil, ErrNotRunning
	}
	operation, cancel := context.WithCancel(ctx)
	service.nextID++
	id := service.nextID
	service.operations[id] = cancel
	service.group.Add(1)
	return operation, func() {
		cancel()
		service.mu.Lock()
		delete(service.operations, id)
		service.mu.Unlock()
		service.group.Done()
	}, nil
}

// Journal commits facts to the session that owns a search call.
type Journal interface {
	Append(context.Context, session.Record) (session.Event, error)
}

// SearchInvocation binds each request intent to its pending tool call.
// Journal is mandatory; standalone searches fail closed.
type SearchInvocation struct {
	Journal    Journal
	Turn, Step uint64
	CallID     string
}

// Search runs every accepted query through the configured route with one
// account preparation. Multiple queries run concurrently; the first failure
// cancels the others and is returned after all have settled.
func (service *Service) Search(ctx context.Context, queries []string, invocation SearchInvocation) (SearchResult, error) {
	accepted, err := ParseQueries(queries)
	if err != nil {
		return SearchResult{}, err
	}
	ctx, done, err := service.begin(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	defer done()
	if invocation.Journal == nil {
		return SearchResult{}, errors.New("web_search requires an owning agent session")
	}
	document, _, err := service.settings.Snapshot()
	if err != nil {
		return SearchResult{}, err
	}
	route := document.Web.Search
	if !route.Configured() {
		return SearchResult{}, &Error{Code: CodeProviderUnavailable, Message: "web search is not configured; set web.search.provider and web.search.model in settings"}
	}
	ctx, cancel := context.WithTimeoutCause(ctx, service.searchTimeout, errSearchTimeout)
	defer cancel()
	call, err := service.models.PrepareCall(ctx, route.Provider, route.Model)
	if err != nil {
		return SearchResult{}, service.searchError(ctx, route, err)
	}
	results, err := runQueries(ctx, call, accepted, invocation, service.searchTimeout.Milliseconds())
	if err != nil {
		return SearchResult{}, service.searchError(ctx, route, err)
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return mergeResults(accepted, results), nil
}

// ParseQueries accepts 1–MaxQueries nonblank queries using ECMAScript whitespace,
// then collapses exact duplicates in first-occurrence order, preserving the text.
// Search dispatch and durable request validation share this rule.
func ParseQueries(queries []string) ([]string, error) {
	switch {
	case len(queries) == 0:
		return nil, errors.New("queries must contain at least one query")
	case len(queries) > MaxQueries:
		return nil, fmt.Errorf("queries must contain at most %d queries", MaxQueries)
	}
	accepted := make([]string, 0, len(queries))
	seen := map[string]struct{}{}
	for _, query := range queries {
		if text.TrimSpace(query) == "" {
			return nil, errors.New("each query must be a non-empty string")
		}
		if _, ok := seen[query]; !ok {
			seen[query] = struct{}{}
			accepted = append(accepted, query)
		}
	}
	return accepted, nil
}

func runQueries(ctx context.Context, call *llm.Call, queries []string, invocation SearchInvocation, timeoutMS int64) ([]SearchResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		group sync.WaitGroup
		once  sync.Once
		first error
	)
	results := make([]SearchResult, len(queries))
	previous := make(chan struct{})
	close(previous)
	for index, query := range queries {
		ready, committed := previous, make(chan struct{})
		previous = committed
		group.Go(func() {
			found, err := call.Search(ctx, llm.SearchRequest{Query: query, MaxResults: MaxResults, TimeoutMS: timeoutMS,
				RecordRequest: func(ctx context.Context, data session.WebSearchRequest) error {
					// Only appends are ordered; dispatched queries overlap. A failed
					// append leaves the gate closed until cancellation releases waiters.
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-ready:
					}
					data.CallID, data.Index = invocation.CallID, index+1
					_, err := invocation.Journal.Append(ctx, session.Record{Type: session.RecordWebSearchRequest, Turn: invocation.Turn, Step: invocation.Step, Search: &data})
					if err != nil {
						return err
					}
					close(committed)
					return nil
				},
			})
			if err != nil {
				once.Do(func() {
					first = err
					cancel()
				})
				return
			}
			results[index] = capSources(found)
		})
	}
	group.Wait()
	return results, first
}

func capSources(found llm.SearchResult) SearchResult {
	result := SearchResult{Content: found.Content, Sources: found.Sources}
	if len(result.Sources) > MaxResults {
		result.Sources, result.Truncated = result.Sources[:MaxResults:MaxResults], true
	}
	return result
}

// mergeResults interleaves sources round-robin by rank, drops repeated URLs,
// and caps the list; answers are labeled with their query.
func mergeResults(queries []string, results []SearchResult) SearchResult {
	ranks := 0
	for _, result := range results {
		ranks = max(ranks, len(result.Sources))
	}
	seen := map[string]struct{}{}
	var merged SearchResult
	for rank := 0; rank < ranks && !merged.Truncated; rank++ {
		for _, result := range results {
			if rank >= len(result.Sources) {
				continue
			}
			source := result.Sources[rank]
			if _, ok := seen[source.URL]; ok {
				continue
			}
			seen[source.URL] = struct{}{}
			if len(merged.Sources) == MaxResults {
				merged.Truncated = true
				break
			}
			merged.Sources = append(merged.Sources, source)
		}
	}
	answers := make([]string, 0, len(results))
	for index, result := range results {
		merged.Truncated = merged.Truncated || result.Truncated
		if result.Content != "" {
			answers = append(answers, "### "+queries[index]+"\n\n"+result.Content)
		}
	}
	merged.Content = strings.Join(answers, "\n\n")
	return merged
}

// searchError classifies a failure by the operation context first, because a
// deadline or cancellation also surfaces as provider transport errors.
func (service *Service) searchError(ctx context.Context, route settings.WebSearch, err error) error {
	target := route.Provider + "/" + route.Model
	var failure *llm.Error
	switch {
	case errors.Is(context.Cause(ctx), errSearchTimeout):
		return &Error{Code: CodeSearchTimeout, Message: fmt.Sprintf("web search through %s timed out after %s", target, service.searchTimeout), Cause: err}
	case ctx.Err() != nil:
		return &Error{Code: CodeAborted, Message: "web search was cancelled", Cause: err}
	case errors.Is(err, llm.ErrSearchAudit):
		return &Error{Code: CodeRequestRecordFailed, Message: "could not persist web search request; request was not sent", Cause: err}
	case errors.Is(err, llm.ErrNoCredential):
		return &Error{Code: CodeCredentialMissing, Message: fmt.Sprintf("provider %q has no account for web search; ask the user to log in to it", route.Provider), Cause: err}
	case errors.As(err, &failure):
		return &Error{Code: CodeProviderError, Message: fmt.Sprintf("web search through %s failed: %s", target, failure.Error()), Cause: err}
	default:
		return &Error{Code: CodeProviderError, Message: fmt.Sprintf("web search through %s failed: %v", target, err), Cause: err}
	}
}

// Fetch retrieves one URL through the configured fetcher.
func (service *Service) Fetch(ctx context.Context, rawURL string) (FetchResult, error) {
	if strings.TrimSpace(rawURL) == "" {
		return FetchResult{}, errors.New("url must be a non-empty string")
	}
	ctx, done, err := service.begin(ctx)
	if err != nil {
		return FetchResult{}, err
	}
	defer done()
	return service.fetcher.Fetch(ctx, rawURL)
}

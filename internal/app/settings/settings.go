// Package settings owns validated, hot-reloadable user configuration.
package settings

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

var (
	// ErrInvalidDocument identifies a settings document that the running composition cannot honor.
	ErrInvalidDocument = errors.New("invalid settings document")
	// ErrNotRunning indicates the settings service has not started or has stopped.
	ErrNotRunning = errors.New("settings service is not running")
	// ErrProviderMounted indicates a second settings provider or service start was attempted.
	ErrProviderMounted = errors.New("settings provider is already mounted")
)

// Route is the default provider/model used by future request preparations.
type Route struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// Model describes provider-owned catalog facts used before a request.
type Model struct {
	ID            string `yaml:"id" json:"id"`
	Name          string `yaml:"name" json:"name"`
	ContextWindow int    `yaml:"context_window" json:"context_window"`
	Vision        bool   `yaml:"vision" json:"vision"`
	Tools         bool   `yaml:"tools" json:"tools"`
}

// Provider configures one of the three installed wire providers.
type Provider struct {
	BaseURL   string  `yaml:"base_url" json:"base_url"`
	APIKeyEnv string  `yaml:"api_key_env" json:"api_key_env"`
	Models    []Model `yaml:"models" json:"models"`
}

// Retry configures provider-routed request recovery.
type Retry struct {
	Mode           string  `yaml:"mode" json:"mode"`
	MaxRetries     int     `yaml:"max_retries" json:"max_retries"`
	InitialDelayMS int64   `yaml:"initial_delay_ms" json:"initial_delay_ms"`
	MaxDelayMS     int64   `yaml:"max_delay_ms" json:"max_delay_ms"`
	JitterRatio    float64 `yaml:"jitter_ratio" json:"jitter_ratio"`
}

// Compaction configures pressure and retained-tail policy.
type Compaction struct {
	ThresholdRatio float64 `yaml:"threshold_ratio" json:"threshold_ratio"`
	RetainRatio    float64 `yaml:"retain_ratio" json:"retain_ratio"`
	MaxTokens      int     `yaml:"max_tokens" json:"max_tokens"`
	Retries        int     `yaml:"retries" json:"retries"`
}

// Document is the complete hot-reloadable configuration.
type Document struct {
	Route      Route               `yaml:"route" json:"route"`
	Providers  map[string]Provider `yaml:"providers" json:"providers"`
	Retry      Retry               `yaml:"retry" json:"retry"`
	Compaction Compaction          `yaml:"compaction" json:"compaction"`
}

// Defaults returns a detached usable document.
func Defaults() Document {
	return Document{
		Route: Route{Provider: "openai", Model: "gpt-5.6-luna"},
		Providers: map[string]Provider{
			"openai": { //nolint:gosec // this block contains an environment-variable name, not a credential
				BaseURL: "https://api.openai.com", APIKeyEnv: "OPENAI_API_KEY",
				Models: []Model{{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", ContextWindow: 200_000, Vision: true, Tools: true}, {ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 200_000, Vision: true, Tools: true}},
			},
			"anthropic": { //nolint:gosec // this block contains an environment-variable name, not a credential
				BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY",
				Models: []Model{{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", ContextWindow: 200_000, Vision: true, Tools: true}},
			},
			"openrouter": { //nolint:gosec // this block contains an environment-variable name, not a credential
				BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY",
				Models: []Model{{ID: "openai/gpt-5.4", Name: "GPT-5.4 via OpenRouter", ContextWindow: 200_000, Vision: true, Tools: true}, {ID: "anthropic/claude-sonnet-4.5", Name: "Claude Sonnet 4.5 via OpenRouter", ContextWindow: 200_000, Vision: true, Tools: true}},
			},
		},
		Retry:      Retry{Mode: "normal", MaxRetries: 5, InitialDelayMS: 500, MaxDelayMS: 10_000, JitterRatio: 0.1},
		Compaction: Compaction{ThresholdRatio: 0.8, RetainRatio: 0.16, MaxTokens: 8192, Retries: 1},
	}
}

// Resolve layers an optional sparse user document over defaults.
func Resolve(user Document) (Document, error) {
	resolved := Defaults()
	if user.Route.Provider != "" {
		resolved.Route.Provider = user.Route.Provider
	}
	if user.Route.Model != "" {
		resolved.Route.Model = user.Route.Model
	}
	for name, provider := range user.Providers {
		base := resolved.Providers[name]
		if provider.BaseURL != "" {
			base.BaseURL = provider.BaseURL
		}
		if provider.APIKeyEnv != "" {
			base.APIKeyEnv = provider.APIKeyEnv
		}
		if provider.Models != nil {
			base.Models = slices.Clone(provider.Models)
		}
		resolved.Providers[name] = base
	}
	if user.Retry.Mode != "" {
		resolved.Retry = user.Retry
	}
	if user.Compaction.ThresholdRatio != 0 {
		resolved.Compaction = user.Compaction
	}
	if err := resolved.Validate(); err != nil {
		return Document{}, err
	}
	return cloneDocument(resolved), nil
}

// Validate rejects every configuration the running composition cannot honor.
func (document Document) Validate() error {
	if !validName(document.Route.Provider, 64) || !validName(document.Route.Model, 256) {
		return invalid("route requires provider and model")
	}
	if len(document.Providers) != 3 {
		return invalid("providers must contain exactly openai, anthropic, and openrouter")
	}
	for _, name := range []string{"openai", "anthropic", "openrouter"} {
		provider, ok := document.Providers[name]
		if !ok {
			return invalid("provider %q is missing", name)
		}
		if err := validateProvider(name, provider); err != nil {
			return err
		}
	}
	selected := document.Providers[document.Route.Provider]
	if !slices.ContainsFunc(selected.Models, func(model Model) bool { return model.ID == document.Route.Model }) {
		return invalid("route model %q is not in provider %q", document.Route.Model, document.Route.Provider)
	}
	if document.Retry.Mode != "normal" && document.Retry.Mode != "always" || document.Retry.MaxRetries < 0 || document.Retry.MaxRetries > 20 || document.Retry.InitialDelayMS < 1 || document.Retry.MaxDelayMS < document.Retry.InitialDelayMS || document.Retry.MaxDelayMS > 60_000 || document.Retry.JitterRatio < 0 || document.Retry.JitterRatio > 1 {
		return invalid("retry policy is invalid")
	}
	if document.Compaction.ThresholdRatio <= 0 || document.Compaction.ThresholdRatio >= 1 || document.Compaction.RetainRatio < 0 || document.Compaction.RetainRatio >= document.Compaction.ThresholdRatio || document.Compaction.MaxTokens < 256 || document.Compaction.MaxTokens > 65_536 || document.Compaction.Retries < 0 || document.Compaction.Retries > 4 {
		return invalid("compaction policy is invalid")
	}
	return nil
}

func validateProvider(name string, provider Provider) error {
	parsed, err := url.Parse(provider.BaseURL)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return invalid("provider %q base URL is invalid", name)
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || parsed.Hostname() != "localhost" && !net.ParseIP(parsed.Hostname()).IsLoopback()) {
		return invalid("provider %q base URL must use HTTPS or loopback HTTP", name)
	}
	if !validEnv(provider.APIKeyEnv) || len(provider.Models) == 0 || len(provider.Models) > 128 {
		return invalid("provider %q credential reference or model count is invalid", name)
	}
	seen := map[string]struct{}{}
	for _, model := range provider.Models {
		if !validName(model.ID, 256) || model.Name == "" || len(model.Name) > 256 || model.ContextWindow < 1024 || model.ContextWindow > 10_000_000 {
			return invalid("provider %q has invalid model metadata", name)
		}
		if _, exists := seen[model.ID]; exists {
			return invalid("provider %q repeats model %q", name, model.ID)
		}
		seen[model.ID] = struct{}{}
	}
	return nil
}

func validName(value string, limit int) bool {
	return value != "" && len(value) <= limit && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\r\n")
}

func validEnv(value string) bool {
	if value == "" || value[0] != '_' && (value[0] < 'A' || value[0] > 'Z') {
		return false
	}
	for index := 1; index < len(value); index++ {
		char := value[index]
		if char != '_' && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func invalid(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDocument, fmt.Sprintf(format, values...))
}

// ProviderBackend is the storage contract consumed by Service.
type ProviderBackend interface {
	Load(context.Context) (Document, error)
	Persist(context.Context, Document) error
	Watch(context.Context, func(Document, error)) error
}

// Service serializes settings writes and publishes immutable snapshots.
type Service struct {
	mu       sync.RWMutex
	writeMu  sync.Mutex
	started  bool
	active   bool
	provider ProviderBackend
	document Document
	revision uint64
	watchers map[uint64]func(Document)
	nextID   uint64
	lastErr  error
}

// New constructs an empty Service Definition.
func New() *Service { return &Service{watchers: map[uint64]func(Document){}} }

// ID returns the stable settings-service plugin identity.
func (*Service) ID() string { return "settings" }

// Start publishes defaults until a provider mounts.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrProviderMounted
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		service.watchers = map[uint64]func(Document){}
		service.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active, service.document = true, true, Defaults()
	return nil
}

// Mount loads one provider and starts its live reload under the caller's scope.
func (service *Service) Mount(ctx context.Context, provider ProviderBackend, scope *plugin.Scope) error {
	if provider == nil {
		return ErrInvalidDocument
	}
	loaded, err := provider.Load(ctx)
	if err != nil {
		return err
	}
	resolved, err := Resolve(loaded)
	if err != nil {
		return err
	}
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return ErrNotRunning
	}
	if service.provider != nil {
		service.mu.Unlock()
		return ErrProviderMounted
	}
	service.provider, service.document, service.revision = provider, resolved, 1
	service.mu.Unlock()
	watchContext, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- provider.Watch(watchContext, service.publish) }()
	err = scope.Defer(func(context.Context) error {
		cancel()
		watchErr := <-done
		service.mu.Lock()
		if service.provider == provider {
			service.provider = nil
		}
		service.mu.Unlock()
		return watchErr
	})
	if err != nil {
		cancel()
		<-done
		service.mu.Lock()
		if service.provider == provider {
			service.provider = nil
		}
		service.mu.Unlock()
	}
	return err
}

func (service *Service) publish(document Document, loadErr error) {
	if loadErr != nil {
		service.mu.Lock()
		service.lastErr = loadErr
		service.mu.Unlock()
		return
	}
	resolved, err := Resolve(document)
	if err != nil {
		service.mu.Lock()
		service.lastErr = err
		service.mu.Unlock()
		return
	}
	service.commit(resolved)
}

func (service *Service) commit(document Document) {
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return
	}
	service.document, service.revision, service.lastErr = cloneDocument(document), service.revision+1, nil
	watchers := make([]func(Document), 0, len(service.watchers))
	for _, watcher := range service.watchers {
		watchers = append(watchers, watcher)
	}
	snapshot := cloneDocument(document)
	service.mu.Unlock()
	for _, watcher := range watchers {
		callWatcher(watcher, cloneDocument(snapshot))
	}
}

func callWatcher(watcher func(Document), document Document) {
	defer func() { _ = recover() }()
	watcher(document)
}

// Snapshot returns the current detached document and revision.
func (service *Service) Snapshot() (Document, uint64, error) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	if !service.active {
		return Document{}, 0, ErrNotRunning
	}
	return cloneDocument(service.document), service.revision, nil
}

// LastReloadError reports the most recent rejected external edit.
func (service *Service) LastReloadError() error {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.lastErr
}

// Watch registers a contained synchronous observer.
func (service *Service) Watch(watcher func(Document)) (func(), error) {
	if watcher == nil {
		return nil, ErrInvalidDocument
	}
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return nil, ErrNotRunning
	}
	service.nextID++
	id := service.nextID
	service.watchers[id] = watcher
	service.mu.Unlock()
	return func() {
		service.mu.Lock()
		delete(service.watchers, id)
		service.mu.Unlock()
	}, nil
}

// Update persists and commits one revision-checked mutation.
func (service *Service) Update(ctx context.Context, expected uint64, mutate func(*Document) error) error {
	if mutate == nil {
		return ErrInvalidDocument
	}
	service.writeMu.Lock()
	defer service.writeMu.Unlock()
	document, revision, err := service.Snapshot()
	if err != nil {
		return err
	}
	if expected != 0 && expected != revision {
		return fmt.Errorf("settings revision changed: expected %d, got %d", expected, revision)
	}
	if err := mutate(&document); err != nil {
		return err
	}
	if err := document.Validate(); err != nil {
		return err
	}
	service.mu.RLock()
	provider := service.provider
	service.mu.RUnlock()
	if provider == nil {
		return ErrNotRunning
	}
	if err := provider.Persist(ctx, document); err != nil {
		return err
	}
	service.commit(document)
	return nil
}

func cloneDocument(document Document) Document {
	cloned := document
	cloned.Providers = make(map[string]Provider, len(document.Providers))
	for name, provider := range document.Providers {
		provider.Models = slices.Clone(provider.Models)
		cloned.Providers[name] = provider
	}
	return cloned
}

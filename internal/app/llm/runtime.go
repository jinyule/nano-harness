// Package llm owns provider routing, account records, and provider-neutral streaming.
package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidConfig identifies an LLM composition, credential, or request that cannot be honored.
	ErrInvalidConfig = errors.New("invalid LLM configuration")
	// ErrNotRunning indicates the LLM runtime has not started or has stopped.
	ErrNotRunning = errors.New("LLM runtime is not running")
	// ErrUnknownProvider identifies a provider route that is not registered.
	ErrUnknownProvider = errors.New("unknown LLM provider")
	// ErrUnknownModel identifies a model absent from the selected provider catalog.
	ErrUnknownModel = errors.New("unknown LLM model")
	// ErrNoCredential indicates neither a stored nor environment account is available.
	ErrNoCredential = errors.New("provider has no credential")
)

// ErrorCode is the stable failure class used by retry and compaction.
type ErrorCode string

const (
	// ErrorUnauthorized identifies a rejected or unusable provider credential.
	ErrorUnauthorized ErrorCode = "unauthorized"
	// ErrorRateLimit identifies provider throttling with an optional retry hint.
	ErrorRateLimit ErrorCode = "rate_limit"
	// ErrorServer identifies a provider-side server failure.
	ErrorServer ErrorCode = "server"
	// ErrorTimeout identifies a provider request deadline failure.
	ErrorTimeout ErrorCode = "timeout"
	// ErrorTransport identifies a failure before a valid provider response arrived.
	ErrorTransport ErrorCode = "transport"
	// ErrorProtocol identifies malformed or incomplete provider wire data.
	ErrorProtocol ErrorCode = "protocol"
	// ErrorInvalid identifies a request rejected as invalid and not retryable.
	ErrorInvalid ErrorCode = "invalid_request"
	// ErrorContextWindow identifies a request that exceeds the provider context window.
	ErrorContextWindow ErrorCode = "context_window_exceeded"
	// ErrorEmptyResponse identifies a completed stream without text or tool calls.
	ErrorEmptyResponse ErrorCode = "empty_response"
)

// Error preserves a safe provider failure class and retry hint.
type Error struct {
	Code         ErrorCode
	Provider     string
	HTTPStatus   int
	RetryAfterMS int64
	Cause        error
}

func (failure *Error) Error() string {
	if failure.HTTPStatus != 0 {
		return fmt.Sprintf("LLM provider %q failed: %s (HTTP %d)", failure.Provider, failure.Code, failure.HTTPStatus)
	}
	return fmt.Sprintf("LLM provider %q failed: %s", failure.Provider, failure.Code)
}

func (failure *Error) Unwrap() error { return failure.Cause }

// CredentialKind distinguishes stored API keys from provider-owned OAuth grants.
type CredentialKind string

const (
	// CredentialAPIKey identifies a provider API-key account record.
	CredentialAPIKey CredentialKind = "api-key"
	// CredentialOAuth identifies a provider-owned renewable OAuth grant.
	CredentialOAuth CredentialKind = "oauth"
)

// Credential is one provider-owned record. Extra never enters logs or prompts.
type Credential struct {
	Kind          CredentialKind    `yaml:"kind" json:"kind"`
	APIKey        string            `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	AccessToken   string            `yaml:"access_token,omitempty" json:"access_token,omitempty"`
	RefreshToken  string            `yaml:"refresh_token,omitempty" json:"refresh_token,omitempty"`
	ExpiresUnixMS int64             `yaml:"expires_unix_ms,omitempty" json:"expires_unix_ms,omitempty"`
	AccountID     string            `yaml:"account_id,omitempty" json:"account_id,omitempty"`
	Extra         map[string]string `yaml:"extra,omitempty" json:"extra,omitempty"`
}

// ValidateCredential checks shape without exposing values.
func ValidateCredential(credential Credential) error {
	for _, value := range []string{credential.APIKey, credential.AccessToken, credential.RefreshToken, credential.AccountID} {
		if len(value) > 256<<10 || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: credential value is invalid", ErrInvalidConfig)
		}
	}
	if len(credential.Extra) > 16 {
		return fmt.Errorf("%w: credential metadata is too large", ErrInvalidConfig)
	}
	for key, value := range credential.Extra {
		if key == "" || len(key) > 64 || len(value) > 4096 || strings.ContainsAny(key+value, "\r\n") {
			return fmt.Errorf("%w: credential metadata is invalid", ErrInvalidConfig)
		}
	}
	switch credential.Kind {
	case CredentialAPIKey:
		if credential.APIKey == "" || credential.AccessToken != "" || credential.RefreshToken != "" || credential.ExpiresUnixMS != 0 {
			return fmt.Errorf("%w: API-key record is invalid", ErrInvalidConfig)
		}
	case CredentialOAuth:
		if credential.AccessToken == "" || credential.APIKey != "" || credential.ExpiresUnixMS < 0 {
			return fmt.Errorf("%w: OAuth record is invalid", ErrInvalidConfig)
		}
	default:
		return fmt.Errorf("%w: unknown credential kind", ErrInvalidConfig)
	}
	return nil
}

// CredentialStore serializes provider record refresh across processes.
type CredentialStore interface {
	Resolve(context.Context, string, string) (Credential, error)
	Modify(context.Context, string, func(*Credential) (*Credential, error)) (Credential, error)
	Delete(context.Context, string) error
	List(context.Context) ([]AccountInfo, error)
}

// AccountInfo is the secret-free account listing.
type AccountInfo struct {
	Provider string
	Kind     CredentialKind
	Source   string
}

// AuthMethod is one provider-owned login path.
type AuthMethod struct {
	ID    string
	Label string
}

// AuthPromptType controls TUI input presentation.
type AuthPromptType string

const (
	// AuthPromptSecret requests masked local input from an authentication interaction.
	AuthPromptSecret AuthPromptType = "secret"
	// AuthPromptSelect requests one choice from provider-owned options.
	AuthPromptSelect AuthPromptType = "select"
	// AuthPromptManual requests unmasked provider-specific manual input.
	AuthPromptManual AuthPromptType = "manual"
)

// AuthOption is one selection prompt choice.
type AuthOption struct {
	ID    string
	Label string
}

// AuthPrompt is a provider-owned request for local user input.
type AuthPrompt struct {
	Type        AuthPromptType
	Message     string
	Placeholder string
	Options     []AuthOption
}

// AuthNotice reports progress, a browser URL, or a device code.
type AuthNotice struct {
	Kind            string
	Message         string
	URL             string
	UserCode        string
	VerificationURL string
}

// AuthInteraction is implemented by a UI, never by a provider.
type AuthInteraction interface {
	Prompt(context.Context, AuthPrompt) (string, error)
	Notify(AuthNotice)
}

// ModelInfo is a provider-owned catalog entry.
type ModelInfo struct {
	Provider      string
	ID            string
	Name          string
	ContextWindow int
	Vision        bool
	Tools         bool
}

// Request is the provider-neutral frozen model envelope.
type Request struct {
	SessionID string
	Purpose   string
	System    string
	Surface   []session.SurfaceNode
	Tools     []session.ToolDefinition
	MaxTokens int
}

// Completion is the final provider response assembled from its stream.
type Completion struct {
	Message session.Message
	Calls   []session.ToolCall
	Usage   *session.TokenUsage
	Stop    string
}

// Emit receives provider-neutral chunks in provider order.
type Emit func(session.AssistantChunk) error

// PreparedModel captures all provider settings before credential I/O.
type PreparedModel interface {
	Info() ModelInfo
	CredentialEnv() string
	Refresh(context.Context, Credential) (Credential, error)
	Stream(context.Context, Credential, Request, Emit) (Completion, error)
}

// Provider owns model catalog, wire implementation, and authentication flows.
type Provider interface {
	ID() string
	Models() []ModelInfo
	Prepare(string) (PreparedModel, error)
	AuthMethods() []AuthMethod
	Login(context.Context, string, AuthInteraction) (Credential, error)
}

// Call is one immutable prepared provider/model/account tuple.
type Call struct {
	provider   string
	prepared   PreparedModel
	credential Credential
}

// Info returns the frozen model metadata for this call.
func (call *Call) Info() ModelInfo { return call.prepared.Info() }

// Stream consumes one request through the frozen provider snapshot.
func (call *Call) Stream(ctx context.Context, request Request, emit Emit) (Completion, error) {
	if emit == nil {
		return Completion{}, ErrInvalidConfig
	}
	return call.prepared.Stream(ctx, call.credential, cloneRequest(request), emit)
}

// Runtime is the provider registry and authorization coordinator.
type Runtime struct {
	store CredentialStore
	now   func() time.Time

	mu        sync.RWMutex
	started   bool
	active    bool
	providers map[string]Provider
}

// New constructs an empty runtime over one account store.
func New(store CredentialStore) (*Runtime, error) {
	if store == nil {
		return nil, ErrInvalidConfig
	}
	return &Runtime{store: store, now: time.Now, providers: map[string]Provider{}}, nil
}

// ID returns the stable LLM-runtime plugin identity.
func (*Runtime) ID() string { return "llm" }

// Start activates provider registration until scope cleanup.
func (runtime *Runtime) Start(_ context.Context, scope *plugin.Scope) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.started {
		return ErrInvalidConfig
	}
	if err := scope.Defer(func(context.Context) error {
		runtime.mu.Lock()
		runtime.active = false
		runtime.providers = map[string]Provider{}
		runtime.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	runtime.started, runtime.active = true, true
	return nil
}

// Register publishes one provider until scope cleanup.
func (runtime *Runtime) Register(provider Provider, scope *plugin.Scope) error {
	if provider == nil || scope == nil || !validID(provider.ID()) {
		return ErrInvalidConfig
	}
	runtime.mu.Lock()
	if !runtime.active {
		runtime.mu.Unlock()
		return ErrNotRunning
	}
	if _, exists := runtime.providers[provider.ID()]; exists {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: duplicate provider %q", ErrInvalidConfig, provider.ID())
	}
	runtime.providers[provider.ID()] = provider
	runtime.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		runtime.mu.Lock()
		if runtime.providers[provider.ID()] == provider {
			delete(runtime.providers, provider.ID())
		}
		runtime.mu.Unlock()
		return nil
	}); err != nil {
		runtime.mu.Lock()
		delete(runtime.providers, provider.ID())
		runtime.mu.Unlock()
		return err
	}
	return nil
}

// PrepareCall freezes provider configuration, then resolves and refreshes one account.
func (runtime *Runtime) PrepareCall(ctx context.Context, providerID, modelID string) (*Call, error) {
	runtime.mu.RLock()
	if !runtime.active {
		runtime.mu.RUnlock()
		return nil, ErrNotRunning
	}
	provider := runtime.providers[providerID]
	runtime.mu.RUnlock()
	if provider == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}
	prepared, err := provider.Prepare(modelID)
	if err != nil {
		return nil, err
	}
	credential, err := runtime.store.Resolve(ctx, providerID, prepared.CredentialEnv())
	if err != nil {
		return nil, err
	}
	if credential.Kind == CredentialOAuth && credential.ExpiresUnixMS != 0 && credential.ExpiresUnixMS <= runtime.now().Add(time.Minute).UnixMilli() {
		credential, err = runtime.store.Modify(ctx, providerID, func(current *Credential) (*Credential, error) {
			if current == nil {
				return nil, ErrNoCredential
			}
			if current.Kind != CredentialOAuth || current.ExpiresUnixMS == 0 || current.ExpiresUnixMS > runtime.now().Add(time.Minute).UnixMilli() {
				copyCurrent := *current
				return &copyCurrent, nil
			}
			refreshed, refreshErr := prepared.Refresh(ctx, *current)
			if refreshErr != nil {
				return nil, refreshErr
			}
			return &refreshed, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return &Call{provider: providerID, prepared: prepared, credential: credential}, nil
}

// Login runs one provider-owned interaction and atomically replaces its record.
func (runtime *Runtime) Login(ctx context.Context, providerID, method string, interaction AuthInteraction) error {
	if interaction == nil {
		return ErrInvalidConfig
	}
	provider, err := runtime.provider(providerID)
	if err != nil {
		return err
	}
	credential, err := provider.Login(ctx, method, interaction)
	if err != nil {
		return err
	}
	if err := ValidateCredential(credential); err != nil {
		return err
	}
	_, err = runtime.store.Modify(ctx, providerID, func(*Credential) (*Credential, error) { return &credential, nil })
	return err
}

// Logout deletes one provider account.
func (runtime *Runtime) Logout(ctx context.Context, providerID string) error {
	if _, err := runtime.provider(providerID); err != nil {
		return err
	}
	return runtime.store.Delete(ctx, providerID)
}

// Providers returns provider IDs in lexical order.
func (runtime *Runtime) Providers() ([]string, error) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if !runtime.active {
		return nil, ErrNotRunning
	}
	ids := make([]string, 0, len(runtime.providers))
	for id := range runtime.providers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// Models returns a detached provider catalog.
func (runtime *Runtime) Models(providerID string) ([]ModelInfo, error) {
	provider, err := runtime.provider(providerID)
	if err != nil {
		return nil, err
	}
	return slices.Clone(provider.Models()), nil
}

// AuthMethods returns one provider's supported login paths.
func (runtime *Runtime) AuthMethods(providerID string) ([]AuthMethod, error) {
	provider, err := runtime.provider(providerID)
	if err != nil {
		return nil, err
	}
	return slices.Clone(provider.AuthMethods()), nil
}

// Accounts returns a secret-free store listing.
func (runtime *Runtime) Accounts(ctx context.Context) ([]AccountInfo, error) {
	return runtime.store.List(ctx)
}

func (runtime *Runtime) provider(id string) (Provider, error) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if !runtime.active {
		return nil, ErrNotRunning
	}
	provider := runtime.providers[id]
	if provider == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}
	return provider, nil
}

func validID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func cloneRequest(request Request) Request {
	request.Surface = slices.Clone(request.Surface)
	for index := range request.Surface {
		node := &request.Surface[index]
		if node.Message != nil {
			message := *node.Message
			message.Content = slices.Clone(message.Content)
			for block := range message.Content {
				if message.Content[block].Image != nil {
					image := *message.Content[block].Image
					message.Content[block].Image = &image
				}
			}
			node.Message = &message
		}
		if node.Call != nil {
			call := *node.Call
			call.Arguments = slices.Clone(call.Arguments)
			node.Call = &call
		}
		if node.Result != nil {
			result := *node.Result
			node.Result = &result
		}
	}
	request.Tools = slices.Clone(request.Tools)
	for index := range request.Tools {
		request.Tools[index].Parameters = slices.Clone(request.Tools[index].Parameters)
	}
	return request
}

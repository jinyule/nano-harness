// Package provider implements the OpenAI, Anthropic, and OpenRouter provider family.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const defaultRequestTimeout = 2 * time.Minute

// Config supplies transports and OAuth endpoints; production defaults are applied per field.
type Config struct {
	ID                string
	HTTPClient        *http.Client
	CodexHome         string
	ChatGPTBaseURL    string
	OpenAIAuthURL     string
	AnthropicAuthURL  string
	OpenRouterAuthURL string
}

type snapshot struct {
	baseURL   string
	apiKeyEnv string
	models    []llm.ModelInfo
}

type settingsSource interface {
	Snapshot() (appsettings.Document, uint64, error)
	Watch(func(appsettings.Document)) (func(), error)
}

// Provider owns one provider catalog, account flow, and wire implementation.
type Provider struct {
	id       string
	runtime  *llm.Runtime
	settings settingsSource
	client   *http.Client
	auth     authConfig
	current  atomic.Pointer[snapshot]
}

// New constructs one of the installed providers.
func New(runtime *llm.Runtime, settings *appsettings.Service, config Config) (*Provider, error) {
	if runtime == nil || settings == nil || config.ID != "openai" && config.ID != "anthropic" && config.ID != "openrouter" {
		return nil, llm.ErrInvalidConfig
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	auth, err := resolveAuthConfig(config)
	if err != nil {
		return nil, err
	}
	return &Provider{id: config.ID, runtime: runtime, settings: settings, client: client, auth: auth}, nil
}

// ID is both the plugin identity and provider route key.
func (provider *Provider) ID() string { return provider.id }

// Start publishes the initial catalog and follows validated settings commits.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	document, _, err := provider.settings.Snapshot()
	if err != nil {
		return err
	}
	provider.install(document)
	if err := provider.runtime.Register(provider, scope); err != nil {
		return err
	}
	dispose, err := provider.settings.Watch(provider.install)
	if err != nil {
		return err
	}
	return scope.Defer(func(context.Context) error {
		dispose()
		provider.current.Store(nil)
		return nil
	})
}

func (provider *Provider) install(document appsettings.Document) {
	configured := document.Providers[provider.id]
	models := make([]llm.ModelInfo, len(configured.Models))
	for index, model := range configured.Models {
		models[index] = llm.ModelInfo{
			Provider: provider.id, ID: model.ID, Name: model.Name,
			ContextWindow: model.ContextWindow, Vision: model.Vision, Tools: model.Tools,
		}
	}
	provider.current.Store(&snapshot{baseURL: strings.TrimRight(configured.BaseURL, "/"), apiKeyEnv: configured.APIKeyEnv, models: models})
}

// Models returns a detached current provider catalog.
func (provider *Provider) Models() []llm.ModelInfo {
	current := provider.current.Load()
	if current == nil {
		return nil
	}
	return slices.Clone(current.models)
}

// Prepare freezes model and endpoint configuration synchronously.
func (provider *Provider) Prepare(modelID string) (llm.PreparedModel, error) {
	current := provider.current.Load()
	if current == nil {
		return nil, llm.ErrNotRunning
	}
	for _, model := range current.models {
		if model.ID == modelID {
			return &prepared{owner: provider, snapshot: current, info: model}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", llm.ErrUnknownModel, provider.id, modelID)
}

// AuthMethods advertises only flows this provider can execute.
func (provider *Provider) AuthMethods() []llm.AuthMethod {
	methods := []llm.AuthMethod{{ID: "api-key", Label: "API key"}}
	switch provider.id {
	case "openai":
		methods = append(methods,
			llm.AuthMethod{ID: "oauth-browser", Label: "ChatGPT browser login"},
			llm.AuthMethod{ID: "oauth-device", Label: "ChatGPT device code"},
			llm.AuthMethod{ID: "codex-import", Label: "Import Codex login"},
		)
	case "anthropic", "openrouter":
		methods = append(methods, llm.AuthMethod{ID: "oauth-browser", Label: "Browser login"})
	}
	return methods
}

// Login executes one provider-owned login interaction.
func (provider *Provider) Login(ctx context.Context, method string, interaction llm.AuthInteraction) (llm.Credential, error) {
	if method == "api-key" {
		value, err := interaction.Prompt(ctx, llm.AuthPrompt{Type: llm.AuthPromptSecret, Message: "Enter " + provider.id + " API key"})
		if err != nil {
			return llm.Credential{}, err
		}
		credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: strings.TrimSpace(value)}
		return credential, llm.ValidateCredential(credential)
	}
	switch {
	case provider.id == "openai" && method == "oauth-browser":
		return provider.loginOpenAIBrowser(ctx, interaction)
	case provider.id == "openai" && method == "oauth-device":
		return provider.loginOpenAIDevice(ctx, interaction)
	case provider.id == "openai" && method == "codex-import":
		return provider.importCodex()
	case provider.id == "anthropic" && method == "oauth-browser":
		return provider.loginAnthropic(ctx, interaction)
	case provider.id == "openrouter" && method == "oauth-browser":
		return provider.loginOpenRouter(ctx, interaction)
	default:
		return llm.Credential{}, fmt.Errorf("%w: unsupported %s login %q", llm.ErrInvalidConfig, provider.id, method)
	}
}

type prepared struct {
	owner    *Provider
	snapshot *snapshot
	info     llm.ModelInfo
}

func (prepared *prepared) Info() llm.ModelInfo { return prepared.info }

func (prepared *prepared) CredentialEnv() string { return prepared.snapshot.apiKeyEnv }

func (prepared *prepared) Refresh(ctx context.Context, credential llm.Credential) (llm.Credential, error) {
	if credential.Kind != llm.CredentialOAuth {
		return credential, nil
	}
	switch prepared.owner.id {
	case "openai":
		return prepared.owner.refreshOpenAI(ctx, credential)
	case "anthropic":
		return prepared.owner.refreshAnthropic(ctx, credential)
	case "openrouter":
		return credential, nil
	default:
		return llm.Credential{}, llm.ErrUnknownProvider
	}
}

func (prepared *prepared) Stream(ctx context.Context, credential llm.Credential, request llm.Request, emit llm.Emit) (llm.Completion, error) {
	if request.MaxTokens < 0 || !prepared.info.Vision && surfaceHasImage(request.Surface) || !prepared.info.Tools && len(request.Tools) != 0 {
		return llm.Completion{}, &llm.Error{Code: llm.ErrorInvalid, Provider: prepared.owner.id}
	}
	switch prepared.owner.id {
	case "openai":
		return prepared.owner.streamResponses(ctx, prepared.snapshot, prepared.info, credential, request, emit)
	case "anthropic":
		return prepared.owner.streamAnthropic(ctx, prepared.snapshot, prepared.info, credential, request, emit)
	case "openrouter":
		return prepared.owner.streamOpenRouter(ctx, prepared.snapshot, prepared.info, credential, request, emit)
	default:
		return llm.Completion{}, llm.ErrUnknownProvider
	}
}

func surfaceHasImage(surface []session.SurfaceNode) bool {
	for _, node := range surface {
		if node.Message != nil && slices.ContainsFunc(node.Message.Content, func(block session.ContentBlock) bool { return block.Type == session.ContentImage }) {
			return true
		}
	}
	return false
}

func statusError(provider string, status int, retryAfter string) error {
	code := llm.ErrorInvalid
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = llm.ErrorUnauthorized
	case status == http.StatusTooManyRequests:
		code = llm.ErrorRateLimit
	case status >= http.StatusInternalServerError:
		code = llm.ErrorServer
	}
	retryMS := parseRetryAfter(retryAfter)
	return &llm.Error{Code: code, Provider: provider, HTTPStatus: status, RetryAfterMS: retryMS}
}

func transportError(provider string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &llm.Error{Code: llm.ErrorTimeout, Provider: provider, Cause: err}
	}
	return &llm.Error{Code: llm.ErrorTransport, Provider: provider, Cause: err}
}

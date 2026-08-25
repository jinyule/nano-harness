package provider

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinyule/nano-harness/internal/app/llm"
)

const (
	defaultChatGPTBaseURL       = "https://chatgpt.com"
	defaultOpenAIAuthURL        = "https://auth.openai.com"
	defaultAnthropicAuthURL     = "https://claude.ai"
	defaultAnthropicExchangeURL = "https://platform.claude.com"
	defaultOpenRouterAuthURL    = "https://openrouter.ai"
	openAICodexClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	anthropicOAuthClientID      = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	maxOAuthResponseBytes       = 1 << 20
	maxProviderRequestBytes     = 16 << 20
	maxProviderResponseBytes    = 16 << 20
	maxProviderSSELineBytes     = 2 << 20
	maxProviderToolCalls        = 64
)

var (
	userHomeDir = os.UserHomeDir
	absPath     = filepath.Abs
)

type authConfig struct {
	codexHome            string
	chatGPTBaseURL       string
	openAIAuthURL        string
	anthropicAuthURL     string
	anthropicExchangeURL string
	openRouterAuthURL    string
}

func resolveAuthConfig(config Config) (authConfig, error) {
	home := config.CodexHome
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := userHomeDir()
		if err != nil {
			return authConfig{}, fmt.Errorf("%w: resolve Codex home", llm.ErrInvalidConfig)
		}
		home = filepath.Join(userHome, ".codex")
	}
	absolute, err := absPath(home)
	if err != nil {
		return authConfig{}, fmt.Errorf("%w: resolve Codex home", llm.ErrInvalidConfig)
	}
	resolved := authConfig{
		codexHome:            absolute,
		chatGPTBaseURL:       defaultString(config.ChatGPTBaseURL, defaultChatGPTBaseURL),
		openAIAuthURL:        defaultString(config.OpenAIAuthURL, defaultOpenAIAuthURL),
		anthropicAuthURL:     defaultString(config.AnthropicAuthURL, defaultAnthropicAuthURL),
		anthropicExchangeURL: defaultAnthropicExchangeURL,
		openRouterAuthURL:    defaultString(config.OpenRouterAuthURL, defaultOpenRouterAuthURL),
	}
	if config.AnthropicAuthURL != "" {
		resolved.anthropicExchangeURL = config.AnthropicAuthURL
	}
	for name, value := range map[string]string{
		"ChatGPT":          resolved.chatGPTBaseURL,
		"OpenAI OAuth":     resolved.openAIAuthURL,
		"Anthropic OAuth":  resolved.anthropicAuthURL,
		"Anthropic token":  resolved.anthropicExchangeURL,
		"OpenRouter OAuth": resolved.openRouterAuthURL,
	} {
		if err := validateEndpoint(value); err != nil {
			return authConfig{}, fmt.Errorf("%w: %s endpoint: %w", llm.ErrInvalidConfig, name, err)
		}
	}
	return resolved, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return strings.TrimRight(value, "/")
}

func validateEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid URL")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()).IsLoopback()) {
		return nil
	}
	return errors.New("must use HTTPS or loopback HTTP")
}

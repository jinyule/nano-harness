package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/jinyule/nano-harness/internal/app/llm"
)

func (provider *Provider) loginAnthropic(ctx context.Context, interaction llm.AuthInteraction) (llm.Credential, error) {
	verifier, err := randomURLToken(32)
	if err != nil {
		return llm.Credential{}, err
	}
	state, err := randomURLToken(24)
	if err != nil {
		return llm.Credential{}, err
	}
	redirect, callbacks, shutdown, err := callbackServer(ctx, "127.0.0.1:53692", "/callback")
	if err != nil {
		return llm.Credential{}, err
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	values := url.Values{
		"code": {"true"}, "client_id": {anthropicOAuthClientID}, "response_type": {"code"},
		"redirect_uri": {redirect}, "scope": {"org:create_api_key user:profile user:inference"},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}, "state": {state},
	}
	authorizeURL := provider.auth.anthropicAuthURL + "/oauth/authorize?" + values.Encode()
	interaction.Notify(llm.AuthNotice{Kind: "browser", Message: "Open this URL to authorize Anthropic", URL: authorizeURL})
	code, err := awaitCallback(ctx, callbacks, state)
	if err != nil {
		return llm.Credential{}, err
	}
	data, err := provider.postJSON(ctx, provider.auth.anthropicExchangeURL+"/v1/oauth/token", map[string]string{
		"grant_type": "authorization_code", "client_id": anthropicOAuthClientID, "code": code,
		"state": state, "redirect_uri": redirect, "code_verifier": verifier,
	}, nil)
	if err != nil {
		return llm.Credential{}, err
	}
	var token oauthToken
	if err := json.Unmarshal(data, &token); err != nil {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	return credentialFromToken(token, "", map[string]string{"endpoint": "anthropic-oauth"})
}

func (provider *Provider) refreshAnthropic(ctx context.Context, credential llm.Credential) (llm.Credential, error) {
	if credential.RefreshToken == "" {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorUnauthorized, Provider: provider.id, Cause: errors.New("OAuth grant has no refresh token")}
	}
	data, err := provider.postJSON(ctx, provider.auth.anthropicExchangeURL+"/v1/oauth/token", map[string]string{
		"grant_type": "refresh_token", "client_id": anthropicOAuthClientID, "refresh_token": credential.RefreshToken,
	}, nil)
	if err != nil {
		return llm.Credential{}, err
	}
	var token oauthToken
	if err := json.Unmarshal(data, &token); err != nil {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	if token.RefreshToken == "" {
		token.RefreshToken = credential.RefreshToken
	}
	return credentialFromToken(token, credential.AccountID, credential.Extra)
}

func (provider *Provider) loginOpenRouter(ctx context.Context, interaction llm.AuthInteraction) (llm.Credential, error) {
	verifier, err := randomURLToken(32)
	if err != nil {
		return llm.Credential{}, err
	}
	state, err := randomURLToken(24)
	if err != nil {
		return llm.Credential{}, err
	}
	redirect, callbacks, shutdown, err := callbackServer(ctx, "127.0.0.1:0", "/callback")
	if err != nil {
		return llm.Credential{}, err
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	values := url.Values{
		"callback_url": {redirect}, "code_challenge": {challenge(verifier)},
		"code_challenge_method": {"S256"}, "state": {state},
	}
	authorizeURL := provider.auth.openRouterAuthURL + "/auth?" + values.Encode()
	interaction.Notify(llm.AuthNotice{Kind: "browser", Message: "Open this URL to authorize OpenRouter", URL: authorizeURL})
	code, err := awaitCallback(ctx, callbacks, state)
	if err != nil {
		return llm.Credential{}, err
	}
	data, err := provider.postJSON(ctx, provider.auth.openRouterAuthURL+"/api/v1/auth/keys", map[string]string{
		"code": code, "code_verifier": verifier, "code_challenge_method": "S256",
	}, nil)
	if err != nil {
		return llm.Credential{}, err
	}
	var response struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &response); err != nil || strings.TrimSpace(response.Key) == "" {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("OpenRouter token response omitted key")}
	}
	credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: response.Key, Extra: map[string]string{"source": "openrouter-oauth"}}
	return credential, llm.ValidateCredential(credential)
}

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
)

const openAIScope = "openid profile email offline_access"

var (
	devicePollFloor = 5 * time.Second
	openAuthFile    = os.Open
)

func (provider *Provider) loginOpenAIBrowser(ctx context.Context, interaction llm.AuthInteraction) (llm.Credential, error) {
	verifier, err := randomURLToken(32)
	if err != nil {
		return llm.Credential{}, err
	}
	state, err := randomURLToken(24)
	if err != nil {
		return llm.Credential{}, err
	}
	redirect, callbacks, shutdown, err := callbackServer(ctx, "127.0.0.1:1455", "/auth/callback")
	if err != nil {
		return llm.Credential{}, err
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	values := url.Values{
		"response_type": {"code"}, "client_id": {openAICodexClientID}, "redirect_uri": {redirect},
		"scope": {openAIScope}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"state": {state}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"},
	}
	authorizeURL := provider.auth.openAIAuthURL + "/oauth/authorize?" + values.Encode()
	interaction.Notify(llm.AuthNotice{Kind: "browser", Message: "Open this URL to authorize ChatGPT", URL: authorizeURL})
	code, err := awaitCallback(ctx, callbacks, state)
	if err != nil {
		return llm.Credential{}, err
	}
	return provider.exchangeOpenAI(ctx, code, verifier, redirect)
}

func (provider *Provider) loginOpenAIDevice(ctx context.Context, interaction llm.AuthInteraction) (llm.Credential, error) {
	data, err := provider.postJSON(ctx, provider.auth.openAIAuthURL+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": openAICodexClientID}, nil)
	if err != nil {
		return llm.Credential{}, err
	}
	var started struct {
		DeviceAuthID    string `json:"device_auth_id"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_uri"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(data, &started); err != nil || started.DeviceAuthID == "" || started.UserCode == "" {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("invalid device authorization response")}
	}
	if started.VerificationURL == "" {
		started.VerificationURL = provider.auth.openAIAuthURL + "/codex/device"
	}
	interaction.Notify(llm.AuthNotice{Kind: "device", Message: "Authorize this device", UserCode: started.UserCode, VerificationURL: started.VerificationURL, URL: started.VerificationURL})
	interval := time.Duration(started.Interval) * time.Second
	if interval < time.Second {
		interval = devicePollFloor
	}
	for {
		if err := waitContext(ctx, interval); err != nil {
			return llm.Credential{}, err
		}
		data, err = provider.postJSON(ctx, provider.auth.openAIAuthURL+"/api/accounts/deviceauth/token", map[string]string{
			"device_auth_id": started.DeviceAuthID, "user_code": started.UserCode,
		}, nil)
		var failure *llm.Error
		if errors.As(err, &failure) && (failure.HTTPStatus == http.StatusBadRequest || failure.HTTPStatus == http.StatusNotFound) {
			continue
		}
		if err != nil {
			return llm.Credential{}, err
		}
		var authorized struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if err := json.Unmarshal(data, &authorized); err != nil || authorized.AuthorizationCode == "" || authorized.CodeVerifier == "" {
			return llm.Credential{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("invalid device token response")}
		}
		return provider.exchangeOpenAI(ctx, authorized.AuthorizationCode, authorized.CodeVerifier, "https://auth.openai.com/deviceauth/callback")
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (provider *Provider) exchangeOpenAI(ctx context.Context, code, verifier, redirect string) (llm.Credential, error) {
	data, err := provider.postForm(ctx, provider.auth.openAIAuthURL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "client_id": {openAICodexClientID}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {redirect},
	}, nil)
	if err != nil {
		return llm.Credential{}, err
	}
	var token oauthToken
	if err := json.Unmarshal(data, &token); err != nil {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	accountID := jwtStringClaim(token.AccessToken, "https://api.openai.com/auth.chatgpt_account_id")
	if accountID == "" {
		accountID = jwtStringClaim(token.AccessToken, "chatgpt_account_id")
	}
	return credentialFromToken(token, accountID, map[string]string{"endpoint": "chatgpt"})
}

func (provider *Provider) refreshOpenAI(ctx context.Context, credential llm.Credential) (llm.Credential, error) {
	if credential.RefreshToken == "" {
		return llm.Credential{}, &llm.Error{Code: llm.ErrorUnauthorized, Provider: provider.id, Cause: errors.New("OAuth grant has no refresh token")}
	}
	data, err := provider.postForm(ctx, provider.auth.openAIAuthURL+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "client_id": {openAICodexClientID}, "refresh_token": {credential.RefreshToken},
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
	accountID := jwtStringClaim(token.AccessToken, "https://api.openai.com/auth.chatgpt_account_id")
	if accountID == "" {
		accountID = credential.AccountID
	}
	return credentialFromToken(token, accountID, credential.Extra)
}

func jwtStringClaim(token, name string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	value, _ := claims[name].(string)
	return value
}

func (provider *Provider) importCodex() (llm.Credential, error) {
	path := filepath.Join(provider.auth.codexHome, "auth.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxOAuthResponseBytes || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return llm.Credential{}, fmt.Errorf("%w: Codex auth cache is absent or unsafe", llm.ErrNoCredential)
	}
	file, err := openAuthFile(path)
	if err != nil {
		return llm.Credential{}, fmt.Errorf("%w: open Codex auth cache", llm.ErrNoCredential)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, maxOAuthResponseBytes+1))
	decoder.DisallowUnknownFields()
	var document struct {
		OpenAIAPIKey json.RawMessage `json:"OPENAI_API_KEY,omitempty"`
		AuthMode     string          `json:"auth_mode"`
		Tokens       struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
			IDToken      string `json:"id_token,omitempty"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh,omitempty"`
	}
	if err := decoder.Decode(&document); err != nil || document.AuthMode != "chatgpt" || document.Tokens.AccessToken == "" || document.Tokens.AccountID == "" {
		return llm.Credential{}, fmt.Errorf("%w: invalid Codex ChatGPT cache", llm.ErrNoCredential)
	}
	expires := jwtNumericClaim(document.Tokens.AccessToken, "exp") * 1000
	credential := llm.Credential{
		Kind: llm.CredentialOAuth, AccessToken: document.Tokens.AccessToken,
		RefreshToken: document.Tokens.RefreshToken, AccountID: document.Tokens.AccountID,
		ExpiresUnixMS: expires, Extra: map[string]string{"endpoint": "chatgpt", "source": "codex-import"},
	}
	return credential, llm.ValidateCredential(credential)
}

func jwtNumericClaim(token, name string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if decoder.Decode(&claims) != nil {
		return 0
	}
	number, ok := claims[name].(json.Number)
	if !ok {
		return 0
	}
	value, _ := number.Int64()
	return value
}

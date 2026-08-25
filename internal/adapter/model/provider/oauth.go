package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
)

var (
	randomRead     = rand.Read
	callbackListen = net.Listen
)

type oauthCallback struct {
	code  string
	state string
	err   error
}

func randomURLToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := randomRead(data); err != nil {
		return "", fmt.Errorf("generate OAuth secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func challenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func callbackServer(_ context.Context, address, path string) (string, <-chan oauthCallback, func(context.Context) error, error) {
	listener, err := callbackListen("tcp", address)
	if err != nil {
		return "", nil, nil, fmt.Errorf("start OAuth callback listener: %w", err)
	}
	results := make(chan oauthCallback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := request.URL.Query()
		result := oauthCallback{code: query.Get("code"), state: query.Get("state")}
		if message := query.Get("error"); message != "" {
			result.err = errors.New("authorization was rejected")
		} else if result.code == "" {
			result.err = errors.New("OAuth callback omitted code")
		}
		select {
		case results <- result:
		default:
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(writer, "Authorization received. You can close this window and return to nano-harness.\n")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	host := listener.Addr().String()
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		host = net.JoinHostPort("localhost", fmt.Sprint(tcp.Port))
	}
	shutdown := func(shutdownContext context.Context) error {
		err := server.Shutdown(shutdownContext)
		<-done
		return err
	}
	return "http://" + host + path, results, shutdown, nil
}

func awaitCallback(ctx context.Context, results <-chan oauthCallback, expectedState string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-results:
		if result.err != nil {
			return "", result.err
		}
		if result.state != expectedState {
			return "", errors.New("OAuth callback state did not match")
		}
		return result.code, nil
	}
}

func (provider *Provider) postForm(ctx context.Context, endpoint string, values url.Values, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return provider.doOAuth(request)
}

func (provider *Provider) postJSON(ctx context.Context, endpoint string, payload any, headers map[string]string) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return provider.doOAuth(request)
}

func (provider *Provider) doOAuth(request *http.Request) ([]byte, error) {
	response, err := provider.client.Do(request)
	if err != nil {
		return nil, transportError(provider.id, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, statusError(provider.id, response.StatusCode, response.Header.Get("Retry-After"))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return nil, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	if len(data) > maxOAuthResponseBytes {
		return nil, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("OAuth response exceeds size limit")}
	}
	return data, nil
}

type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func credentialFromToken(token oauthToken, accountID string, extra map[string]string) (llm.Credential, error) {
	credential := llm.Credential{
		Kind: llm.CredentialOAuth, AccessToken: token.AccessToken,
		RefreshToken: token.RefreshToken, AccountID: accountID, Extra: extra,
	}
	if token.ExpiresIn > 0 {
		credential.ExpiresUnixMS = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UnixMilli()
	}
	return credential, llm.ValidateCredential(credential)
}

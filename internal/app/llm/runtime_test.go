package llm

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type fakeStore struct {
	credential    Credential
	resolveErr    error
	modifyErr     error
	modifyCurrent *Credential
	currentNil    bool
	deleted       string
	accounts      []AccountInfo
}

func (store *fakeStore) Resolve(context.Context, string, string) (Credential, error) {
	return store.credential, store.resolveErr
}
func (store *fakeStore) Modify(_ context.Context, _ string, mutate func(*Credential) (*Credential, error)) (Credential, error) {
	if store.modifyErr != nil {
		return Credential{}, store.modifyErr
	}
	var current *Credential
	if !store.currentNil {
		current = &store.credential
		if store.modifyCurrent != nil {
			current = store.modifyCurrent
		}
	}
	next, err := mutate(current)
	if err != nil {
		return Credential{}, err
	}
	if next == nil {
		store.credential = Credential{}
	} else {
		store.credential = *next
	}
	return store.credential, nil
}
func (store *fakeStore) Delete(_ context.Context, provider string) error {
	store.deleted = provider
	return store.modifyErr
}
func (store *fakeStore) List(context.Context) ([]AccountInfo, error) {
	return store.accounts, store.resolveErr
}

type fakeProvider struct {
	id         string
	models     []ModelInfo
	prepared   *fakePrepared
	prepareErr error
	login      Credential
	loginErr   error
}

func (provider *fakeProvider) ID() string          { return provider.id }
func (provider *fakeProvider) Models() []ModelInfo { return provider.models }
func (provider *fakeProvider) Prepare(string) (PreparedModel, error) {
	return provider.prepared, provider.prepareErr
}
func (provider *fakeProvider) AuthMethods() []AuthMethod {
	return []AuthMethod{{ID: "api-key", Label: "key"}}
}
func (provider *fakeProvider) Login(context.Context, string, AuthInteraction) (Credential, error) {
	return provider.login, provider.loginErr
}

type fakePrepared struct {
	info        ModelInfo
	environment string
	refreshed   Credential
	refreshErr  error
	completion  Completion
	streamErr   error
	request     Request
}

func (prepared *fakePrepared) Info() ModelInfo       { return prepared.info }
func (prepared *fakePrepared) CredentialEnv() string { return prepared.environment }
func (prepared *fakePrepared) Refresh(context.Context, Credential) (Credential, error) {
	return prepared.refreshed, prepared.refreshErr
}
func (prepared *fakePrepared) Stream(_ context.Context, _ Credential, request Request, emit Emit) (Completion, error) {
	prepared.request = request
	if len(request.Surface) > 0 && request.Surface[0].Message != nil {
		request.Surface[0].Message.Content[0].Text = "mutated"
	}
	if len(request.Tools) > 0 {
		request.Tools[0].Parameters[0] = '['
	}
	_ = emit(session.AssistantChunk{Kind: session.ChunkText, Text: "x"})
	return prepared.completion, prepared.streamErr
}

type fakeInteraction struct{}

func (fakeInteraction) Prompt(context.Context, AuthPrompt) (string, error) { return "", nil }
func (fakeInteraction) Notify(AuthNotice)                                  {}

func activeRuntime(t *testing.T, store *fakeStore) (*Runtime, *plugin.Scope) {
	t.Helper()
	runtime, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if runtime.ID() != "llm" || runtime.Start(context.Background(), scope) != nil {
		t.Fatal("start runtime")
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return runtime, scope
}

func TestCredentialAndError(t *testing.T) {
	cause := errors.New("cause")
	for _, failure := range []*Error{{Code: ErrorServer, Provider: "p", Cause: cause}, {Code: ErrorInvalid, Provider: "p", HTTPStatus: 400}} {
		if failure.Error() == "" {
			t.Fatal("empty error")
		}
	}
	if !errors.Is(&Error{Code: ErrorServer, Provider: "p", Cause: cause}, cause) {
		t.Fatal("unwrap lost")
	}
	valid := []Credential{
		{Kind: CredentialAPIKey, APIKey: "key", Extra: map[string]string{"source": "test"}},
		{Kind: CredentialOAuth, AccessToken: "access", RefreshToken: "refresh", ExpiresUnixMS: 1, AccountID: "account"},
	}
	for _, credential := range valid {
		if err := ValidateCredential(credential); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []Credential{
		{},
		{Kind: CredentialAPIKey},
		{Kind: CredentialAPIKey, APIKey: "key\n"},
		{Kind: CredentialAPIKey, APIKey: "key", AccessToken: "bad"},
		{Kind: CredentialOAuth},
		{Kind: CredentialOAuth, AccessToken: "a", APIKey: "bad"},
		{Kind: CredentialOAuth, AccessToken: "a", ExpiresUnixMS: -1},
		{Kind: CredentialAPIKey, APIKey: "a", Extra: map[string]string{"bad\n": "x"}},
	}
	extra := map[string]string{}
	for index := range 17 {
		extra[string(rune('a'+index))] = "x"
	}
	invalid = append(invalid, Credential{Kind: CredentialAPIKey, APIKey: "a", Extra: extra})
	for _, credential := range invalid {
		if ValidateCredential(credential) == nil {
			t.Fatalf("accepted %#v", credential)
		}
	}
}

func TestRuntimeRouteLoginAndCleanup(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil store accepted")
	}
	store := &fakeStore{credential: Credential{Kind: CredentialAPIKey, APIKey: "key"}, accounts: []AccountInfo{{Provider: "p"}}}
	closedStart := &plugin.Scope{}
	_ = closedStart.Close(context.Background())
	closedRuntime, _ := New(store)
	if err := closedRuntime.Start(context.Background(), closedStart); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed start=%v", err)
	}
	inactive, _ := New(store)
	inactiveProvider := &fakeProvider{id: "inactive"}
	if err := inactive.Register(inactiveProvider, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive register=%v", err)
	}
	if _, err := inactive.PrepareCall(context.Background(), "inactive", "m"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive prepare=%v", err)
	}
	if _, err := inactive.Models("inactive"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive provider lookup=%v", err)
	}
	runtime, runtimeScope := activeRuntime(t, store)
	if runtime.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start accepted")
	}
	prepared := &fakePrepared{info: ModelInfo{Provider: "p", ID: "m"}, environment: "KEY", completion: Completion{Message: session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "p"}}}}
	provider := &fakeProvider{id: "p", models: []ModelInfo{{Provider: "p", ID: "m"}}, prepared: prepared, login: Credential{Kind: CredentialAPIKey, APIKey: "new"}}
	providerScope := &plugin.Scope{}
	if err := runtime.Register(provider, providerScope); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register(provider, &plugin.Scope{}); err == nil {
		t.Fatal("duplicate provider accepted")
	}
	if err := runtime.Register(nil, &plugin.Scope{}); err == nil {
		t.Fatal("nil provider accepted")
	}
	ids, _ := runtime.Providers()
	models, _ := runtime.Models("p")
	methods, _ := runtime.AuthMethods("p")
	accounts, _ := runtime.Accounts(context.Background())
	if !slices.Equal(ids, []string{"p"}) || len(models) != 1 || len(methods) != 1 || len(accounts) != 1 {
		t.Fatal("registry listing")
	}
	call, err := runtime.PrepareCall(context.Background(), "p", "m")
	if err != nil || call.Info().ID != "m" {
		t.Fatal(err)
	}
	original := Request{Surface: []session.SurfaceNode{{Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "u"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "hello"}}}}}, Tools: []session.ToolDefinition{{Name: "t", Description: "t", Parameters: json.RawMessage(`{}`)}}}
	if _, err := call.Stream(context.Background(), original, func(session.AssistantChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if session.Text(*original.Surface[0].Message) != "hello" || string(original.Tools[0].Parameters) != `{}` {
		t.Fatal("request was not cloned")
	}
	if _, err := call.Stream(context.Background(), Request{}, nil); err == nil {
		t.Fatal("nil emit accepted")
	}
	if err := runtime.Login(context.Background(), "p", "api-key", nil); err == nil {
		t.Fatal("nil interaction accepted")
	}
	if err := runtime.Login(context.Background(), "p", "api-key", fakeInteraction{}); err != nil || store.credential.APIKey != "new" {
		t.Fatalf("login=%v credential=%#v", err, store.credential)
	}
	if err := runtime.Logout(context.Background(), "p"); err != nil || store.deleted != "p" {
		t.Fatal("logout")
	}
	if err := runtime.Login(context.Background(), "missing", "api-key", fakeInteraction{}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("login unknown provider=%v", err)
	}
	if err := runtime.Logout(context.Background(), "missing"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("logout unknown provider=%v", err)
	}
	if _, err := runtime.AuthMethods("missing"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("methods unknown provider=%v", err)
	}
	if err := providerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Models("p"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unregistered provider=%v", err)
	}
	if err := runtimeScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Providers(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("providers after close=%v", err)
	}
}

func TestRuntimeRefreshAndFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	store := &fakeStore{credential: Credential{Kind: CredentialOAuth, AccessToken: "old", RefreshToken: "r", ExpiresUnixMS: now.UnixMilli()}}
	runtime, _ := activeRuntime(t, store)
	runtime.now = func() time.Time { return now }
	prepared := &fakePrepared{info: ModelInfo{Provider: "p", ID: "m"}, refreshed: Credential{Kind: CredentialOAuth, AccessToken: "new", RefreshToken: "r", ExpiresUnixMS: now.Add(time.Hour).UnixMilli()}}
	provider := &fakeProvider{id: "p", prepared: prepared}
	scope := &plugin.Scope{}
	if err := runtime.Register(provider, scope); err != nil {
		t.Fatal(err)
	}
	if call, err := runtime.PrepareCall(context.Background(), "p", "m"); err != nil || call.credential.AccessToken != "new" {
		t.Fatalf("call=%#v err=%v", call, err)
	}
	store.credential = Credential{Kind: CredentialOAuth, AccessToken: "old", RefreshToken: "r", ExpiresUnixMS: now.UnixMilli()}
	store.currentNil = true
	if _, err := runtime.PrepareCall(context.Background(), "p", "m"); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("refresh missing current=%v", err)
	}
	store.currentNil = false
	currentAPIKey := Credential{Kind: CredentialAPIKey, APIKey: "raced"}
	store.modifyCurrent = &currentAPIKey
	if call, err := runtime.PrepareCall(context.Background(), "p", "m"); err != nil || call.credential.APIKey != "raced" {
		t.Fatalf("refresh race call=%#v err=%v", call, err)
	}
	store.modifyCurrent = nil
	store.credential = Credential{Kind: CredentialOAuth, AccessToken: "old", RefreshToken: "r", ExpiresUnixMS: now.UnixMilli()}
	prepared.refreshErr = errors.New("refresh")
	if _, err := runtime.PrepareCall(context.Background(), "p", "m"); err == nil {
		t.Fatal("refresh error lost")
	}
	prepared.refreshErr = nil
	store.modifyErr = errors.New("modify")
	if _, err := runtime.PrepareCall(context.Background(), "p", "m"); err == nil {
		t.Fatal("refresh modify error lost")
	}
	store.modifyErr = nil
	store.resolveErr = errors.New("resolve")
	if _, err := runtime.PrepareCall(context.Background(), "p", "m"); err == nil {
		t.Fatal("resolve error lost")
	}
	store.resolveErr = nil
	provider.prepareErr = ErrUnknownModel
	if _, err := runtime.PrepareCall(context.Background(), "p", "m"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("prepare error=%v", err)
	}
	provider.prepareErr = nil
	provider.loginErr = errors.New("login")
	if err := runtime.Login(context.Background(), "p", "x", fakeInteraction{}); err == nil {
		t.Fatal("login error lost")
	}
	provider.loginErr = nil
	provider.login = Credential{}
	if err := runtime.Login(context.Background(), "p", "x", fakeInteraction{}); err == nil {
		t.Fatal("invalid login credential accepted")
	}
	provider.login = Credential{Kind: CredentialAPIKey, APIKey: "new"}
	store.modifyErr = errors.New("login store")
	if err := runtime.Login(context.Background(), "p", "x", fakeInteraction{}); err == nil {
		t.Fatal("login store error lost")
	}
	store.modifyErr = nil
	if _, err := runtime.PrepareCall(context.Background(), "missing", "m"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown provider=%v", err)
	}
	if validID("") || validID("UPPER") || !validID("good-1") {
		t.Fatal("validID")
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	provider2 := &fakeProvider{id: "q"}
	if err := runtime.Register(provider2, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed registration=%v", err)
	}
}

func TestCloneRequestDetachesImagesCallsAndResults(t *testing.T) {
	image := &session.Image{ID: "image", Name: "image.jpg"}
	request := Request{Surface: []session.SurfaceNode{
		{Message: &session.Message{Content: []session.ContentBlock{{Type: session.ContentImage, Image: image}}}},
		{Call: &session.ToolCall{ID: "call", Arguments: json.RawMessage(`{}`)}},
		{Result: &session.ToolResult{CallID: "call", Output: "ok"}},
	}}
	cloned := cloneRequest(request)
	cloned.Surface[0].Message.Content[0].Image.Name = "changed"
	cloned.Surface[1].Call.Arguments[0] = '['
	cloned.Surface[2].Result.Output = "changed"
	if request.Surface[0].Message.Content[0].Image.Name == "changed" || request.Surface[1].Call.Arguments[0] == '[' || request.Surface[2].Result.Output == "changed" {
		t.Fatal("cloneRequest aliases source")
	}
}

package settings

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

type fakeBackend struct {
	load    Document
	loadErr error
	persist func(Document) error
	watch   func(context.Context, func(Document, error)) error
}

func (backend *fakeBackend) Load(context.Context) (Document, error) {
	return backend.load, backend.loadErr
}
func (backend *fakeBackend) Persist(_ context.Context, document Document) error {
	if backend.persist != nil {
		return backend.persist(document)
	}
	return nil
}
func (backend *fakeBackend) Watch(ctx context.Context, publish func(Document, error)) error {
	if backend.watch != nil {
		return backend.watch(ctx, publish)
	}
	<-ctx.Done()
	return nil
}

func startService(t *testing.T) (*Service, *plugin.Scope) {
	t.Helper()
	service := New()
	scope := &plugin.Scope{}
	if service.ID() != "settings" || service.Start(context.Background(), scope) != nil {
		t.Fatal("start settings")
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, scope
}

func TestDocumentResolveAndValidation(t *testing.T) {
	defaults := Defaults()
	resolved, err := Resolve(Document{Route: Route{Provider: "openrouter", Model: "openai/gpt-5.4"}, Providers: map[string]Provider{
		"openrouter": {BaseURL: "http://localhost:1234", APIKeyEnv: "CUSTOM_KEY", Models: []Model{{ID: "openai/gpt-5.4", Name: "Custom", ContextWindow: 4096, Vision: true, Tools: true}}},
	}, Retry: Retry{Mode: "always", MaxRetries: 1, InitialDelayMS: 1, MaxDelayMS: 2}, Compaction: Compaction{ThresholdRatio: .7, RetainRatio: .1, MaxTokens: 256}})
	if err != nil || resolved.Route.Provider != "openrouter" || resolved.Providers["openrouter"].BaseURL != "http://localhost:1234" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	resolved.Providers["openai"] = Provider{}
	if defaults.Providers["openai"].BaseURL == "" {
		t.Fatal("Resolve aliased defaults")
	}

	mutations := []func(*Document){
		func(document *Document) { document.Route.Provider = "" },
		func(document *Document) { delete(document.Providers, "openai") },
		func(document *Document) { document.Providers["extra"] = document.Providers["openai"] },
		func(document *Document) {
			provider := document.Providers["openai"]
			provider.BaseURL = "://"
			document.Providers["openai"] = provider
		},
		func(document *Document) {
			provider := document.Providers["openai"]
			provider.BaseURL = "ftp://example.com"
			document.Providers["openai"] = provider
		},
		func(document *Document) {
			provider := document.Providers["openai"]
			provider.APIKeyEnv = "bad"
			document.Providers["openai"] = provider
		},
		func(document *Document) {
			provider := document.Providers["openai"]
			provider.Models = nil
			document.Providers["openai"] = provider
		},
		func(document *Document) {
			provider := document.Providers["openai"]
			provider.Models[0].ID = ""
			document.Providers["openai"] = provider
		},
		func(document *Document) {
			provider := document.Providers["openai"]
			provider.Models = append(provider.Models, provider.Models[0])
			document.Providers["openai"] = provider
		},
		func(document *Document) { document.Route.Model = "missing" },
		func(document *Document) { document.Retry.Mode = "bad" },
		func(document *Document) { document.Compaction.ThresholdRatio = 1 },
	}
	for index, mutate := range mutations {
		document := Defaults()
		mutate(&document)
		if err := document.Validate(); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("mutation %d error=%v", index, err)
		}
	}
	for _, value := range []string{"_OK", "A0_B"} {
		if !validEnv(value) {
			t.Errorf("validEnv(%q)=false", value)
		}
	}
	for _, value := range []string{"", "0BAD", "A-b"} {
		if validEnv(value) {
			t.Errorf("validEnv(%q)=true", value)
		}
	}
	if validName(" bad ", 10) || validName("", 10) || !validName("good", 10) {
		t.Fatal("validName behavior")
	}
	missing := Defaults()
	delete(missing.Providers, "openai")
	missing.Providers["extra"] = Defaults().Providers["openai"]
	if err := missing.Validate(); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("missing required provider=%v", err)
	}
}

func TestServiceLifecycleHotReloadAndUpdate(t *testing.T) {
	service, scope := startService(t)
	if service.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start accepted")
	}
	loaded := make(chan struct{})
	backend := &fakeBackend{watch: func(ctx context.Context, publish func(Document, error)) error {
		publish(Document{}, errors.New("bad edit"))
		publish(Document{}, nil)
		close(loaded)
		<-ctx.Done()
		return nil
	}}
	mountScope := &plugin.Scope{}
	if err := service.Mount(context.Background(), backend, mountScope); err != nil {
		t.Fatal(err)
	}
	<-loaded
	if service.LastReloadError() != nil {
		t.Fatal("valid edit did not clear last error")
	}
	var mu sync.Mutex
	seen := 0
	dispose, err := service.Watch(func(Document) {
		mu.Lock()
		seen++
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	_, revision, _ := service.Snapshot()
	backend.persist = func(document Document) error {
		if document.Route.Provider != "anthropic" {
			t.Fatal("mutation not persisted")
		}
		return nil
	}
	if err := service.Update(context.Background(), revision, func(document *Document) error {
		document.Route = Route{Provider: "anthropic", Model: "claude-sonnet-4-5"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dispose()
	mu.Lock()
	if seen != 1 {
		t.Fatalf("watch count=%d", seen)
	}
	mu.Unlock()
	if err := service.Update(context.Background(), revision, func(*Document) error { return nil }); err == nil {
		t.Fatal("stale revision accepted")
	}
	if err := service.Update(context.Background(), 0, func(*Document) error { return errors.New("mutate") }); err == nil {
		t.Fatal("mutation error lost")
	}
	if err := service.Update(context.Background(), 0, nil); err == nil {
		t.Fatal("nil mutation accepted")
	}
	if _, err := service.Watch(nil); err == nil {
		t.Fatal("nil watcher accepted")
	}
	callWatcher(func(Document) { panic("contained") }, Defaults())
	if err := service.Mount(context.Background(), backend, &plugin.Scope{}); !errors.Is(err, ErrProviderMounted) {
		t.Fatalf("duplicate mount=%v", err)
	}
	if err := mountScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Snapshot(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("snapshot after close=%v", err)
	}
	if _, err := service.Watch(func(Document) {}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("watch after close=%v", err)
	}
}

func TestServiceMountFailures(t *testing.T) {
	closedStart := &plugin.Scope{}
	_ = closedStart.Close(context.Background())
	if err := New().Start(context.Background(), closedStart); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed start scope=%v", err)
	}
	service, _ := startService(t)
	if err := service.Mount(context.Background(), nil, &plugin.Scope{}); err == nil {
		t.Fatal("nil backend accepted")
	}
	failure := errors.New("load")
	if err := service.Mount(context.Background(), &fakeBackend{loadErr: failure}, &plugin.Scope{}); !errors.Is(err, failure) {
		t.Fatalf("load error=%v", err)
	}
	if err := service.Mount(context.Background(), &fakeBackend{load: Document{Route: Route{Provider: "bad"}}}, &plugin.Scope{}); err == nil {
		t.Fatal("invalid load accepted")
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	backend := &fakeBackend{}
	if err := service.Mount(context.Background(), backend, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
	inactive, inactiveScope := startService(t)
	_ = inactiveScope.Close(context.Background())
	if err := inactive.Mount(context.Background(), &fakeBackend{}, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive mount=%v", err)
	}
	inactive.publish(Document{Retry: Retry{Mode: "bad"}}, nil)
	inactive.commit(Defaults())
	if err := inactive.Update(context.Background(), 0, func(*Document) error { return nil }); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive update=%v", err)
	}
}

func TestServiceUpdateValidationStorageAndUnmountedFailures(t *testing.T) {
	service, _ := startService(t)
	if err := service.Update(context.Background(), 0, func(*Document) error { return nil }); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("unmounted update=%v", err)
	}
	backend := &fakeBackend{}
	mountScope := &plugin.Scope{}
	if err := service.Mount(context.Background(), backend, mountScope); err != nil {
		t.Fatal(err)
	}
	if err := service.Update(context.Background(), 0, func(document *Document) error {
		document.Route.Model = "missing"
		return nil
	}); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("invalid update=%v", err)
	}
	backend.persist = func(Document) error { return errors.New("persist") }
	if err := service.Update(context.Background(), 0, func(*Document) error { return nil }); err == nil {
		t.Fatal("persist error missing")
	}
	service.publish(Document{Retry: Retry{Mode: "bad"}}, nil)
	if service.LastReloadError() == nil {
		t.Fatal("invalid hot reload was not retained")
	}
	_ = mountScope.Close(context.Background())
}

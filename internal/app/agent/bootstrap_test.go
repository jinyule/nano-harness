package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

func TestBootstrap_ValidatesCreatesPublishesAndCleansRoot(t *testing.T) {
	harness := startEngineHarness(t, 1)
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)
	for _, request := range []CreateRequest{{ParentID: "parent"}, {Depth: 1}} {
		if _, err := NewBootstrap(registry, request); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("request %+v error = %v", request, err)
		}
	}
	if _, err := NewBootstrap(nil, CreateRequest{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil registry error = %v", err)
	}
	bootstrap, err := NewBootstrap(registry, CreateRequest{SessionID: "root", Create: true})
	if err != nil || bootstrap.ID() != "root-agent" || bootstrap.request.Mode != "continuable" {
		t.Fatalf("bootstrap = %+v, error = %v", bootstrap, err)
	}
	if _, err := bootstrap.Agent(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("pre-start Agent() error = %v", err)
	}
	scope := &plugin.Scope{}
	if err := bootstrap.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	root, err := bootstrap.Agent()
	if err != nil || root.Status().SessionID != "root" {
		t.Fatalf("Agent() = %+v, %v", root, err)
	}
	if err := bootstrap.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double start error = %v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Agent(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("post-close Agent() error = %v", err)
	}
	if _, err := registry.Find("root"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("root remained mounted: %v", err)
	}
}

func TestBootstrap_ContainsCreateAndPublicationFailures(t *testing.T) {
	harness := startEngineHarness(t, 1)
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	registry, _ := startRegistry(t, harness, repository, policy)
	repository.openErr = errors.New("open")
	failed, _ := NewBootstrap(registry, CreateRequest{SessionID: "failed", Create: true})
	if err := failed.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, repository.openErr) {
		t.Fatalf("create error = %v", err)
	}

	repository.openErr = nil
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	rollback, _ := NewBootstrap(registry, CreateRequest{SessionID: "rollback", Create: true})
	if err := rollback.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("publication error = %v", err)
	}
	if _, err := registry.Find("rollback"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("rollback root remained mounted: %v", err)
	}
}

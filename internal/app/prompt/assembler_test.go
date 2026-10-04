package prompt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestAssemblerLifecycleAndSections(t *testing.T) {
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := New().Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
	assembler := New()
	if _, err := assembler.Build(Input{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("before start=%v", err)
	}
	scope := &plugin.Scope{}
	if assembler.ID() != "prompt" || assembler.Start(context.Background(), scope) != nil {
		t.Fatal("start")
	}
	if assembler.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start")
	}
	prompt, err := assembler.Build(Input{
		Workspace: "/work", Provider: "openai", Model: "model", Persona: "reviewer", Delegated: true,
		Tools: []session.ToolDefinition{{Name: "z"}, {Name: "a"}}, Guidance: []string{"first guidance", "second guidance"},
	})
	if err != nil || !strings.Contains(prompt, "Delegation:") || !strings.Contains(prompt, "reviewer") || !strings.Contains(prompt, "sandbox escalation") || !strings.Contains(prompt, "reject paths outside it") {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
	if !strings.HasSuffix(prompt, "Available tools: a, z. Follow each JSON schema exactly and use tool results as the only authority for side effects.\n\nfirst guidance\n\nsecond guidance") {
		t.Fatalf("tool sections out of order: %q", prompt)
	}
	if _, err := assembler.Build(Input{Workspace: ""}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid=%v", err)
	}
	if _, err := assembler.Build(Input{Workspace: "/w", Provider: "p", Model: "m", Persona: strings.Repeat("x", session.MaxTextBytes)}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("large=%v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := assembler.Build(Input{Workspace: "/w", Provider: "p", Model: "m"}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after close=%v", err)
	}
}

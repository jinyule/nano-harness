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
		Workspace: "/work", Provider: "openai", Model: "model", Persona: "reviewer",
		Tools: []session.ToolDefinition{{Name: "z"}, {Name: "a"}}, Guidance: []string{"first guidance", "second guidance"},
	})
	// Delegation scope is runtime context, never a child-only system section.
	if err != nil || strings.Contains(prompt, "Delegation:") || !strings.Contains(prompt, "reviewer") {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
	for _, policy := range []string{
		"read, grep, and read_image may also read absolute paths in this workspace's spill partition",
		"Exact historical spill files named in committed tool results remain readable after a spill-root change",
		"glob, write, edit, and bash workdir remain confined to the workspace",
		"bash can leave its workspace-write sandbox only with an approved danger-full-access request",
	} {
		if !strings.Contains(prompt, policy) {
			t.Errorf("Safety section lacks %q", policy)
		}
	}
	if !strings.HasSuffix(prompt, "Available tools: a, z. Follow each JSON schema exactly and use tool results as the only authority for side effects.\n\nfirst guidance\n\nsecond guidance") {
		t.Fatalf("tool sections out of order: %q", prompt)
	}
	planned, err := assembler.Build(Input{
		Workspace: "/work", Provider: "openai", Model: "model", Persona: "reviewer", PlanPolicy: "plan policy\n",
		Tools: []session.ToolDefinition{{Name: "a"}}, Guidance: []string{"guidance"},
	})
	if err != nil || !strings.HasSuffix(planned, "Assigned role:\nreviewer\n\nplan policy\n\n\nAvailable tools: a. Follow each JSON schema exactly and use tool results as the only authority for side effects.\n\nguidance") {
		t.Fatalf("plan policy is not between the role and the tool sections: %q, %v", planned, err)
	}
	if strings.Contains(prompt, "plan policy") {
		t.Fatal("plan policy leaked into a request outside plan mode")
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

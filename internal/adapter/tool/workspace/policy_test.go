package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestRoot_FullAccessAndEscalation(t *testing.T) {
	root, err := Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outside, "file")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []session.SandboxMode{session.SandboxReadOnly, session.SandboxWorkspaceWrite} {
		if _, err := root.WritableIn(path, mode); !errors.Is(err, ErrOutsideRoot) {
			t.Fatal(err)
		}
	}
	if target, err := root.WritableIn(path, session.SandboxDangerFullAccess); err != nil || target != path {
		t.Fatalf("host mutation=%s %v", target, err)
	}
	if target, err := root.WritableIn("new", session.SandboxDangerFullAccess); err != nil || target != filepath.Join(root.Path(), "new") {
		t.Fatalf("relative mutation=%s %v", target, err)
	}
	if _, target, err := root.ExistingIn(outside, session.SandboxDangerFullAccess); err != nil || target != outside {
		t.Fatalf("host directory=%s %v", target, err)
	}
	journal := &historyJournal{events: []session.Event{{Record: session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: session.SandboxDangerFullAccess}}}}}
	if _, target, err := root.ReadableFrom(t.Context(), path, appTool.Invocation{Journal: journal}); err != nil || target != path {
		t.Fatalf("host read=%s %v", target, err)
	}
	for _, standing := range []session.SandboxMode{session.SandboxReadOnly, session.SandboxWorkspaceWrite, session.SandboxDangerFullAccess} {
		if got, err := ResolveEscalation(standing, nil); err != nil || got != standing {
			t.Fatal(got, err)
		}
		for _, target := range []string{"workspace-write", "danger-full-access", "unknown"} {
			_, err := ResolveEscalation(standing, &target)
			wantError := target == "unknown" || standing == session.SandboxDangerFullAccess && target == "workspace-write"
			if (err != nil) != wantError {
				t.Fatalf("%s -> %s: %v", standing, target, err)
			}
		}
	}
}

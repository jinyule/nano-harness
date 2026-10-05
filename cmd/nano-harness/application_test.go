package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jinyule/nano-harness/internal/adapter/tui"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// This frontend exercises the shared composition without linking UI behavior to Tea.
type probeFrontend struct {
	app        *application
	events     <-chan session.Event
	controller agent.Controller
	closed     bool
}

func (*probeFrontend) ID() string { return "probe-frontend" }
func (*probeFrontend) Ask(context.Context, approval.Question) session.ApprovalOutcome {
	return session.ApprovalRejected
}
func (frontend *probeFrontend) Start(ctx context.Context, scope *plugin.Scope) error {
	controller, err := frontend.app.root.Agent()
	if err != nil {
		return err
	}
	frontend.controller = controller
	events, dispose, err := controller.Subscribe(1)
	if err != nil {
		return err
	}
	frontend.events = events
	if err := scope.Defer(func(context.Context) error {
		dispose()
		// Dependencies must still be active during frontend teardown.
		_, err := controller.Events(ctx)
		frontend.closed = err == nil
		return err
	}); err != nil {
		dispose()
		return err
	}
	return frontend.app.approval.RegisterBroker(frontend, scope)
}

func TestApplication_SupportsIndependentFrontendPlugin(t *testing.T) {
	previous := newTerminal
	t.Cleanup(func() { newTerminal = previous })
	newTerminal = func(tui.Config) (*tui.App, error) { t.Fatal("shared application constructed TUI"); return nil, nil }
	root := t.TempDir()
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(root, "sessions"),
		settingsPath: filepath.Join(root, "settings.yaml"), credentialPath: filepath.Join(root, "credentials.yaml"),
		skillsDir: filepath.Join(root, "skills"), agentsSkillsDir: filepath.Join(root, "agents-skills"),
		sessionID: "frontend-probe", maxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	frontend := &probeFrontend{app: app}
	runtime, err := plugin.New(append(app.plugins, frontend)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if err := app.registry.SetPolicy(t.Context(), "frontend-probe", session.ApprovalNever); err != nil {
		t.Fatal(err)
	}
	event := <-frontend.events
	if event.Record.Type != session.RecordApprovalPolicy {
		t.Fatalf("frontend saw %s", event.Record.Type)
	}
	if frontend.controller.Status().SessionID != "frontend-probe" {
		t.Fatal("frontend used a different session")
	}
	if err := runtime.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !frontend.closed {
		t.Fatal("frontend stopped after its dependencies")
	}
	if _, open := <-frontend.events; open {
		t.Fatal("frontend subscription survived shutdown")
	}
}

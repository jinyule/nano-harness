package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

func TestComposition_CanceledSettingsUpdatePreservesFileAndRevision(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	path := filepath.Join(data, "settings.yaml")
	const original = "{}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"),
		spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"),
		settingsPath: path, credentialPath: filepath.Join(data, "credentials.yaml"),
		skillsDir: filepath.Join(data, "skills"), agentsSkillsDir: filepath.Join(data, "agents-skills"),
		sessionID: "settings-cancel", maxSteps: 1, create: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := plugin.New(app.plugins...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, revision, err := app.settings.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := app.settings.Update(ctx, revision, func(document *settings.Document) error {
		document.Route = settings.Route{Provider: "anthropic", Model: "claude-sonnet-4-5"}
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled settings update = %v", err)
	}
	current, err := os.ReadFile(path) //nolint:gosec // this test owns the private settings path
	if err != nil || string(current) != original {
		t.Errorf("canceled update replaced settings file, err=%v", err)
	}
	after, nextRevision, err := app.settings.Snapshot()
	if err != nil || nextRevision != revision || after.Route != before.Route {
		t.Errorf("canceled update changed snapshot: route=%v revision=%d err=%v", after.Route, nextRevision, err)
	}
	for _, pattern := range []string{path + ".lock", path + ".tmp-*"} {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) != 0 {
			t.Errorf("canceled update left files: %v, err=%v", files, err)
		}
	}
}

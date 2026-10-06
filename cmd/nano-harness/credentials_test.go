package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	credentialfile "github.com/jinyule/nano-harness/internal/adapter/credential/file"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

func TestComposition_CanceledCredentialLogoutPreservesFile(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	path := filepath.Join(data, "credentials.yaml")
	store, err := credentialfile.New(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := store.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scope.Close(context.Background()) }()
	if _, err := store.Modify(t.Context(), "openai", func(*llm.Credential) (*llm.Credential, error) {
		return &llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "fixture-account"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path) //nolint:gosec // this test owns the private credential path
	if err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"),
		spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"),
		settingsPath: filepath.Join(data, "settings.yaml"), credentialPath: path,
		skillsDir: filepath.Join(data, "skills"), agentsSkillsDir: filepath.Join(data, "agents-skills"),
		sessionID: "credential-cancel", maxSteps: 1, create: true,
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := app.models.Logout(ctx, "openai"); !errors.Is(err, context.Canceled) {
		t.Errorf("pre-canceled logout = %v", err)
	}
	current, err := os.ReadFile(path) //nolint:gosec // this test owns the private credential path
	if err != nil || !bytes.Equal(current, original) {
		t.Errorf("canceled logout changed credential file, err=%v", err)
	}
	accounts, err := app.models.Accounts(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].Provider != "openai" {
		t.Errorf("canceled logout changed account metadata, err=%v", err)
	}
	for _, pattern := range []string{path + ".lock", path + ".tmp-*"} {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) != 0 {
			t.Errorf("canceled logout left staging or lock files, err=%v", err)
		}
	}
}

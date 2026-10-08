package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/adapter/tui"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// TestNormalizeConfig_KeepsSecretsAndSessionsOutsideTheWorkspace proves the
// credential and settings files may not resolve inside the workspace, and
// the session root and the workspace may not contain each other. Files are
// judged by their own resolved location, so a workspace inside a file's
// directory is allowed.
func TestNormalizeConfig_KeepsSecretsAndSessionsOutsideTheWorkspace(t *testing.T) {
	restoreMainHooks(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(workspaceRoot, ".config"), outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// alias reaches the workspace through a link; secret.yaml outside links
	// to a file inside it.
	if err := os.Symlink(filepath.Join(workspaceRoot, ".config"), filepath.Join(outside, "alias")); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(workspaceRoot, ".config", "inner.yaml")
	if err := os.WriteFile(inner, []byte("synthetic: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(outside, "secret.yaml")); err != nil {
		t.Fatal(err)
	}
	valid := func() applicationConfig {
		return applicationConfig{
			workspaceRoot: workspaceRoot, sessionRoot: filepath.Join(outside, "sessions"), spillRoot: filepath.Join(outside, "spill"),
			attachmentRoot: filepath.Join(outside, "attachments"), settingsPath: filepath.Join(outside, "settings.yaml"),
			credentialPath: filepath.Join(outside, "credentials.yaml"), skillsDir: filepath.Join(outside, "skills"),
			agentsSkillsDir: filepath.Join(outside, "agents"), sessionID: "session", maxSteps: 1,
		}
	}
	for _, test := range []struct {
		name, flag string
		set        func(*applicationConfig)
		allowed    bool
	}{
		{"credentials inside, not yet created", "--credentials", func(c *applicationConfig) {
			c.credentialPath = filepath.Join(workspaceRoot, ".config", "nano-harness", "credentials.yaml")
		}, false},
		{"credentials through a linked directory", "--credentials", func(c *applicationConfig) {
			c.credentialPath = filepath.Join(outside, "alias", "credentials.yaml")
		}, false},
		{"credentials linked to a workspace file", "--credentials", func(c *applicationConfig) { c.credentialPath = filepath.Join(outside, "secret.yaml") }, false},
		{"settings inside", "--settings", func(c *applicationConfig) { c.settingsPath = filepath.Join(workspaceRoot, "settings.yaml") }, false},
		{"settings linked to a workspace file", "--settings", func(c *applicationConfig) { c.settingsPath = filepath.Join(outside, "secret.yaml") }, false},
		{"session root inside", "--session-root", func(c *applicationConfig) { c.sessionRoot = filepath.Join(workspaceRoot, ".config", "sessions") }, false},
		{"session root is the workspace", "--session-root", func(c *applicationConfig) { c.sessionRoot = workspaceRoot }, false},
		{"session root linked into the workspace", "--session-root", func(c *applicationConfig) { c.sessionRoot = filepath.Join(outside, "alias", "sessions") }, false},
		{"session root containing the workspace", "--session-root", func(c *applicationConfig) { c.sessionRoot = base }, false},
		{"credentials beside the workspace", "", func(c *applicationConfig) { c.credentialPath = filepath.Join(base, "credentials.yaml") }, true},
		{"settings sharing the workspace prefix", "", func(c *applicationConfig) { c.settingsPath = workspaceRoot + "-settings.yaml" }, true},
		{"credentials sharing the workspace prefix", "", func(c *applicationConfig) {
			c.credentialPath = filepath.Join(workspaceRoot+"-config", "credentials.yaml")
		}, true},
		{"session root sharing the workspace prefix", "", func(c *applicationConfig) { c.sessionRoot = workspaceRoot + "-sessions" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid()
			test.set(&config)
			_, err := normalizeConfig(config)
			if test.allowed != (err == nil) || !test.allowed && !strings.Contains(err.Error(), "choose another "+test.flag) {
				t.Fatalf("normalizeConfig = %v", err)
			}
		})
	}
}

// TestRunTUI_RefusesAHomeWorkspaceHoldingTheDefaultPrivateFiles proves the
// real entry point refuses --root at home, where the default configuration
// directory lies, naming every flag to move and creating nothing.
func TestRunTUI_RefusesAHomeWorkspaceHoldingTheDefaultPrivateFiles(t *testing.T) {
	restoreMainHooks(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	credentials := filepath.Join(home, ".config", "nano-harness", "credentials.yaml")
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("synthetic: credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	currentWorkingDirectory = func() (string, error) { return home, nil }
	userConfigDirectory = func() (string, error) { return filepath.Join(home, ".config"), nil }
	userHomeDirectory = func() (string, error) { return home, nil }
	started := false
	deps := dependencies{
		startRuntime:    func(context.Context, *plugin.Runtime) error { started = true; return nil },
		runTerminal:     func(context.Context, *tui.App, io.Reader, io.Writer) error { return nil },
		shutdownRuntime: func(context.Context, *plugin.Runtime) error { return nil },
	}
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"tui", "--root", home, "--spill-root", filepath.Join(base, "spill"), "--attachment-root", filepath.Join(base, "attachments")}, strings.NewReader(""), io.Discard, &stderr, deps)
	for _, flag := range []string{"--session-root", "--credentials", "--settings"} {
		if !strings.Contains(stderr.String(), "choose another "+flag) {
			t.Errorf("stderr lacks %s guidance: %q", flag, stderr.String())
		}
	}
	if code != 2 || started {
		t.Fatalf("code = %d, started = %t", code, started)
	}
	if entries, err := os.ReadDir(filepath.Dir(credentials)); err != nil || len(entries) != 1 {
		t.Fatalf("startup created private files: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(credentials); err != nil || string(data) != "synthetic: credential\n" { //nolint:gosec // fixed name under this test's private directory
		t.Fatalf("credentials changed: %q, %v", data, err)
	}
}

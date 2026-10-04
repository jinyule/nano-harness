package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommand_RejectsArchitectureViolations(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "archcheck")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil { //nolint:gosec // fixed Go command builds the checker into a test-owned private directory
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, test := range []struct{ name, path, source, diagnostic string }{
		{"valid", "internal/core/value/value.go", "package value\nimport _ \"context\"\n", ""},
		{"external-app", "internal/app/value/value.go", "package value\nimport _ \"example.test/external\"\n", "core/app may depend only on standard library"},
		{"external-core", "internal/core/value/value.go", "package value\nimport _ \"example.test/external\"\n", "core/app may depend only on standard library"},
		{"adapter-tool", "internal/adapter/value/value.go", "package value\nimport _ \"example.test/project/internal/tools/helper\"\n", "repository tools cannot be product dependencies"},
		{"command-tool", "cmd/nano-harness/extra.go", "package main\nimport _ \"example.test/project/internal/tools/helper\"\n", "repository tools cannot be product dependencies"},
		{"alternate-command", "cmd/other/main.go", "package main\nfunc main() {}\n", "unsupported product entry point"},
		{"windows-command", "examples/other_windows.go", "//go:build windows\n\npackage main\nfunc main() {}\n", "unsupported product entry point"},
		{"tagged-dependency", "internal/adapter/value/extra_windows.go", "//go:build windows\n\npackage value\nimport _ \"example.test/project/internal/tools/helper\"\n", "repository tools cannot be product dependencies"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"go.mod":                          "module example.test/project\n\ngo 1.26.0\nrequire example.test/external v0.0.0\nreplace example.test/external => ./external\n",
				"external/go.mod":                 "module example.test/external\n\ngo 1.26.0\n",
				"external/value.go":               "package external\n",
				"cmd/nano-harness/main.go":        "package main\nfunc main() {}\n",
				"internal/tools/helper/helper.go": "package helper\n",
				"internal/adapter/value/value.go": "package value\n",
			}
			files[test.path] = test.source
			for name, contents := range files {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.CommandContext(t.Context(), binary)
			command.Dir = root
			output, err := command.CombinedOutput()
			if test.diagnostic == "" {
				if err != nil {
					t.Fatalf("valid: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), test.diagnostic) {
				t.Fatalf("got %v\n%s; want rejection %q", err, output, test.diagnostic)
			}
		})
	}
}

package main

import (
	"strings"
	"testing"
)

func TestViolationsFor(t *testing.T) {
	t.Parallel()

	const module = "example.test/nano-harness"
	tests := []struct {
		name       string
		pkg        packageInfo
		wantReason string
	}{
		{
			name: "core may import core",
			pkg: packageInfo{
				ImportPath: module + "/internal/core/session",
				Imports:    []string{module + "/internal/core/event", "context"},
			},
		},
		{
			name: "core may not import adapter",
			pkg: packageInfo{
				ImportPath: module + "/internal/core/session",
				Imports:    []string{module + "/internal/adapter/sqlite"},
			},
			wantReason: "core may depend only on core",
		},
		{
			name: "app may import core",
			pkg: packageInfo{
				ImportPath: module + "/internal/app/run",
				Imports:    []string{module + "/internal/core/session"},
			},
		},
		{
			name: "app may not import platform",
			pkg: packageInfo{
				ImportPath: module + "/internal/app/run",
				Imports:    []string{module + "/internal/platform/process"},
			},
			wantReason: "application packages may depend only on app and core",
		},
		{
			name: "adapter may import its own children",
			pkg: packageInfo{
				ImportPath: module + "/internal/adapter/sqlite/store",
				Imports:    []string{module + "/internal/adapter/sqlite/schema"},
			},
		},
		{
			name: "adapter may not import sibling",
			pkg: packageInfo{
				ImportPath: module + "/internal/adapter/sqlite",
				Imports:    []string{module + "/internal/adapter/http"},
			},
			wantReason: "an adapter may not depend on a sibling adapter",
		},
		{
			name: "platform may not import core",
			pkg: packageInfo{
				ImportPath: module + "/internal/platform/process",
				Imports:    []string{module + "/internal/core/session"},
			},
			wantReason: "platform packages must remain domain-independent",
		},
		{
			name: "non-command may not import command",
			pkg: packageInfo{
				ImportPath: module + "/internal/version",
				Imports:    []string{module + "/cmd/nano-harness"},
			},
			wantReason: "command packages are composition roots",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			violations := violationsFor(module, test.pkg)
			if test.wantReason == "" && len(violations) != 0 {
				t.Fatalf("violationsFor() = %v, want none", violations)
			}
			if test.wantReason != "" && (len(violations) != 1 || !strings.Contains(violations[0], test.wantReason)) {
				t.Fatalf("violationsFor() = %v, want one containing %q", violations, test.wantReason)
			}
		})
	}
}

func TestLocalPath(t *testing.T) {
	t.Parallel()

	if got, ok := localPath("example.test/project", "example.test/project/internal/core"); !ok || got != "internal/core" {
		t.Fatalf("localPath() = %q, %v", got, ok)
	}
	if _, ok := localPath("example.test/project", "example.test/project-two/internal/core"); ok {
		t.Fatal("localPath() accepted a different module")
	}
}

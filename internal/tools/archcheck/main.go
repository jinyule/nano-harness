// Command archcheck enforces the repository's directional package dependencies.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

type packageInfo struct {
	ImportPath string   `json:"ImportPath"`
	Imports    []string `json:"Imports"`
}

func main() {
	module, err := goOutput("list", "-m", "-f", "{{.Path}}")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	packages, err := listPackages()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var violations []string
	for _, pkg := range packages {
		violations = append(violations, violationsFor(strings.TrimSpace(module), pkg)...)
	}
	slices.Sort(violations)
	for _, violation := range violations {
		fmt.Fprintln(os.Stderr, violation)
	}
	if len(violations) != 0 {
		os.Exit(1)
	}

	fmt.Printf("architecture: checked %d packages\n", len(packages))
}

func goOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// The executable is fixed; callers only select repository-owned go subcommands.
	output, err := exec.CommandContext(ctx, "go", args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	return string(output), nil
}

func listPackages() ([]packageInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "list", "-json", "./...")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture go list output: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start go list: %w", err)
	}

	var packages []packageInfo
	decoder := json.NewDecoder(stdout)
	for decoder.More() {
		var pkg packageInfo
		if err := decoder.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		packages = append(packages, pkg)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("go list packages: %w", err)
	}
	return packages, nil
}

func violationsFor(module string, pkg packageInfo) []string {
	importer, local := localPath(module, pkg.ImportPath)
	if !local || strings.HasPrefix(importer, "cmd/") || strings.HasPrefix(importer, "internal/tools/") {
		return nil
	}

	var violations []string
	for _, importedPath := range pkg.Imports {
		imported, isLocal := localPath(module, importedPath)
		if !isLocal {
			continue
		}
		if reason := forbiddenImport(importer, imported); reason != "" {
			violations = append(violations, fmt.Sprintf("architecture: %s imports %s: %s", importer, imported, reason))
		}
	}
	return violations
}

func localPath(module, importPath string) (string, bool) {
	if importPath == module {
		return ".", true
	}
	prefix := module + "/"
	if !strings.HasPrefix(importPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(importPath, prefix), true
}

func forbiddenImport(importer, imported string) string {
	if strings.HasPrefix(imported, "cmd/") {
		return "command packages are composition roots and cannot be dependencies"
	}

	switch {
	case inLayer(importer, "internal/core"):
		if !inLayer(imported, "internal/core") {
			return "core may depend only on core"
		}
	case inLayer(importer, "internal/app"):
		if !inLayer(imported, "internal/app") && !inLayer(imported, "internal/core") {
			return "application packages may depend only on app and core"
		}
	case inLayer(importer, "internal/adapter"):
		if inLayer(imported, "internal/adapter") && adapterRoot(importer) != adapterRoot(imported) {
			return "an adapter may not depend on a sibling adapter"
		}
	case inLayer(importer, "internal/platform"):
		if !inLayer(imported, "internal/platform") {
			return "platform packages must remain domain-independent"
		}
	}
	return ""
}

func inLayer(path, layer string) bool {
	return path == layer || strings.HasPrefix(path, layer+"/")
}

func adapterRoot(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return path
	}
	return strings.Join(parts[:3], "/")
}

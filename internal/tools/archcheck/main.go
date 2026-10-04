// Command archcheck enforces source dependencies and product entry points on all platforms.
package main

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type packageInfo struct {
	ImportPath string
	Imports    []string
}

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check() error {
	module, err := goOutput("list", "-m", "-f", "{{.Path}}")
	if err != nil {
		return err
	}
	standard, err := goOutput("list", "std")
	if err != nil {
		return err
	}
	standards := map[string]bool{}
	for name := range strings.FieldsSeq(standard) {
		standards[name] = true
	}
	violations, err := scanSources(".", strings.TrimSpace(module), standards)
	if err != nil {
		return err
	}
	slices.Sort(violations)
	if len(violations) > 0 {
		return fmt.Errorf("%s", strings.Join(violations, "\n"))
	}
	fmt.Println("architecture: all source imports and entry points verified")
	return nil
}

func goOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "go", args...).CombinedOutput() //nolint:gosec // executable and subcommands are fixed by this tool
	if err != nil {
		return "", fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	return string(output), nil
}

// Parse every source file, including files excluded by the host's build tags.
func scanSources(root, module string, standard map[string]bool) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || slices.Contains([]string{"third_party", "vendor", "testdata", "bin", "dist", "release-artifacts"}, entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if source.Name.Name == "main" && relative != "cmd/nano-harness" && !inLayer(relative, "internal/tools") {
			violations = append(violations, fmt.Sprintf("architecture: %s: unsupported product entry point", path))
		}
		pkg := packageInfo{ImportPath: module + "/" + relative}
		for _, spec := range source.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if standard[name] {
				continue
			}
			if _, local := localPath(module, name); !local && (inLayer(relative, "internal/core") || inLayer(relative, "internal/app")) {
				violations = append(violations, fmt.Sprintf("architecture: %s imports %s: core/app may depend only on standard library and their allowed local layers", relative, name))
			}
			pkg.Imports = append(pkg.Imports, name)
		}
		violations = append(violations, violationsFor(module, pkg)...)
		return nil
	})
	return slices.Compact(violations), err
}

func violationsFor(module string, pkg packageInfo) []string {
	importer, local := localPath(module, pkg.ImportPath)
	if !local || inLayer(importer, "internal/tools") {
		return nil
	}
	var violations []string
	for _, name := range pkg.Imports {
		imported, isLocal := localPath(module, name)
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
	if inLayer(imported, "internal/tools") {
		return "repository tools cannot be product dependencies"
	}
	if inLayer(imported, "cmd") {
		return "command packages are composition roots and cannot be dependencies"
	}
	switch {
	case importer == "cmd/nano-harness":
		if imported == "internal/version" || inLayer(imported, "internal/core") || inLayer(imported, "internal/app") || inLayer(imported, "internal/adapter") || inLayer(imported, "internal/platform") {
			return ""
		}
	case inLayer(importer, "internal/core"):
		if inLayer(imported, "internal/core") {
			return ""
		}
		return "core may depend only on core"
	case inLayer(importer, "internal/app"):
		if inLayer(imported, "internal/app") || inLayer(imported, "internal/core") {
			return ""
		}
		return "application packages may depend only on app and core"
	case inLayer(importer, "internal/adapter"):
		if inLayer(imported, "internal/app") || inLayer(imported, "internal/core") || inLayer(imported, "internal/platform") {
			return ""
		}
		if inLayer(imported, "internal/adapter") {
			if adapterRoot(importer) == adapterRoot(imported) {
				return ""
			}
			return "an adapter may not depend on a sibling adapter"
		}
	case inLayer(importer, "internal/platform"):
		if inLayer(imported, "internal/platform") {
			return ""
		}
		return "platform packages must remain domain-independent"
	}
	return "dependency is outside the allowed layers"
}

func inLayer(path, layer string) bool { return path == layer || strings.HasPrefix(path, layer+"/") }
func adapterRoot(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return path
	}
	return strings.Join(parts[:3], "/")
}

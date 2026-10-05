package goal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// humanSourceProducers are the only product packages allowed to attribute a
// message to HumanSource: the terminal, for typed input and commands, and the
// image normalizer behind its /attach command.
var humanSourceProducers = []string{"internal/adapter/media/image", "internal/adapter/tui"}

// TestHumanSource_OnlyFrontendsAttributeHumanInput guards the invariant that
// direct-human goal authority rests on: a "user" message source comes from a
// person. It parses every product source file and fails when any other
// package sets a source Kind to "user" or to HumanSource.
func TestHumanSource_OnlyFrontendsAttributeHumanInput(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	producers := map[string][]string{}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(root, path)
			relative = filepath.ToSlash(relative)
			if entry.IsDir() {
				// Repository tools and fixtures never enter the product binary.
				if relative == "internal/tools" || entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			for _, position := range humanSourceAssignments(t, path) {
				directory := filepath.ToSlash(filepath.Dir(relative))
				producers[directory] = append(producers[directory], relative+":"+strconv.Itoa(position))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	directories := make([]string, 0, len(producers))
	for directory := range producers {
		directories = append(directories, directory)
	}
	slices.Sort(directories)
	if !slices.Equal(directories, humanSourceProducers) {
		t.Fatalf("human message sources are built in %v, want only %v: %v", directories, humanSourceProducers, producers)
	}
}

// humanSourceAssignments returns the lines where a file sets a Kind field to
// the human source, either as a composite literal key or an assignment.
func humanSourceAssignments(t *testing.T, path string) []int {
	t.Helper()
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok && key.Name == "Kind" && isHumanSource(node.Value) {
				lines = append(lines, files.Position(node.Pos()).Line)
			}
		case *ast.AssignStmt:
			for index, target := range node.Lhs {
				if selector, ok := target.(*ast.SelectorExpr); ok && selector.Sel.Name == "Kind" && index < len(node.Rhs) && isHumanSource(node.Rhs[index]) {
					lines = append(lines, files.Position(node.Pos()).Line)
				}
			}
		}
		return true
	})
	return lines
}

func isHumanSource(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.BasicLit:
		text, err := strconv.Unquote(value.Value)
		return value.Kind == token.STRING && err == nil && text == HumanSource
	case *ast.Ident:
		return value.Name == "HumanSource"
	case *ast.SelectorExpr:
		return value.Sel.Name == "HumanSource"
	default:
		return false
	}
}

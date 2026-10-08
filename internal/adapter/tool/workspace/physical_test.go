package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRoot_PhysicalParentTraversal(t *testing.T) {
	base := tempRoot(t)
	root, err := Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(base, "picked.txt"))
	mustWrite(t, filepath.Join(base, "a", "picked.txt"))
	if err := os.Mkdir(filepath.Join(base, "a", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustLink(t, "a/nested", filepath.Join(base, "alias"))
	mustLink(t, ".", filepath.Join(base, "rootlink"))
	for _, test := range []struct {
		path, target string
		existing     error
		writable     error
	}{
		{path: "alias/../picked.txt", target: "a/picked.txt", writable: ErrSymlink},
		{path: "a//nested/./../picked.txt", target: "a/picked.txt"},
		{path: "a/nested/../new/child.txt", target: "a/new/child.txt", existing: fs.ErrNotExist},
		{path: "missing/../picked.txt", existing: fs.ErrNotExist, writable: fs.ErrNotExist},
		{path: "missing/new/../../picked.txt", existing: fs.ErrNotExist, writable: fs.ErrNotExist},
		{path: "picked.txt/../picked.txt", existing: syscall.ENOTDIR, writable: syscall.ENOTDIR},
		{path: "rootlink/../" + filepath.Base(base) + "/picked.txt", existing: ErrOutsideRoot, writable: ErrSymlink},
		{path: "../" + filepath.Base(base) + "/picked.txt", existing: ErrOutsideRoot, writable: ErrOutsideRoot},
	} {
		for _, path := range []string{test.path, base + string(filepath.Separator) + test.path} {
			t.Run(path, func(t *testing.T) {
				display, resolved, err := root.Existing(path)
				if !matches(err, test.existing) || err == nil && (resolved != filepath.Join(base, test.target) || display != resolved) {
					t.Errorf("Existing = %q, %q, %v", display, resolved, err)
				}
				target, err := root.Writable(path)
				if !matches(err, test.writable) || err == nil && target != filepath.Join(base, test.target) {
					t.Errorf("Writable = %q, %v", target, err)
				}
			})
		}
	}
	// A different absolute prefix cannot be erased to impersonate the root.
	if _, _, err := root.Existing(filepath.Dir(base) + "/missing/../" + filepath.Base(base) + "/picked.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("absolute prefix = %v", err)
	}
}

func TestRoot_PhysicalReadOnlyTraversal(t *testing.T) {
	base := tempRoot(t)
	work, spill := filepath.Join(base, "work"), filepath.Join(base, "spill")
	mustWrite(t, filepath.Join(work, "picked.txt"))
	mustWrite(t, filepath.Join(spill, "a", "picked.txt"))
	if err := os.Mkdir(filepath.Join(spill, "a", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustLink(t, "a/nested", filepath.Join(spill, "alias"))
	mustLink(t, ".", filepath.Join(spill, "rootlink"))
	root, _ := Resolve(work)
	root = root.WithReadOnly(spill)
	for _, test := range []struct {
		path string
		want error
	}{
		{"alias/../picked.txt", nil},
		{"missing/../a/picked.txt", fs.ErrNotExist},
		{"rootlink/../spill/a/picked.txt", ErrOutsideRoot},
	} {
		t.Run(test.path, func(t *testing.T) {
			display, resolved, err := root.Readable(spill + "/" + test.path)
			if !matches(err, test.want) || err == nil && (resolved != filepath.Join(spill, "a", "picked.txt") || display != resolved) {
				t.Fatalf("Readable = %q, %q, %v", display, resolved, err)
			}
		})
	}
}

func TestRoot_PhysicalResolutionPropagatesIOFailures(t *testing.T) {
	restoreHooks(t)
	base := tempRoot(t)
	root, _ := Resolve(base)
	mustLink(t, ".", filepath.Join(base, "alias"))
	failure := errors.New("disk failure")
	statPath = func(string) (os.FileInfo, error) { return nil, failure }
	if _, _, err := root.Existing("alias/.."); !errors.Is(err, failure) {
		t.Fatalf("stat failure = %v", err)
	}
	statPath = os.Stat
	resolveLinks = func(string) (string, error) { return "", failure }
	if _, _, err := root.Existing("alias/.."); !errors.Is(err, failure) {
		t.Fatalf("link failure = %v", err)
	}
	if _, err := root.Writable("missing/../file"); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "parent traversal crosses a missing directory") {
		t.Fatalf("missing traversal diagnostic = %v", err)
	}
}

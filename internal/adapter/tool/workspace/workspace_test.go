package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func restoreHooks(t *testing.T) {
	t.Helper()
	abs, links, stat, lstat, relative := resolveAbs, resolveLinks, statPath, lstatPath, relativePath
	t.Cleanup(func() {
		resolveAbs, resolveLinks, statPath, lstatPath, relativePath = abs, links, stat, lstat, relative
	})
}

func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolve_ValidatesAndFixesRoot(t *testing.T) {
	restoreHooks(t)
	root := tempRoot(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(link)
	if err != nil || resolved.Path() != root {
		t.Fatalf("Resolve(link) = %q, %v", resolved.Path(), err)
	}
	if (Root{}).Path() != "" {
		t.Fatal("zero root has a path")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{" ", file, filepath.Join(root, "missing")} {
		if _, err := Resolve(path); !errors.Is(err, ErrInvalidRoot) {
			t.Fatalf("Resolve(%q) = %v", path, err)
		}
	}
	resolveAbs = func(string) (string, error) { return "", errors.New("abs") }
	if _, err := Resolve(root); !errors.Is(err, ErrInvalidRoot) || !strings.Contains(err.Error(), "abs") {
		t.Fatalf("abs error = %v", err)
	}
}

// TestRoot_PathMatrix pins which model paths each resolution mode accepts.
func TestRoot_PathMatrix(t *testing.T) {
	restoreHooks(t)
	base := tempRoot(t)
	root, err := Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	outside := tempRoot(t)
	mustWrite(t, filepath.Join(base, "dir", "file.txt"))
	mustWrite(t, filepath.Join(outside, "secret.txt"))
	mustLink(t, filepath.Join(outside, "secret.txt"), filepath.Join(base, "escape"))
	mustLink(t, filepath.Join(base, "dir", "file.txt"), filepath.Join(base, "alias"))
	mustLink(t, filepath.Join(base, "dir"), filepath.Join(base, "dirlink"))

	type outcome struct {
		lexical, existing, writable error
	}
	for _, test := range []struct {
		path string
		want outcome
	}{
		{path: "dir/file.txt"},
		{path: "./dir/../dir/file.txt"},
		{path: filepath.Join(base, "dir", "file.txt")},
		{path: "dir/new/child.txt", want: outcome{existing: fs.ErrNotExist}},
		{path: "alias", want: outcome{writable: ErrSymlink}},
		{path: "dirlink/file.txt", want: outcome{writable: ErrSymlink}},
		{path: "dirlink/new.txt", want: outcome{existing: fs.ErrNotExist, writable: ErrSymlink}},
		{path: "escape", want: outcome{existing: ErrOutsideRoot, writable: ErrSymlink}},
		{path: "../" + filepath.Base(outside) + "/secret.txt", want: outcome{ErrOutsideRoot, ErrOutsideRoot, ErrOutsideRoot}},
		{path: filepath.Join(outside, "secret.txt"), want: outcome{ErrOutsideRoot, ErrOutsideRoot, ErrOutsideRoot}},
		{path: base + "/../" + filepath.Base(outside), want: outcome{ErrOutsideRoot, ErrOutsideRoot, ErrOutsideRoot}},
	} {
		t.Run(test.path, func(t *testing.T) {
			if _, err := root.Lexical(test.path); !matches(err, test.want.lexical) {
				t.Errorf("Lexical error = %v, want %v", err, test.want.lexical)
			}
			if _, _, err := root.Existing(test.path); !matches(err, test.want.existing) {
				t.Errorf("Existing error = %v, want %v", err, test.want.existing)
			}
			if _, err := root.Writable(test.path); !matches(err, test.want.writable) {
				t.Errorf("Writable error = %v, want %v", err, test.want.writable)
			}
		})
	}
	lexical, resolved, err := root.Existing("alias")
	if err != nil || lexical != filepath.Join(base, "alias") || resolved != filepath.Join(base, "dir", "file.txt") {
		t.Fatalf("alias = %q %q %v", lexical, resolved, err)
	}
	if lexical, _, err := root.Existing("missing"); !errors.Is(err, fs.ErrNotExist) || lexical != filepath.Join(base, "missing") {
		t.Fatalf("missing lexical = %q, %v", lexical, err)
	}
	if target, err := root.Writable("."); err != nil || target != base {
		t.Fatalf("root target = %q, %v", target, err)
	}
	if got := root.Relative(filepath.Join(base, "dir", "file.txt")); got != "dir/file.txt" {
		t.Fatalf("relative = %q", got)
	}
	if got := root.Relative(base); got != "." {
		t.Fatalf("root relative = %q", got)
	}

	lstatPath = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
	if _, err := root.Writable("dir/file.txt"); err == nil || err.Error() != "lstat" {
		t.Fatalf("lstat error = %v", err)
	}
	relativePath = func(string, string) (string, error) { return "", errors.New("relative") }
	if got := root.Relative(filepath.Join(base, "x")); got != filepath.ToSlash(filepath.Join(base, "x")) {
		t.Fatalf("relative fallback = %q", got)
	}
	if _, err := root.Lexical("x"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("relative failure must fail closed: %v", err)
	}
}

func matches(err, want error) bool {
	if want == nil {
		return err == nil
	}
	return errors.Is(err, want)
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestEscalationVocabulary_MatchesUpstream(t *testing.T) {
	properties := EscalationProperties("command", "command")
	if len(properties) != 2 || properties[0].Name != "sandbox_permissions" || properties[1].Name != "justification" || properties[0].Required || properties[1].Required {
		t.Fatalf("properties = %#v", properties)
	}
	if got := strings.Join(properties[0].Schema.Enum, ","); got != "workspace-write,danger-full-access" {
		t.Fatalf("enum = %q", got)
	}
	if !strings.Contains(properties[0].Schema.Description, "exact command the sandbox just denied") || !strings.Contains(properties[1].Schema.Description, "user’s current request") {
		t.Fatalf("descriptions = %#v", properties)
	}
	mode, reason, blank := "danger-full-access", "needs host", " "
	for _, test := range []struct {
		mode, justification *string
		want                string
	}{
		{},
		{mode: &mode, justification: &reason},
		{mode: &mode, want: "requires a justification"},
		{justification: &reason, want: "only valid together"},
		{mode: &mode, justification: &blank, want: "non-empty sentence"},
	} {
		err := ValidateEscalation(test.mode, test.justification)
		if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
			t.Errorf("ValidateEscalation(%v, %v) = %v", test.mode, test.justification, err)
		}
	}
	if DenialMarker(ModeWorkspaceWrite) != "[sandbox: file access denied under workspace-write mode]" {
		t.Fatal("denial marker")
	}
	if !strings.HasPrefix(EscalationHint("command"), "[sandbox: escalation available — retry this exact command once") {
		t.Fatal("escalation hint")
	}
}

package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	coreskill "github.com/jinyule/nano-harness/internal/core/skill"
)

func TestDiscover_RanksRootsAndResolvesDuplicates(t *testing.T) {
	f := newFixture(t)
	nano := filepath.Join(f.workspace, ".nano-harness", "skills")
	writeFile(t, filepath.Join(nano, "dup", "SKILL.md"), skillText("dup", "Project nano wins."))
	writeFile(t, filepath.Join(f.projectRoot(), "dup.md"), skillText("dup", "Project agents loses."))
	writeFile(t, filepath.Join(f.projectRoot(), "a-flat.md"), skillText("pair", "Flat sorts first."))
	writeFile(t, filepath.Join(f.projectRoot(), "b-dir", "SKILL.md"), skillText("pair", "Bundle sorts second."))
	writeFile(t, filepath.Join(f.user, "user-skill", "SKILL.md"), skillText("user-skill", "User."))
	writeFile(t, filepath.Join(f.user, "pair.md"), skillText("pair", "User loses to project."))
	writeFile(t, filepath.Join(f.user, ".system", "SKILL.md"), skillText("system", "Reserved."))
	writeFile(t, filepath.Join(f.agents, ".system", "SKILL.md"), skillText("agents-system", "Not reserved here."))
	writeFile(t, filepath.Join(f.agents, "user-skill.md"), skillText("user-skill", "Agents loses to user."))

	skills, err := f.provider.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]summary{}
	var names []string
	for _, skill := range skills {
		got[skill.name] = skill
		names = append(names, skill.name)
	}
	if !slices.Equal(names, []string{"agents-system", "dup", "pair", "user-skill"}) {
		t.Fatalf("names = %v", names)
	}
	for name, description := range map[string]string{"dup": "Project nano wins.", "pair": "Flat sorts first.", "user-skill": "User."} {
		if got[name].description != description {
			t.Errorf("%s = %+v", name, got[name])
		}
	}
	if got["pair"].file != filepath.Join(f.projectRoot(), "a-flat.md") || got["pair"].directory != f.projectRoot() {
		t.Fatalf("flat locator = %+v", got["pair"])
	}
	if got["dup"].file != filepath.Join(nano, "dup", "SKILL.md") || got["dup"].directory != filepath.Join(nano, "dup") {
		t.Fatalf("bundle locator = %+v", got["dup"])
	}
}

func TestDiscover_UsesNearestGitAncestorAsProjectRoot(t *testing.T) {
	f := newFixture(t)
	repository := filepath.Dir(f.workspace)
	nested := filepath.Join(f.workspace, "nested")
	writeFile(t, filepath.Join(repository, ".git"), "gitdir: elsewhere\n")
	writeFile(t, filepath.Join(nested, ".agents", "skills", "inner.md"), skillText("inner", "Inside the workspace."))
	writeFile(t, filepath.Join(f.projectRoot(), "outer.md"), skillText("outer", "At the git root."))
	provider := &Provider{config: Config{Workspace: nested, UserDir: f.user, AgentsDir: f.agents}}
	if roots := provider.roots(); roots[1].path != filepath.Join(f.workspace, ".agents", "skills") {
		t.Fatalf("project roots = %+v", roots)
	}
	if err := os.RemoveAll(filepath.Join(f.workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	if roots := provider.roots(); roots[0].path != filepath.Join(repository, ".nano-harness", "skills") {
		t.Fatalf("git file ancestor roots = %+v", roots)
	}
	if err := os.Remove(filepath.Join(repository, ".git")); err != nil {
		t.Fatal(err)
	}
	restoreHooks(t)
	statPath = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	if roots := provider.roots(); roots[1].path != filepath.Join(nested, ".agents", "skills") || roots[2].path != f.user || !roots[2].skipSystem || roots[3].path != f.agents {
		t.Fatalf("roots without git = %+v", roots)
	}
}

func TestDiscover_SkipsLinksSpecialAndInvalidFiles(t *testing.T) {
	f := newFixture(t)
	root := f.projectRoot()
	outside := filepath.Join(filepath.Dir(f.workspace), "outside")
	writeFile(t, filepath.Join(outside, "linked", "SKILL.md"), skillText("linked", "Through a directory link."))
	writeFile(t, filepath.Join(outside, "flat.md"), skillText("flat-link", "Through a file link."))
	writeFile(t, filepath.Join(outside, "inner.md"), skillText("inner-link", "Through an instruction link."))
	writeFile(t, filepath.Join(root, "ok.md"), skillText("ok", "Valid."))
	for link, target := range map[string]string{
		filepath.Join(root, "linked"):                 filepath.Join(outside, "linked"),
		filepath.Join(root, "flat.md"):                filepath.Join(outside, "flat.md"),
		filepath.Join(root, "inner-link", "SKILL.md"): filepath.Join(outside, "inner.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	exact := skillText("exact", "At the size limit.")
	exact += strings.Repeat("x", maxSkillBytes-len(exact))
	writeFile(t, filepath.Join(root, "exact.md"), exact)
	writeFile(t, filepath.Join(root, "oversized.md"), exact+"x")
	writeFile(t, filepath.Join(root, "binary.md"), skillText("binary", "Has NUL.")+"\x00")
	writeFile(t, filepath.Join(root, "latin1.md"), skillText("latin1", "Invalid UTF-8 \xff."))
	writeFile(t, filepath.Join(root, "notes.txt"), skillText("text", "Not markdown."))
	writeFile(t, filepath.Join(root, "missing", "README.md"), "no instruction file")
	if err := os.MkdirAll(filepath.Join(root, "dir-instruction", "SKILL.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := f.names(t); !slices.Equal(got, []string{"exact", "ok"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestParseSkill_ValidatesFrontmatter(t *testing.T) {
	valid := map[string]definition{
		"---\nname: a\ndescription: d\n---\n\n  Body\n\n":                     {name: "a", description: "d", model: true, user: true, body: "Body"},
		"---\r\nname: a\r\ndescription: d\r\n---\r\nBody\r\n":                 {name: "a", description: "d", model: true, user: true, body: "Body"},
		"---\nname: a\ndescription: d\nlicense: MIT\n---":                     {name: "a", description: "d", model: true, user: true},
		"---\nname: a\ndescription: d\n---\nfirst\n---\nsecond":               {name: "a", description: "d", model: true, user: true, body: "first\n---\nsecond"},
		"---\nname: a\ndescription: d\ndisable-model-invocation: true\n---\n": {name: "a", description: "d", model: false, user: true},
		"---\nname: a\ndescription: d\nuser-invocable: false\n---\n":          {name: "a", description: "d", model: true, user: false},
	}
	for raw, want := range valid {
		if got, ok := parseSkill(raw); !ok || got != want {
			t.Errorf("parseSkill(%q) = %+v, %v", raw, got, ok)
		}
	}
	for _, raw := range []string{
		"",
		"---",
		"name: a\ndescription: d\n",
		"\ufeff---\nname: a\ndescription: d\n---\n",
		"--- \nname: a\ndescription: d\n---\n",
		"---\nname: a\ndescription: d\n",
		"---\n---\nbody",
		"---\njust a scalar\n---\n",
		"---\n- a\n- b\n---\n",
		"---\nname: [unclosed\n---\n",
		"---\nname: a\nname: b\ndescription: d\n---\n",
		"---\ndescription: d\n---\n",
		"---\nname: 7\ndescription: d\n---\n",
		"---\nname: Bad_Name\ndescription: d\n---\n",
		"---\nname: a\n---\n",
		"---\nname: a\ndescription: \"\"\n---\n",
		"---\nname: a\ndescription: [d]\n---\n",
		"---\nname: a\ndescription: d\ndisableModelInvocation: true\n---\n",
		"---\nname: a\ndescription: d\nmodelInvocable: false\n---\n",
		"---\nname: a\ndescription: d\nuserInvocable: false\n---\n",
		"---\nname: a\ndescription: d\ndisable-model-invocation: maybe\n---\n",
		"---\nname: a\ndescription: d\nuser-invocable: 2\n---\n",
	} {
		if got, ok := parseSkill(raw); ok {
			t.Errorf("parseSkill(%q) accepted %+v", raw, got)
		}
	}
}

func TestFrontmatterBoolean_FollowsUpstreamGrammar(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "True": true, "yes": true, "ON": true, "1": true, "'1'": true, "1.0": true, "0x1": true,
		"false": false, "FALSE": false, "no": false, "Off": false, "0": false, "'0'": false, "0.0": false,
	} {
		raw := fmt.Sprintf("---\nname: a\ndescription: d\ndisable-model-invocation: %s\n---\n", value)
		got, ok := parseSkill(raw)
		if !ok || got.model == want {
			t.Errorf("disable-model-invocation: %s = %+v, %v", value, got, ok)
		}
	}
	for _, value := range []string{"maybe", "2", "-1", "0.5", "[]", "{}", "~", "''"} {
		raw := fmt.Sprintf("---\nname: a\ndescription: d\nuser-invocable: %s\n---\n", value)
		if got, ok := parseSkill(raw); ok {
			t.Errorf("user-invocable: %s accepted %+v", value, got)
		}
	}
}

func TestDiscover_ReportsIncompleteObservations(t *testing.T) {
	failure := errors.New("injected")
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *fixture)
		match string
	}{
		{name: "root is a file", match: "read skill root", setup: func(t *testing.T, f *fixture) {
			writeFile(t, f.agents, "file")
		}},
		{name: "root open", match: "open skill root", setup: func(_ *testing.T, f *fixture) {
			openPath = func(path string) (*os.File, error) {
				if path == f.user {
					return nil, failure
				}
				return os.Open(path) //nolint:gosec // test-owned temporary path
			}
		}},
		{name: "too many entries", match: "more than 1024 entries", setup: func(t *testing.T, f *fixture) {
			for index := range maxRootEntries + 1 {
				writeFile(t, filepath.Join(f.user, fmt.Sprintf("n%04d.txt", index)), "")
			}
		}},
		{name: "too many skills", match: "101 skills exceed the limit of 100", setup: func(t *testing.T, f *fixture) {
			for index := range coreskill.MaxCatalogEntries + 1 {
				name := fmt.Sprintf("s%03d", index)
				writeFile(t, filepath.Join(f.user, name+".md"), skillText(name, "Generated."))
			}
		}},
		{name: "instruction lstat", match: "inspect skill file", setup: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.user, "a.md"), skillText("a", "A."))
			lstatPath = func(string) (fs.FileInfo, error) { return nil, failure }
		}},
		{name: "instruction open", match: "open skill file", setup: func(t *testing.T, f *fixture) {
			file := filepath.Join(f.user, "a.md")
			writeFile(t, file, skillText("a", "A."))
			openPath = func(path string) (*os.File, error) {
				if path == file {
					return nil, failure
				}
				return os.Open(path) //nolint:gosec // test-owned temporary path
			}
		}},
		{name: "instruction fstat", match: "inspect skill file", setup: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.user, "a.md"), skillText("a", "A."))
			statFile = func(*os.File) (fs.FileInfo, error) { return nil, failure }
		}},
		{name: "instruction read", match: "read skill file", setup: func(t *testing.T, f *fixture) {
			file := filepath.Join(f.user, "a.md")
			writeFile(t, file, skillText("a", "A."))
			openPath = func(path string) (*os.File, error) {
				if path == file {
					return os.Open(f.user)
				}
				return os.Open(path) //nolint:gosec // test-owned temporary path
			}
			statFile = func(*os.File) (fs.FileInfo, error) { return os.Lstat(file) }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			restoreHooks(t)
			f := newFixture(t)
			test.setup(t, f)
			if _, err := f.provider.discover(context.Background()); err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("discover error = %v", err)
			}
		})
	}
}

func TestDiscover_ContainsRacesAndCancellation(t *testing.T) {
	restoreHooks(t)
	f := newFixture(t)
	vanished := filepath.Join(f.user, "vanished.md")
	swapped := filepath.Join(f.user, "swapped.md")
	writeFile(t, vanished, skillText("vanished", "Removed after inspection."))
	writeFile(t, swapped, skillText("swapped", "Replaced after inspection."))
	writeFile(t, filepath.Join(f.user, "kept.md"), skillText("kept", "Kept."))
	openPath = func(path string) (*os.File, error) {
		switch path {
		case vanished:
			return nil, fs.ErrNotExist
		case swapped:
			return os.Open(filepath.Join(f.user, "kept.md"))
		}
		return os.Open(path) //nolint:gosec // test-owned temporary path
	}
	if got := f.names(t); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("names = %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.provider.discover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discover error = %v", err)
	}
}

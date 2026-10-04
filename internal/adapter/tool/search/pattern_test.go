package search

import (
	"strings"
	"testing"
)

func TestCompilePattern_FollowsRipgrepGlobSemantics(t *testing.T) {
	for _, test := range []struct {
		pattern, path string
		isDir, want   bool
	}{
		{"*.ts", "a.ts", false, true},
		{"*.ts", "src/deep/a.ts", false, true},
		{"*.ts", "a.tsx", false, false},
		{"*", "src/deep/file", false, true},
		{"src/*.ts", "src/a.ts", false, true},
		{"src/*.ts", "src/x/a.ts", false, false},
		{"src/*.ts", "other/src/a.ts", false, false},
		{"/top.txt", "top.txt", false, true},
		{"/top.txt", "sub/top.txt", false, false},
		{"**/*.go", "main.go", false, true},
		{"**/*.go", "a/b/main.go", false, true},
		{"src/**/*.test.js", "src/a.test.js", false, true},
		{"src/**/*.test.js", "src/a/b/c.test.js", false, true},
		{"foo/**", "foo/bar/z.go", false, true},
		{"foo/**", "foo", true, false},
		{"**", "any/thing", false, true},
		{"a**b", "axxb", false, true},
		{"a**b", "a/b", false, false},
		{"?.md", "x.md", false, true},
		{"?.md", "xy.md", false, false},
		{"[nm]*.txt", "new.txt", false, true},
		{"[!nm]*.txt", "old.txt", false, true},
		{"[^nm]*.txt", "new.txt", false, false},
		{"[]]x", "]x", false, true},
		{"*.{js,jsx}", "a.jsx", false, true},
		{"*.{js,jsx}", "a.ts", false, false},
		{"{src,lib}/**/*.rs", "lib/a/b.rs", false, true},
		{`\*.md`, "*.md", false, true},
		{`\*.md`, "a.md", false, false},
		{"build/", "build", true, true},
		{"build/", "build", false, false},
		{"a.b", "axb", false, false},
		{"!*.log", "x.log", false, true},
	} {
		compiled, err := compilePattern(test.pattern)
		if err != nil {
			t.Fatalf("compile(%q) = %v", test.pattern, err)
		}
		if got := compiled.match(test.path, test.isDir); got != test.want {
			t.Errorf("%q.match(%q, dir=%v) = %v", test.pattern, test.path, test.isDir, got)
		}
	}
	if negated, _ := compilePattern("!x"); !negated.negated {
		t.Fatal("negation lost")
	}
	for _, invalid := range []string{"", "!", "/", `trailing\`, "{a,{b}}", "{open", "[open", "[z-a]"} {
		if _, err := compilePattern(invalid); err == nil {
			t.Errorf("compile(%q) accepted", invalid)
		}
	}
}

func TestParseIgnore_SkipsCommentsAndInvalidRules(t *testing.T) {
	rules := parseIgnore([]byte("# comment\n\n*.log  \r\n\\#literal\nkeep\\ \n[broken\n!important.log\n"))
	if len(rules) != 4 {
		t.Fatalf("rules = %d", len(rules))
	}
	if !rules[0].match("a.log", false) || !rules[1].match("#literal", false) || !rules[2].match("keep ", false) || !rules[3].negated {
		t.Fatalf("rules = %#v", rules)
	}
	if !strings.HasPrefix(rules[0].expression.String(), "^(?:.*/)?") {
		t.Fatalf("basename rule = %s", rules[0].expression)
	}
}

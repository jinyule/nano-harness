package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

func TestSearch_ECMAScriptBlankArguments(t *testing.T) {
	runner := &scriptedRunner{version: supported()}
	h := newHarness(t, runner)
	h.write(t, "\u0085", "x")
	for _, field := range []struct{ tool, name string }{{"glob", "pattern"}, {"glob", "path"}, {"grep", "path"}, {"grep", "include"}} {
		for _, test := range []struct {
			name, value string
			blank       bool
		}{{"BOM", "\ufeff", true}, {"NEL", "\u0085", false}, {"mixed blank", "\ufeff\t\u2028\u3000", true}} {
			t.Run(field.tool+"/"+field.name+"/"+test.name, func(t *testing.T) {
				arguments := map[string]any{"pattern": "x", field.name: test.value}
				before := len(runner.requests)
				result := h.call(t, field.tool, arguments)
				if result.IsError != test.blank || test.blank && !strings.Contains(result.Output, "must be a non-empty") {
					t.Fatalf("blank=%v: error=%v output=%q", test.blank, result.IsError, result.Output)
				}
				if test.blank && len(runner.requests) != before {
					t.Fatal("blank arguments launched ripgrep")
				}
			})
		}
	}
	for _, pattern := range []string{" ", "\ufeff", "\u0085"} {
		if result := h.call(t, "grep", map[string]any{"pattern": pattern}); result.IsError {
			t.Fatalf("nonempty regex %q rejected: %q", pattern, result.Output)
		}
	}
}

func TestGrep_ParsesFramingBeforeMatchData(t *testing.T) {
	match := `{"type":"match","data":{"path":{"text":"a.txt"},"line_number":1,"lines":{"text":"needle\n"}}}`
	for _, framing := range []string{`{"type":"begin","data":"framing"}`, `{"type":42,"data":false}`, `[]`, `{"data":"future"}`} {
		t.Run(framing, func(t *testing.T) {
			matches, err := parseMatches(framing + "\n" + match)
			if err != nil || len(matches) != 1 || matches[0].text != "needle" {
				t.Fatalf("framing lost match: matches=%v err=%v", matches, err)
			}
		})
	}
	for _, invalid := range []string{`null`, `"framing"`, `true`, `42`, `{"type":"match","data":"framing"}`} {
		t.Run(invalid, func(t *testing.T) {
			if matches, err := parseMatches(match + "\n" + invalid); err == nil || matches != nil || !strings.Contains(err.Error(), "malformed ripgrep --json output") {
				t.Fatalf("malformed record accepted or partial result returned: matches=%v err=%v", matches, err)
			}
		})
	}
}

func TestSearch_Retains65536ByteDiagnosticTail(t *testing.T) {
	restoreHooks(t)
	script := filepath.Join(t.TempDir(), "rg")
	body := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'ripgrep 15.2.0\\n'; else head -c 70000 /dev/zero | tr '\\000' x >&2; exit 2; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // the private executable fixture must be runnable by the real process runner
		t.Fatal(err)
	}
	lookPath = func(string) (string, error) { return script, nil }
	h := newHarness(t, platformProcess.New())
	result := h.call(t, "grep", map[string]any{"pattern": "x"})
	want := "Error: grep search failed (exit 2): " + strings.Repeat("x", 65536) + " [stderr truncated]"
	if !result.IsError || result.Output != want {
		t.Fatalf("diagnostic tail: got %d bytes, want %d", len(result.Output), len(want))
	}
}

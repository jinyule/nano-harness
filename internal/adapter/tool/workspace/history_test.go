package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type historyJournal struct {
	header session.Header
	events []session.Event
	err    error
}

func (journal *historyJournal) Header() session.Header { return journal.header }
func (journal *historyJournal) Events(context.Context) ([]session.Event, error) {
	return journal.events, journal.err
}
func (*historyJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

func historicalFixture(t *testing.T) (Root, string, appTool.Invocation, *historyJournal) {
	t.Helper()
	base := tempRoot(t)
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := Resolve(work)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(root.Path()))
	path := filepath.Join(base, "old spill root, with space", fmt.Sprintf("workspace-%x", sum[:8]), "session-012345abcdef", "012345abcdef-grep-results.txt")
	mustWrite(t, path)
	journal := &historyJournal{header: session.Header{SessionID: "resumed", Cwd: work}, events: []session.Event{{Record: session.Record{Type: session.RecordToolResult, Result: &session.ToolResult{
		Output: "(Full grep result stored at: " + path + ". Use read with offset/limit, or grep this path to search within it.)",
	}}}}}
	return root.WithReadOnly(filepath.Join(base, "new-spill")), path, appTool.Invocation{SessionID: "resumed", Journal: journal}, journal
}

func TestRoot_HistoricalSpillAllowDenyMatrix(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *Root, *string, *appTool.Invocation, *historyJournal)
		want   error
	}{
		{name: "recorded exact artifact"},
		{name: "fork inherited result", change: func(_ *testing.T, _ *Root, _ *string, invocation *appTool.Invocation, journal *historyJournal) {
			invocation.SessionID, journal.header.SessionID = "fork", "fork"
			journal.events = append(journal.events, session.Event{Record: session.Record{Type: session.RecordSubagentDescriptor, Subagent: &session.SubagentDescriptor{Inherited: 1}}})
		}},
		{name: "error result", change: func(_ *testing.T, _ *Root, _ *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.events[0].Record.Result.IsError = true
		}},
		{name: "compacted raw result", change: func(_ *testing.T, _ *Root, _ *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.events = append(journal.events, session.Event{Record: session.Record{Type: session.RecordCompactionSummary}})
		}},
		{name: "no journal", change: func(_ *testing.T, _ *Root, _ *string, invocation *appTool.Invocation, _ *historyJournal) {
			invocation.Journal = nil
		}, want: ErrOutsideRoot},
		{name: "no session", change: func(_ *testing.T, _ *Root, _ *string, invocation *appTool.Invocation, _ *historyJournal) {
			invocation.SessionID = ""
		}, want: ErrOutsideRoot},
		{name: "wrong session log", change: func(_ *testing.T, _ *Root, _ *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.header.SessionID = "other"
		}, want: ErrOutsideRoot},
		{name: "wrong workspace log", change: func(_ *testing.T, _ *Root, _ *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.header.Cwd = "/other"
		}, want: ErrOutsideRoot},
		{name: "unrecorded sibling", change: func(t *testing.T, _ *Root, path *string, _ *appTool.Invocation, _ *historyJournal) {
			*path = filepath.Join(filepath.Dir(*path), "abcdef012345-grep-results.txt")
			mustWrite(t, *path)
		}, want: ErrOutsideRoot},
		{name: "directory", change: func(_ *testing.T, _ *Root, path *string, _ *appTool.Invocation, _ *historyJournal) {
			*path = filepath.Dir(*path)
		}, want: ErrOutsideRoot},
		{name: "relative spelling", change: func(_ *testing.T, root *Root, path *string, _ *appTool.Invocation, _ *historyJournal) {
			*path, _ = filepath.Rel(root.Path(), *path)
		}, want: ErrOutsideRoot},
		{name: "unclean spelling", change: func(_ *testing.T, _ *Root, path *string, _ *appTool.Invocation, _ *historyJournal) {
			*path = filepath.Dir(*path) + "/../" + filepath.Base(filepath.Dir(*path)) + "/" + filepath.Base(*path)
		}, want: ErrOutsideRoot},
		{name: "ordinary text mention", change: func(_ *testing.T, _ *Root, path *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.events[0].Record.Result.Output = *path
		}, want: ErrOutsideRoot},
		{name: "user message mention", change: func(_ *testing.T, _ *Root, _ *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.events[0].Record.Type = session.RecordUserMessage
		}, want: ErrOutsideRoot},
		{name: "tool call mention", change: func(_ *testing.T, _ *Root, _ *string, _ *appTool.Invocation, journal *historyJournal) {
			journal.events[0].Record.Type = session.RecordToolCall
		}, want: ErrOutsideRoot},
		{name: "foreign workspace partition", change: func(t *testing.T, _ *Root, path *string, _ *appTool.Invocation, journal *historyJournal) {
			*path = filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(*path))), "workspace-0000000000000000", "session-012345abcdef", filepath.Base(*path))
			mustWrite(t, *path)
			journal.events[0].Record.Result.Output = "Full grep result stored at: " + *path + ". Use read."
		}, want: ErrOutsideRoot},
		{name: "ordinary filename", change: func(_ *testing.T, _ *Root, path *string, _ *appTool.Invocation, journal *historyJournal) {
			*path = filepath.Join(filepath.Dir(*path), "secret.txt")
			journal.events[0].Record.Result.Output = "Full grep result stored at: " + *path + ". Use read."
		}, want: ErrOutsideRoot},
		{name: "wrong session layout", change: func(_ *testing.T, _ *Root, path *string, _ *appTool.Invocation, journal *historyJournal) {
			*path = filepath.Join(filepath.Dir(filepath.Dir(*path)), "session-x", filepath.Base(*path))
			journal.events[0].Record.Result.Output = "Full grep result stored at: " + *path + ". Use read."
		}, want: ErrOutsideRoot},
		{name: "removed artifact", change: func(t *testing.T, _ *Root, path *string, _ *appTool.Invocation, _ *historyJournal) {
			if err := os.Remove(*path); err != nil {
				t.Fatal(err)
			}
		}, want: fs.ErrNotExist},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, path, invocation, journal := historicalFixture(t)
			if test.change != nil {
				test.change(t, &root, &path, &invocation, journal)
			}
			lexical, resolved, err := root.ReadableFrom(t.Context(), path, invocation)
			if !matches(err, test.want) || test.want == nil && (lexical != path || resolved != path) {
				t.Fatalf("read = %q %q %v, want %v", lexical, resolved, err, test.want)
			}
			if _, err := root.Writable(path); !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("write allowance leaked: %v", err)
			}
			if _, _, err := root.Existing(path); !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("glob/workdir allowance leaked: %v", err)
			}
		})
	}
}

func TestRoot_HistoricalSpillRejectsUnsafeFilesAndDirectories(t *testing.T) {
	for level := range 4 {
		for _, unsafe := range []string{"symlink", "public", "wrong type"} {
			t.Run(fmt.Sprintf("level-%d/%s", level, unsafe), func(t *testing.T) {
				root, path, invocation, _ := historicalFixture(t)
				target := path
				for range level {
					target = filepath.Dir(target)
				}
				want := ErrOutsideRoot
				switch unsafe {
				case "symlink":
					moved := target + "-original"
					if err := os.Rename(target, moved); err != nil {
						t.Fatal(err)
					}
					mustLink(t, moved, target)
					want = ErrSymlink
				case "public":
					if runtime.GOOS == "windows" {
						t.Skip("Unix permission bits")
					}
					if err := os.Chmod(target, 0o755); err != nil { //nolint:gosec // deliberately public permissions prove historical artifacts are rejected
						t.Fatal(err)
					}
				case "wrong type":
					if err := os.RemoveAll(target); err != nil {
						t.Fatal(err)
					}
					if level == 0 {
						if err := os.Mkdir(target, 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						mustWrite(t, target)
						want = nil // A missing child/ENOTDIR is still a denial.
					}
				}
				_, resolved, err := root.ReadableFrom(t.Context(), path, invocation)
				if err == nil || resolved != "" || want != nil && !errors.Is(err, want) {
					t.Fatalf("unsafe target accepted: %q %v, want %v", resolved, err, want)
				}
			})
		}
	}
}

func TestRoot_HistoricalSpillPropagatesFailuresAndKeepsNormalReads(t *testing.T) {
	restoreHooks(t)
	root, path, invocation, journal := historicalFixture(t)
	mustWrite(t, filepath.Join(root.Path(), "normal.txt"))
	if _, _, err := root.ReadableFrom(t.Context(), "normal.txt", appTool.Invocation{}); err != nil {
		t.Fatal(err)
	}
	journal.err = context.Canceled
	if _, _, err := root.ReadableFrom(t.Context(), path, invocation); !errors.Is(err, context.Canceled) {
		t.Fatalf("log error: %v", err)
	}
	journal.err = nil
	failure := errors.New("lstat denied")
	lstatPath = func(string) (os.FileInfo, error) { return nil, failure }
	if _, _, err := root.ReadableFrom(t.Context(), path, invocation); !errors.Is(err, failure) {
		t.Fatalf("lstat error: %v", err)
	}
	lstatPath = os.Lstat
	resolveLinks = func(string) (string, error) { return "", failure }
	if _, _, err := root.ReadableFrom(t.Context(), path, invocation); !errors.Is(err, failure) {
		t.Fatalf("resolve error: %v", err)
	}
}

func TestMentionsSpill_ExactFooterBoundaries(t *testing.T) {
	path := "/root, with spaces/artifact.txt"
	for _, text := range []string{
		"Full formatted result stored at: " + path + ". Use read.",
		"Full sorted result stored at: " + path + ". Use read.",
		"Full grep result stored at: " + path + ". Use read.",
		"[output truncated; full output: " + path + "]",
		"[some output was dropped from memory; full output: /first, " + path + "]",
		"[output truncated; full output: /first]\n[stderr]\n[output truncated; full output: " + path + "]",
	} {
		if !mentionsSpill(text, path) {
			t.Errorf("footer not recognized: %q", text)
		}
	}
	for _, text := range []string{path, "Full grep result stored at: " + path + "-sibling. Use read.", "[output truncated; full output: " + path + "-sibling]"} {
		if mentionsSpill(text, path) {
			t.Errorf("non-locator mention accepted: %q", text)
		}
	}
}

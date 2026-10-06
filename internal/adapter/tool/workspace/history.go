package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	historicalSession = regexp.MustCompile(`^session-[0-9a-f]{12}$`)
	historicalFile    = regexp.MustCompile(`^[0-9a-f]{12}-[A-Za-z0-9._-]{1,64}$`)
)

// spillJournal is the calling session's committed log, including fork seeds
// and records hidden by compaction. Append-only callers grant no history.
type spillJournal interface {
	Header() session.Header
	Events(context.Context) ([]session.Event, error)
}

// ReadableFrom applies the standing file policy: full access allows host
// reads; otherwise Readable also admits exact historical spill files named
// in committed tool results, with no directories or sibling allowances.
// A missing log grants no history; an unreadable log fails the read.
func (root Root) ReadableFrom(ctx context.Context, path string, invocation appTool.Invocation) (lexical, resolved string, err error) {
	var events []session.Event
	if journal, ok := invocation.Journal.(interface {
		Events(context.Context) ([]session.Event, error)
	}); ok {
		events, err = journal.Events(ctx)
		if err != nil {
			return "", "", fmt.Errorf("read file policy for %s: %w", invocation.SessionID, err)
		}
		if mode := session.EffectiveSandbox(events); mode == session.SandboxDangerFullAccess {
			return root.ExistingIn(path, mode)
		}
	}
	lexical, resolved, err = root.Readable(path)
	if !errors.Is(err, ErrOutsideRoot) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return lexical, resolved, err
	}
	journal, ok := invocation.Journal.(spillJournal)
	if !ok || invocation.SessionID == "" || journal.Header().SessionID != invocation.SessionID || journal.Header().Cwd != root.path || !root.spillShape(path) {
		return lexical, resolved, err
	}
	for _, event := range events {
		if event.Record.Type == session.RecordToolResult && mentionsSpill(event.Record.Result.Output, path) {
			return root.historicalFile(path)
		}
	}
	return lexical, resolved, err
}

// spillShape requires the current workspace partition and the store's exact
// session/file naming layout, independently of the configured write root.
func (root Root) spillShape(path string) bool {
	sum := sha256.Sum256([]byte(root.path))
	dir := filepath.Dir(path)
	return historicalFile.MatchString(filepath.Base(path)) && historicalSession.MatchString(filepath.Base(dir)) &&
		filepath.Base(filepath.Dir(dir)) == fmt.Sprintf("workspace-%x", sum[:8])
}

// mentionsSpill recognizes the locator boundaries in spill/search/shell/job
// footers. Plain text, call arguments and user messages grant no allowance.
func mentionsSpill(text, path string) bool {
	for _, prefix := range []string{"Full formatted result stored at: ", "Full sorted result stored at: ", "Full grep result stored at: "} {
		if strings.Contains(text, prefix+path+". ") {
			return true
		}
	}
	for _, output := range strings.Split(text, "full output: ")[1:] {
		list, _, _ := strings.Cut(output, "]")
		if strings.Contains(", "+list+", ", ", "+path+", ") {
			return true
		}
	}
	return false
}

func (root Root) historicalFile(path string) (string, string, error) {
	// Check the spill root, partition, session and file. Ancestors of the
	// root may use OS aliases (such as macOS /var); none of these four may link.
	current := path
	for index := range 4 {
		info, err := lstatPath(current)
		if err != nil {
			return path, "", fmt.Errorf("inspect historical spill %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return path, "", fmt.Errorf("%w: %s", ErrSymlink, current)
		}
		if index == 0 && !info.Mode().IsRegular() || index > 0 && !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return path, "", fmt.Errorf("%w: historical spill %s must be private with the expected file type", ErrOutsideRoot, current)
		}
		current = filepath.Dir(current)
	}
	resolved, err := resolveLinks(path)
	if err != nil {
		return path, "", fmt.Errorf("resolve historical spill %s: %w", path, err)
	}
	return path, resolved, nil
}

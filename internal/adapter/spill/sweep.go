package spill

import (
	"context"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

// sweep is the best-effort startup cleanup. Under root it visits only
// private workspace-<hash>/session-<hash> directories, deletes regular files
// modified strictly before cutoff, prunes session directories it emptied,
// and prunes other workspaces' partitions once empty; the active partition
// stays because this process writes into it. Symbolic links and unrelated
// entries are never followed or deleted. Failures are contained: an entry
// that cannot be inspected or removed is left for the next start. layout is
// held while a session directory is pruned, so a create in this process never
// runs between its mkdir and its open on a directory being removed; another
// process's create recreates a pruned directory and retries.
func sweep(ctx context.Context, root, active string, cutoff time.Time, layout sync.Locker) {
	for _, workspace := range privateDirectories(ctx, root, workspaceEntry) {
		emptied := true
		for _, session := range privateDirectories(ctx, workspace, sessionEntry) {
			if !sweepSession(ctx, session, cutoff) || prune(session, layout) != nil {
				emptied = false
			}
		}
		if workspace != active && emptied {
			_ = removePath(workspace) // fails harmlessly when unrelated entries or new artifacts remain
		}
	}
}

// prune removes an empty directory while holding layout.
func prune(dir string, layout sync.Locker) error {
	layout.Lock()
	defer layout.Unlock()
	return removePath(dir)
}

// privateDirectories lists the children of dir whose names match pattern and
// that are real directories readable only by the owner.
func privateDirectories(ctx context.Context, dir string, pattern *regexp.Regexp) []string {
	entries, err := readDir(dir)
	if err != nil {
		return nil
	}
	var matches []string
	for _, entry := range entries {
		if ctx.Err() != nil {
			return matches
		}
		path := filepath.Join(dir, entry.Name())
		if !pattern.MatchString(entry.Name()) {
			continue
		}
		info, err := lstatPath(path)
		if err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			continue
		}
		matches = append(matches, path)
	}
	return matches
}

// sweepSession deletes expired regular files in one session directory and
// reports whether the directory is now empty.
func sweepSession(ctx context.Context, dir string, cutoff time.Time) bool {
	entries, err := readDir(dir)
	if err != nil {
		return false
	}
	remaining := len(entries)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return false
		}
		path := filepath.Join(dir, entry.Name())
		info, err := lstatPath(path)
		if err != nil || info.Mode()&fs.ModeType != 0 || !info.ModTime().Before(cutoff) {
			continue
		}
		if removePath(path) == nil {
			remaining--
		}
	}
	return remaining == 0
}

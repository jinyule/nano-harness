package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// maxIgnoreFileBytes bounds one ignore file loaded during traversal.
const maxIgnoreFileBytes = 1 << 20

// ignoreFiles lists per-directory ignore files from highest to lowest
// precedence, as ripgrep reads them. The last entry applies only inside a
// Git repository.
var ignoreFiles = []string{".rgignore", ".ignore", ".gitignore"}

// vcsDirectories are never listed by glob, matching upstream's exclusions.
var vcsDirectories = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// ignoreSet holds the rules loaded from one directory. Within one file the
// last matching rule wins; across files the earlier list takes precedence.
type ignoreSet struct {
	directory string
	rules     [][]pattern
}

// decide matches target, which is always a descendant of the set's directory.
func (set ignoreSet) decide(target string, isDir bool) (matched, ignored bool) {
	local := filepath.ToSlash(strings.TrimPrefix(target, set.directory+string(filepath.Separator)))
	for _, rules := range set.rules {
		for _, rule := range slices.Backward(rules) {
			if rule.match(local, isDir) {
				return true, !rule.negated
			}
		}
	}
	return false, false
}

// walker traverses one resolved directory without following symbolic links
// and hands every selected regular file to visit. Display paths are
// workspace-relative with forward slashes.
type walker struct {
	ctx context.Context
	// filtered applies ripgrep's default hidden-file and ignore-file rules.
	filtered bool
	// include whitelists matching files and is the only file filter when set.
	include *pattern
	// prune skips directories matched by this negated glob and VCS metadata.
	prune func(display string) bool
	visit func(resolved, display string, entry fs.DirEntry) error
}

// start loads the ignore rules of every directory from the workspace root
// down to the parent of the traversal start, then walks the start itself.
func (walk *walker) start(root, resolved, display string) error {
	var sets []ignoreSet
	inRepository := false
	if walk.filtered {
		inRepository = gitAbove(filepath.Dir(root))
		directory := root
		for part := range strings.SplitSeq(filepath.ToSlash(strings.TrimPrefix(resolved, root)), "/") {
			if part == "" {
				continue
			}
			set, repository, err := loadIgnoreSet(directory, inRepository)
			if err != nil {
				return err
			}
			sets, inRepository = append(sets, set), repository
			directory = filepath.Join(directory, part)
		}
	}
	return walk.directory(resolved, display, sets, inRepository)
}

func (walk *walker) directory(resolved, display string, sets []ignoreSet, inRepository bool) error {
	if err := walk.ctx.Err(); err != nil {
		return err
	}
	if walk.filtered {
		set, repository, err := loadIgnoreSet(resolved, inRepository)
		if err != nil {
			return err
		}
		sets, inRepository = append(slices.Clip(sets), set), repository
	}
	entries, err := readDirectory(resolved)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childResolved := filepath.Join(resolved, entry.Name())
		childDisplay := path.Join(display, entry.Name())
		if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			continue
		}
		if !walk.selected(entry, childResolved, childDisplay, sets) {
			continue
		}
		var err error
		if entry.IsDir() {
			err = walk.directory(childResolved, childDisplay, sets, inRepository)
		} else {
			err = walk.visit(childResolved, childDisplay, entry)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// selected applies ripgrep's precedence: an include match whitelists the
// entry, otherwise hidden names and ignore rules exclude it; directories are
// also pruned by the walker's own rule, and files must match an include.
func (walk *walker) selected(entry fs.DirEntry, resolved, display string, sets []ignoreSet) bool {
	whitelisted := walk.include != nil && walk.include.match(display, entry.IsDir())
	if walk.filtered && !whitelisted && (strings.HasPrefix(entry.Name(), ".") || ignored(sets, resolved, entry.IsDir())) {
		return false
	}
	if entry.IsDir() {
		return walk.prune == nil || !walk.prune(display)
	}
	return walk.include == nil || whitelisted
}

func ignored(sets []ignoreSet, target string, isDir bool) bool {
	for _, set := range slices.Backward(sets) {
		if matched, ignore := set.decide(target, isDir); matched {
			return ignore
		}
	}
	return false
}

// loadIgnoreSet reads one directory's ignore files and reports whether the
// directory is inside a Git repository, where .gitignore applies.
func loadIgnoreSet(directory string, inRepository bool) (ignoreSet, bool, error) {
	if _, err := lstatPath(filepath.Join(directory, ".git")); err == nil {
		inRepository = true
	}
	set := ignoreSet{directory: directory}
	for _, name := range ignoreFiles {
		if name == ".gitignore" && !inRepository {
			continue
		}
		data, err := readIgnoreFile(filepath.Join(directory, name))
		if err != nil {
			return ignoreSet{}, false, err
		}
		set.rules = append(set.rules, parseIgnore(data))
	}
	return set, inRepository, nil
}

func readIgnoreFile(name string) ([]byte, error) {
	info, err := lstatPath(name)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > maxIgnoreFileBytes {
		return nil, fmt.Errorf("ignore file %s exceeds %d bytes", name, maxIgnoreFileBytes)
	}
	return readFile(name)
}

// parseIgnore compiles gitignore lines; invalid patterns are skipped like Git.
func parseIgnore(data []byte) []pattern {
	var rules []pattern
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasSuffix(line, `\ `) {
			line = strings.TrimRight(line, " ")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rule, err := compilePattern(line); err == nil {
			rules = append(rules, rule)
		}
	}
	return rules
}

// gitAbove reports whether directory or one of its ancestors holds .git. It
// only probes metadata; ignore files above the workspace are never read.
func gitAbove(directory string) bool {
	for {
		if _, err := lstatPath(filepath.Join(directory, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return false
		}
		directory = parent
	}
}

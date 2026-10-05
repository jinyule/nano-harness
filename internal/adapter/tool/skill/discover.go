package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	coreskill "github.com/jinyule/nano-harness/internal/core/skill"
	"gopkg.in/yaml.v3"
)

const (
	// maxRootEntries bounds the directory entries examined in one root.
	maxRootEntries = 1024
	// maxSkillBytes bounds one instruction file, so a loaded body and its
	// wrapper always fit one tool result.
	maxSkillBytes = 128 << 10
	// instructionFile is the instruction file of a directory bundle.
	instructionFile = "SKILL.md"
)

var (
	statPath    = os.Stat
	lstatPath   = os.Lstat
	openPath    = os.Open
	readEntries = func(directory *os.File, limit int) ([]fs.DirEntry, error) { return directory.ReadDir(limit) }
	statFile    = func(file *os.File) (fs.FileInfo, error) { return file.Stat() }
)

// root is one scanned skill directory. Earlier roots win duplicate names.
type root struct {
	path       string
	skipSystem bool
}

// summary is one winning discovered skill. file is the instruction file and
// directory the base for its relative resources, both as discovered.
type summary struct {
	name        string
	description string
	model       bool
	user        bool
	file        string
	directory   string
}

// definition is one parsed instruction file.
type definition struct {
	name        string
	description string
	model       bool
	user        bool
	body        string
}

// roots lists the scan order: the project roots of the nearest ancestor of
// the workspace that holds a .git entry (or of the workspace itself), then
// the user root, then the shared agents root.
func (provider *Provider) roots() []root {
	project := provider.config.Workspace
	for current := project; ; current = filepath.Dir(current) {
		if _, err := statPath(filepath.Join(current, ".git")); err == nil {
			project = current
			break
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return []root{
		{path: filepath.Join(project, ".nano-harness", "skills")},
		{path: filepath.Join(project, ".agents", "skills")},
		{path: provider.config.UserDir, skipSystem: true},
		{path: provider.config.AgentsDir},
	}
}

// discover returns the winning skills sorted by name. A missing root is
// empty; any other I/O failure or an over-limit catalog makes the
// observation incomplete and is returned as an error. Invalid skills are
// skipped.
func (provider *Provider) discover(ctx context.Context) ([]summary, error) {
	var winners []summary
	for _, candidate := range provider.roots() {
		found, err := scanRoot(ctx, candidate)
		if err != nil {
			return nil, err
		}
		for _, skill := range found {
			if !slices.ContainsFunc(winners, func(winner summary) bool { return winner.name == skill.name }) {
				winners = append(winners, skill)
			}
		}
	}
	if len(winners) > coreskill.MaxCatalogEntries {
		return nil, fmt.Errorf("discover skills: %d skills exceed the limit of %d", len(winners), coreskill.MaxCatalogEntries)
	}
	slices.SortFunc(winners, func(left, right summary) int { return strings.Compare(left.name, right.name) })
	return winners, nil
}

// scanRoot reads the direct entries of one root in byte order: a directory
// bundle contributes <name>/SKILL.md and a regular <name>.md file contributes
// itself. Symbolic links and special files are never followed.
func scanRoot(ctx context.Context, candidate root) ([]summary, error) {
	directory, err := openPath(candidate.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open skill root: %w", err)
	}
	defer func() { _ = directory.Close() }() // read-only handle; nothing to flush
	entries, err := readEntries(directory, maxRootEntries+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read skill root %s: %w", candidate.path, err)
	}
	if len(entries) > maxRootEntries {
		return nil, fmt.Errorf("read skill root %s: more than %d entries", candidate.path, maxRootEntries)
	}
	slices.SortFunc(entries, func(left, right fs.DirEntry) int { return strings.Compare(left.Name(), right.Name()) })
	var found []summary
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, base, ok := candidate.locate(entry)
		if !ok {
			continue
		}
		parsed, ok, err := readSkill(file)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, summary{name: parsed.name, description: parsed.description, model: parsed.model, user: parsed.user, file: file, directory: base})
		}
	}
	return found, nil
}

// locate maps one root entry to its instruction file and resource base. The
// entry type comes from the directory listing, so symbolic links are neither
// bundles nor flat files.
func (candidate root) locate(entry fs.DirEntry) (file, base string, ok bool) {
	name := entry.Name()
	switch {
	case candidate.skipSystem && name == ".system":
		return "", "", false
	case entry.IsDir():
		return filepath.Join(candidate.path, name, instructionFile), filepath.Join(candidate.path, name), true
	case entry.Type().IsRegular() && strings.HasSuffix(name, ".md"):
		return filepath.Join(candidate.path, name), candidate.path, true
	default:
		return "", "", false
	}
}

// readSkill reads and parses one instruction file. A missing file, a
// symbolic link, a special or oversized file, binary or invalid UTF-8 text,
// and invalid frontmatter report false; other I/O failures are errors.
func readSkill(path string) (definition, bool, error) {
	info, err := lstatPath(path)
	if errors.Is(err, fs.ErrNotExist) {
		return definition{}, false, nil
	}
	if err != nil {
		return definition{}, false, fmt.Errorf("inspect skill file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxSkillBytes {
		return definition{}, false, nil
	}
	file, err := openPath(path)
	if errors.Is(err, fs.ErrNotExist) {
		return definition{}, false, nil
	}
	if err != nil {
		return definition{}, false, fmt.Errorf("open skill file: %w", err)
	}
	defer func() { _ = file.Close() }() // read-only handle; nothing to flush
	opened, err := statFile(file)
	if err != nil {
		return definition{}, false, fmt.Errorf("inspect skill file: %w", err)
	}
	if !os.SameFile(info, opened) {
		return definition{}, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSkillBytes+1))
	if err != nil {
		return definition{}, false, fmt.Errorf("read skill file: %w", err)
	}
	if len(data) > maxSkillBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return definition{}, false, nil
	}
	parsed, ok := parseSkill(string(data))
	return parsed, ok, nil
}

// parseSkill splits YAML frontmatter delimited by "---" lines from the body
// and validates the upstream fields: required kebab-case name and non-empty
// description, plus the optional disable-model-invocation and user-invocable
// switches. Legacy camel-case switches invalidate the skill rather than being
// silently ignored.
func parseSkill(raw string) (definition, bool) {
	header, body, ok := splitFrontmatter(raw)
	if !ok {
		return definition{}, false
	}
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(header), &fields); err != nil || fields == nil {
		return definition{}, false
	}
	name, nameOK := fields["name"].(string)
	description, descriptionOK := fields["description"].(string)
	if !nameOK || !descriptionOK || description == "" || !coreskill.ValidName(name) {
		return definition{}, false
	}
	for _, legacy := range []string{"disableModelInvocation", "modelInvocable", "userInvocable"} {
		if _, present := fields[legacy]; present {
			return definition{}, false
		}
	}
	disabled, disabledOK := frontmatterBoolean(fields, "disable-model-invocation", false)
	user, userOK := frontmatterBoolean(fields, "user-invocable", true)
	if !disabledOK || !userOK {
		return definition{}, false
	}
	return definition{name: name, description: description, model: !disabled, user: user, body: coreskill.TrimSpace(body)}, true
}

// splitFrontmatter returns the text between an opening "---" first line and
// the next "---" line, and the body after it. Lines may end in CRLF.
func splitFrontmatter(raw string) (header, body string, ok bool) {
	first, rest, found := strings.Cut(raw, "\n")
	if !found || strings.TrimSuffix(first, "\r") != "---" {
		return "", "", false
	}
	for offset := 0; ; {
		line, after, more := strings.Cut(rest[offset:], "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			return rest[:offset], after, true
		}
		if !more {
			return "", "", false
		}
		offset += len(line) + 1
	}
}

// frontmatterBoolean reads an optional switch with the upstream grammar:
// YAML booleans, the numbers 1 and 0, and the case-insensitive strings
// true/yes/on/1 and false/no/off/0. Any other value is invalid.
func frontmatterBoolean(fields map[string]any, key string, fallback bool) (bool, bool) {
	value, present := fields[key]
	if !present {
		return fallback, true
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case int:
		return typed == 1, typed == 1 || typed == 0
	case float64:
		return typed == 1, typed == 1 || typed == 0
	case string:
		switch strings.ToLower(typed) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

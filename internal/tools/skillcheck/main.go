// Command skillcheck validates repository-local agent skills without external dependencies.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxSkillNameLength  = 64
	maxDescriptionRunes = 1024
	minShortDescription = 25
	maxShortDescription = 64
)

var (
	skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	todoPattern      = regexp.MustCompile(`(?m)^[ \t]*\[TODO:[^\n]*\][ \t]*$`)
	markdownLink     = regexp.MustCompile(`\[[^\]\n]+\]\(([^)\n]+)\)`)
)

func main() {
	count, err := validateSkills(".agents/skills")
	if err != nil {
		fmt.Fprintf(os.Stderr, "skills: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("skills: %d repository skills valid\n", count)
}

func validateSkills(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read root: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return 0, errors.New("no skill directories found")
	}

	repositoryRoot := filepath.Clean(filepath.Join(root, "..", ".."))
	var failures []error
	for _, name := range names {
		if err := validateSkill(filepath.Join(root, name), repositoryRoot); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
	}
	return len(names), errors.Join(failures...)
}

func validateSkill(directory, repositoryRoot string) error {
	skillPath := filepath.Join(directory, "SKILL.md")
	content, err := os.ReadFile(skillPath) //nolint:gosec // The path is rooted in the repository skill directory.
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}
	frontmatter, body, err := parseFrontmatter(string(content))
	if err != nil {
		return err
	}
	if len(frontmatter) != 2 {
		return errors.New("frontmatter must contain only name and description")
	}

	name, hasName := frontmatter["name"]
	description, hasDescription := frontmatter["description"]
	if !hasName || !hasDescription {
		return errors.New("frontmatter requires name and description")
	}
	if !skillNamePattern.MatchString(name) || len(name) > maxSkillNameLength {
		return fmt.Errorf("invalid skill name %q", name)
	}
	if name != filepath.Base(directory) {
		return fmt.Errorf("frontmatter name %q does not match directory", name)
	}
	if description == "" || utf8.RuneCountInString(description) > maxDescriptionRunes {
		return errors.New("description must contain 1 through 1024 characters")
	}
	if strings.ContainsAny(description, "<>") {
		return errors.New("description cannot contain angle brackets")
	}
	if todoPattern.MatchString(body) {
		return errors.New("SKILL.md contains an unfinished TODO placeholder")
	}

	if err := validateOpenAI(filepath.Join(directory, "agents", "openai.yaml"), name); err != nil {
		return err
	}
	if err := validateMarkdownLinks(directory, repositoryRoot); err != nil {
		return err
	}
	return nil
}

func parseFrontmatter(content string) (map[string]string, string, error) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, "", errors.New("SKILL.md must start with YAML frontmatter")
	}
	remainder := strings.TrimPrefix(content, "---\n")
	frontmatterText, body, found := strings.Cut(remainder, "\n---\n")
	if !found {
		return nil, "", errors.New("SKILL.md has no closing frontmatter marker")
	}

	values := make(map[string]string)
	for line := range strings.SplitSeq(frontmatterText, "\n") {
		key, value, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, "", fmt.Errorf("invalid frontmatter line %q", line)
		}
		if _, exists := values[key]; exists {
			return nil, "", fmt.Errorf("duplicate frontmatter key %q", key)
		}
		parsed, err := parseScalar(value)
		if err != nil {
			return nil, "", fmt.Errorf("parse frontmatter %s: %w", key, err)
		}
		values[key] = parsed
	}
	return values, body, nil
}

func parseScalar(value string) (string, error) {
	if !strings.HasPrefix(value, `"`) {
		return value, nil
	}
	parsed, err := strconv.Unquote(value)
	if err != nil {
		return "", err
	}
	return parsed, nil
}

func validateOpenAI(path, name string) error {
	content, err := os.ReadFile(path) //nolint:gosec // The path is derived from one validated repository skill directory.
	if err != nil {
		return fmt.Errorf("read agents/openai.yaml: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != 4 || lines[0] != "interface:" {
		return errors.New("agents/openai.yaml must contain the three supported interface fields")
	}
	displayName, err := quotedField(lines[1], "  display_name: ")
	if err != nil || displayName == "" {
		return errors.New("agents/openai.yaml has an invalid display_name")
	}
	shortDescription, err := quotedField(lines[2], "  short_description: ")
	if err != nil {
		return errors.New("agents/openai.yaml has an invalid short_description")
	}
	length := utf8.RuneCountInString(shortDescription)
	if length < minShortDescription || length > maxShortDescription {
		return fmt.Errorf("short_description must contain 25 through 64 characters, got %d", length)
	}
	defaultPrompt, err := quotedField(lines[3], "  default_prompt: ")
	if err != nil || !strings.Contains(defaultPrompt, "$"+name) {
		return fmt.Errorf("default_prompt must mention $%s", name)
	}
	return nil
}

func quotedField(line, prefix string) (string, error) {
	if !strings.HasPrefix(line, prefix) {
		return "", errors.New("unexpected field")
	}
	return strconv.Unquote(strings.TrimPrefix(line, prefix))
}

func validateMarkdownLinks(directory, repositoryRoot string) error {
	return filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		content, err := os.ReadFile(path) //nolint:gosec // WalkDir constrains the path to the current skill directory.
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatchIndex(string(content), -1) {
			raw := strings.TrimSpace(string(content)[match[2]:match[3]])
			target := strings.Trim(raw, "<>")
			if separator := strings.IndexAny(target, " \t"); separator >= 0 {
				target = target[:separator]
			}
			if skipLink(target) {
				continue
			}
			if marker := strings.IndexAny(target, "?#"); marker >= 0 {
				target = target[:marker]
			}
			decoded, err := url.PathUnescape(target)
			if err != nil {
				return fmt.Errorf("%s: invalid link %q", path, target)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(decoded)))
			within, err := filepath.Rel(repositoryRoot, resolved)
			if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%s: link escapes repository: %s", path, target)
			}
			if _, err := os.Stat(resolved); err != nil {
				line := 1 + strings.Count(string(content)[:match[0]], "\n")
				return fmt.Errorf("%s:%d: broken link %s", path, line, target)
			}
		}
		return nil
	})
}

func skipLink(target string) bool {
	return target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "https://") ||
		strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "mailto:")
}

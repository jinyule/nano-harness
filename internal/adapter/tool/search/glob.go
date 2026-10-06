package search

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

// globMaxResults is the inline path cap of one glob call.
const globMaxResults = 100

// vcsDirectories are excluded from glob listings, matching upstream.
var vcsDirectories = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

type globArgs struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
}

func (provider *Provider) globTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[globArgs]{
		Name: "glob",
		Description: "Find files, not directories, whose paths match a glob pattern, including hidden and ignored files. " +
			fmt.Sprintf("Returns up to %d paths in modification-time order; a larger result keeps the first paths ", globMaxResults) +
			"and reports where the complete list was saved.",
		Parameters: appTool.Parameters{
			appTool.Required("pattern", appTool.String("Glob pattern to match file paths against (e.g. \"**/*.ts\", \"src/**/*.test.js\"). "+
				"A pattern with no \"/\" matches the basename at any depth, so \"*\" and \"*.ts\" both search the whole tree; include a separator to anchor the depth.")),
			appTool.Optional("path", appTool.String("Directory to search in. Defaults to the session workspace; a relative path resolves against it.")),
		},
		Guidance: appTool.StaticGuidance(appTool.OrderGlob, "Use the glob tool — not shell find — to discover files by path pattern."),
		Check: func(_ context.Context, _ appTool.Invocation, arguments globArgs) error {
			if appTool.IsBlank(arguments.Pattern) {
				return errors.New("pattern must be a non-empty string")
			}
			if arguments.Path != nil && appTool.IsBlank(*arguments.Path) {
				return errors.New("path must be a non-empty string when given")
			}
			return nil
		},
		Concurrent: func(globArgs) bool { return true },
		Execute:    provider.glob,
	})
}

// glob runs `rg --files --sort=modified --no-ignore --hidden` with upstream's
// VCS exclusions, so paths arrive oldest modification first.
func (provider *Provider) glob(ctx context.Context, invocation appTool.Invocation, arguments globArgs) (appTool.Result, error) {
	start, err := provider.locate(ctx, invocation, "glob", arguments.Path, false)
	if err != nil {
		return appTool.Result{}, err
	}
	command := []string{"--files", "--glob=" + arguments.Pattern, "--sort=modified", "--no-ignore", "--hidden"}
	for _, name := range vcsDirectories {
		// The bare form prunes the directory; the contents form still applies
		// when the search root is at or inside it.
		command = append(command, "--glob=!**/"+name, "--glob=!**/"+name+"/**")
	}
	stdout, empty, err := provider.run(ctx, "glob", append(command, start.arguments()...))
	if err != nil {
		return appTool.Result{}, err
	}
	var paths []string
	if !empty {
		paths = strings.FieldsFunc(stdout, func(char rune) bool { return char == '\n' })
	}
	if len(paths) <= globMaxResults {
		return appTool.Text(renderGlob(paths, nil)), nil
	}
	ref, err := invocation.SaveText(ctx, "glob-results.txt", strings.Join(paths, "\n"))
	if err != nil {
		// Like upstream, an unsaved complete result changes the footer, not
		// the outcome of the search.
		return appTool.Text(renderGlob(paths, nil)), nil
	}
	return appTool.Text(renderGlob(paths, &ref)), nil
}

// renderGlob shows a result that fits whole; a larger one keeps the first
// paths and points at the saved complete list, or says it could not be saved.
func renderGlob(paths []string, ref *appTool.SpillRef) string {
	if len(paths) == 0 {
		return "No files found"
	}
	if len(paths) <= globMaxResults {
		return strings.Join(paths, "\n")
	}
	recovery := "The complete result could not be saved; narrow pattern or path to see more."
	if ref != nil {
		recovery = fmt.Sprintf("Full sorted result stored at: %s. %s", ref.Locator, ref.Hint)
	}
	return fmt.Sprintf("%s\n\n(Showing %d of %d paths. %s)", strings.Join(paths[:globMaxResults], "\n"), globMaxResults, len(paths), recovery)
}

package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// globMaxResults is the inline path cap of one glob call.
	globMaxResults = 100
	// rawOutputMaxBytes bounds the complete matched path list.
	rawOutputMaxBytes = 20_000_000
)

type globArgs struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
}

type globMatch struct {
	display  string
	modified time.Time
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
		Check: func(arguments globArgs) error {
			if strings.TrimSpace(arguments.Pattern) == "" {
				return errors.New("pattern must be a non-empty string")
			}
			if arguments.Path != nil && strings.TrimSpace(*arguments.Path) == "" {
				return errors.New("path must be a non-empty string when given")
			}
			return nil
		},
		Concurrent: func(globArgs) bool { return true },
		Execute:    provider.glob,
	})
}

// glob lists regular files whose workspace-relative path matches the
// pattern, oldest modification first, like `rg --files --sort=modified
// --no-ignore --hidden` with VCS metadata excluded.
func (provider *Provider) glob(ctx context.Context, _ appTool.Invocation, arguments globArgs) (appTool.Result, error) {
	compiled, err := compilePattern(arguments.Pattern)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("glob pattern rejected: %w", err)
	}
	start, err := provider.locate("glob", arguments.Path)
	if err != nil {
		return appTool.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	var matches []globMatch
	total := 0
	accept := func(display string, entry fs.DirEntry) error {
		if slices.ContainsFunc(strings.Split(display, "/"), func(part string) bool { return slices.Contains(vcsDirectories, part) }) {
			return nil
		}
		if compiled.match(display, false) == compiled.negated {
			return nil
		}
		info, err := fileInfo(entry)
		if err != nil {
			return err
		}
		if total += len(display) + 1; total > provider.rawLimit {
			return fmt.Errorf("glob produced more than %d bytes of raw output; narrow pattern, path, or include and retry", provider.rawLimit)
		}
		matches = append(matches, globMatch{display: display, modified: info.ModTime()})
		return nil
	}
	if start.info.IsDir() {
		walk := walker{
			ctx: ctx,
			prune: func(display string) bool {
				return slices.Contains(vcsDirectories, path.Base(display)) || compiled.negated && compiled.match(display, true)
			},
			visit: func(_, display string, entry fs.DirEntry) error { return accept(display, entry) },
		}
		err = walk.directory(start.resolved, start.display, nil, false)
	} else {
		err = accept(start.display, fs.FileInfoToDirEntry(start.info))
	}
	if err != nil {
		return appTool.Result{}, aborted("glob", err)
	}
	slices.SortFunc(matches, func(left, right globMatch) int {
		if order := left.modified.Compare(right.modified); order != 0 {
			return order
		}
		return strings.Compare(left.display, right.display)
	})
	return appTool.Text(renderGlob(matches)), nil
}

func renderGlob(matches []globMatch) string {
	if len(matches) == 0 {
		return "No files found"
	}
	paths := make([]string, min(len(matches), globMaxResults))
	for index := range paths {
		paths[index] = matches[index].display
	}
	body := strings.Join(paths, "\n")
	if len(matches) <= globMaxResults {
		return body
	}
	return fmt.Sprintf("%s\n\n(Showing %d of %d paths. The complete result could not be saved; narrow pattern or path to see more.)", body, len(paths), len(matches))
}

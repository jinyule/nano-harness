package search

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

const (
	// rawOutputMaxBytes bounds the complete ripgrep stdout one call parses.
	rawOutputMaxBytes = 20_000_000
	// stderrMaxBytes is upstream's retained diagnostic tail for search.
	stderrMaxBytes = 65_536
	// versionTimeout bounds the startup `rg --version` probe.
	versionTimeout = 10 * time.Second
)

// minimumVersion is the ripgrep release packaged by the upstream reference
// (@vscode/ripgrep 1.18.0); the search contract is defined against it.
var minimumVersion = [3]int{15, 0, 0}

// invalidPattern matches ripgrep diagnostics for a rejected regex or glob.
var invalidPattern = regexp.MustCompile(`(?i)regex parse error|error parsing glob`)

// run executes ripgrep with a fixed argv in the workspace and returns its
// complete stdout. Exit 0 means results and exit 1 means none; every other
// outcome becomes an error in upstream's search vocabulary.
func (provider *Provider) run(ctx context.Context, tool string, arguments []string) (string, bool, error) {
	result, err := provider.runner.Run(ctx, platformProcess.Request{
		Path: provider.ripgrep, Args: append([]string{"--no-config"}, arguments...),
		Root: provider.root.Path(), Cwd: provider.root.Path(), Mode: platformProcess.ModeHost,
		Timeout: provider.timeout, StdoutLimit: provider.rawLimit, StderrLimit: stderrMaxBytes,
	})
	switch {
	case ctx.Err() != nil || err == nil && result.TimedOut:
		return "", false, &searchFailure{text: fmt.Sprintf("%s was aborted before completion (tool timeout or caller cancellation)", tool), code: "SEARCH_ABORTED", cause: errors.Join(ctx.Err(), err)}
	case err != nil:
		return "", false, searchError("SEARCH_FAILED", fmt.Errorf("%s could not start its search command (ripgrep launch failed): %w", tool, err))
	case result.Signal != "":
		return "", false, searchError("SEARCH_FAILED", fmt.Errorf("%s search command was killed by signal %s", tool, result.Signal))
	case result.ExitCode != 0 && result.ExitCode != 1:
		stderr := strings.TrimSpace(result.Stderr.Text)
		if stderr != "" && result.Stderr.Truncated {
			stderr += " [stderr truncated]"
		}
		if invalidPattern.MatchString(stderr) {
			return "", false, searchError("SEARCH_INVALID_PATTERN", fmt.Errorf("%s pattern rejected by ripgrep: %s", tool, stderr))
		}
		if stderr != "" {
			stderr = ": " + stderr
		}
		return "", false, searchError("SEARCH_FAILED", fmt.Errorf("%s search failed (exit %d)%s", tool, result.ExitCode, stderr))
	case result.Stdout.Truncated:
		return "", false, searchError("SEARCH_RAW_OUTPUT_OVERFLOW", fmt.Errorf("%s produced more raw output than the %d-byte cap; narrow pattern, path, or include and retry", tool, provider.rawLimit))
	}
	return result.Stdout.Text, result.ExitCode == 1, nil
}

// checkVersion runs `rg --version` and refuses releases older than
// minimumVersion.
func (provider *Provider) checkVersion(ctx context.Context) error {
	result, err := provider.runner.Run(ctx, platformProcess.Request{
		Path: provider.ripgrep, Args: []string{"--version"},
		Root: provider.root.Path(), Cwd: provider.root.Path(), Mode: platformProcess.ModeHost, Timeout: versionTimeout,
	})
	if err != nil {
		return fmt.Errorf("%w: run %s --version: %w", ErrRipgrepUnavailable, provider.ripgrep, err)
	}
	version, ok := parseVersion(result.Stdout.Text)
	if result.ExitCode != 0 || result.Signal != "" || result.TimedOut || !ok {
		return fmt.Errorf("%w: %s --version did not report a ripgrep version", ErrRipgrepUnavailable, provider.ripgrep)
	}
	for index := range version {
		if version[index] != minimumVersion[index] {
			if version[index] < minimumVersion[index] {
				return fmt.Errorf("%w: %s is ripgrep %s; %s or newer is required", ErrRipgrepUnavailable, provider.ripgrep, formatVersion(version), formatVersion(minimumVersion))
			}
			break
		}
	}
	return nil
}

// parseVersion reads "ripgrep MAJOR.MINOR.PATCH" from the first output line;
// a pre-release or build suffix after '-' or '+' is ignored.
func parseVersion(output string) ([3]int, bool) {
	line, _, _ := strings.Cut(output, "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "ripgrep" {
		return [3]int{}, false
	}
	core, _, _ := strings.Cut(fields[1], "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var version [3]int
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}, false
		}
		version[index] = number
	}
	return version, true
}

func formatVersion(version [3]int) string {
	return fmt.Sprintf("%d.%d.%d", version[0], version[1], version[2])
}

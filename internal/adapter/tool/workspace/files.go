package workspace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	maxReadBytes   = 256 << 10
	maxSearchBytes = 2 << 20
	maxWalkEntries = 20_000
	maxSearchHits  = 200
)

type readTool struct{ owner *Provider }

func (tool readTool) Definition() session.ToolDefinition {
	return definition("read_file", "Read a UTF-8 text file inside the workspace, optionally by line range.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":2000}},"required":["path"],"additionalProperties":false}`)
}
func (readTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (readTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool readTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArguments(execution.Arguments, &arguments); err != nil || strings.TrimSpace(arguments.Path) == "" {
		return "", errors.Join(err, errors.New("path is required"))
	}
	if arguments.Offset < 0 || arguments.Limit < 0 || arguments.Limit > 2000 {
		return "", errors.New("offset and limit are outside the supported range")
	}
	path, err := tool.owner.existingPath(arguments.Path)
	if err != nil {
		return "", err
	}
	info, err := workspaceStat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReadBytes {
		return "", errors.New("file is not a regular text file within the read limit")
	}
	data, err := workspaceRead(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return "", errors.New("file is not UTF-8 text")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	offset := max(arguments.Offset, 1)
	limit := arguments.Limit
	if limit == 0 {
		limit = 2000
	}
	if offset > len(lines) {
		return "", nil
	}
	end := min(offset-1+limit, len(lines))
	var output strings.Builder
	for index := offset - 1; index < end; index++ {
		fmt.Fprintf(&output, "%d: %s\n", index+1, lines[index])
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

type listTool struct{ owner *Provider }

func (tool listTool) Definition() session.ToolDefinition {
	return definition("list_files", "List files and directories below a workspace path without following symbolic links.", `{"type":"object","properties":{"path":{"type":"string"},"depth":{"type":"integer","minimum":0,"maximum":8}},"required":["path"],"additionalProperties":false}`)
}
func (listTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (listTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool listTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := decodeArguments(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	if arguments.Depth < 0 || arguments.Depth > 8 {
		return "", errors.New("depth must be 0-8")
	}
	path, err := tool.owner.existingPath(arguments.Path)
	if err != nil {
		return "", err
	}
	if arguments.Depth == 0 {
		arguments.Depth = 2
	}
	baseDepth := strings.Count(filepath.Clean(path), string(filepath.Separator))
	entries := make([]string, 0)
	err = workspaceWalk(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current != path && strings.Count(filepath.Clean(current), string(filepath.Separator))-baseDepth > arguments.Depth {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current != path {
			name := relativeName(tool.owner.root, current)
			if entry.IsDir() {
				name += "/"
			}
			entries = append(entries, name)
			if len(entries) > maxWalkEntries {
				return errors.New("workspace listing exceeds entry limit")
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n"), nil
}

type searchTool struct{ owner *Provider }

func (tool searchTool) Definition() session.ToolDefinition {
	return definition("search_files", "Search UTF-8 workspace files with a Go regular expression and return file:line matches.", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern","path"],"additionalProperties":false}`)
}
func (searchTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (searchTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool searchTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decodeArguments(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	if arguments.Pattern == "" || len(arguments.Pattern) > 1024 {
		return "", errors.New("pattern must be 1-1024 bytes")
	}
	expression, err := regexp.Compile(arguments.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regular expression: %w", err)
	}
	path, err := tool.owner.existingPath(arguments.Path)
	if err != nil {
		return "", err
	}
	hits := make([]string, 0)
	visited := 0
	err = workspaceWalk(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > maxWalkEntries {
			return errors.New("workspace search exceeds entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxSearchBytes {
			return nil
		}
		file, err := workspaceOpen(current)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(io.LimitReader(file, maxSearchBytes+1))
		scanner.Buffer(make([]byte, 4096), maxReadBytes)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if expression.MatchString(text) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", relativeName(tool.owner.root, current), line, text))
				if len(hits) >= maxSearchHits {
					break
				}
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return scanErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(hits) >= maxSearchHits {
			return errEnoughHits
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnoughHits) {
		return "", err
	}
	sort.Strings(hits)
	return strings.Join(hits, "\n"), nil
}

var errEnoughHits = errors.New("enough search hits")

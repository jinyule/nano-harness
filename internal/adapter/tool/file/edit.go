package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// maxEditBytes bounds the file an edit loads into memory.
	maxEditBytes = 10 << 20
	// lineEndingSample is the prefix used to detect the dominant line ending.
	lineEndingSample = 4096
)

type editArgs struct {
	FilePath           string  `json:"file_path"`
	OldString          string  `json:"old_string"`
	NewString          string  `json:"new_string"`
	ReplaceAll         *bool   `json:"replace_all"`
	SandboxPermissions *string `json:"sandbox_permissions"`
	Justification      *string `json:"justification"`
}

func (provider *Provider) editTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[editArgs]{
		Name:        "edit",
		Description: "Edit an existing UTF-8 text file by replacing literal text.",
		Parameters: append(appTool.Parameters{
			appTool.Required("file_path", appTool.String("Path to edit, resolved by the filesystem backend. Provide `file_path` before `old_string` and `new_string` in the arguments.")),
			appTool.Required("old_string", appTool.String("Literal text to replace.")),
			appTool.Required("new_string", appTool.String("Literal replacement text. Use an empty string to delete the match.")),
			appTool.Optional("replace_all", appTool.Boolean("Replace all matches. Defaults to false; when false, old_string must appear exactly once.")),
		}, workspace.EscalationProperties("operation", "file operation")...),
		Check: func(arguments editArgs) error {
			switch {
			case strings.TrimSpace(arguments.FilePath) == "":
				return errors.New("file_path must be a non-empty string")
			case arguments.OldString == "":
				return errors.New("old_string must be a non-empty string")
			case arguments.OldString == arguments.NewString:
				return errors.New("old_string and new_string must differ")
			}
			if err := checkEscalation(arguments.SandboxPermissions, arguments.Justification); err != nil {
				return err
			}
			// Refuse unsafe targets before asking; execution re-checks them.
			if _, err := provider.root.Writable(arguments.FilePath); err != nil {
				return fmt.Errorf("cannot edit %q: %w", arguments.FilePath, err)
			}
			return nil
		},
		Approval: func(arguments editArgs) string { return fmt.Sprintf("edit file %q", arguments.FilePath) },
		Execute:  provider.edit,
	})
}

func (provider *Provider) edit(ctx context.Context, invocation appTool.Invocation, arguments editArgs) (appTool.Result, error) {
	if !invocation.Approved {
		return appTool.Result{}, errors.New("edit approval was not granted")
	}
	if err := ctx.Err(); err != nil {
		return appTool.Result{}, fmt.Errorf("edit aborted: %w", err)
	}
	target, err := provider.root.Writable(arguments.FilePath)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot edit %q: %w", arguments.FilePath, err)
	}
	info, err := lstatFile(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return appTool.Result{}, fmt.Errorf("cannot edit %q: not found", target)
	case err != nil:
		return appTool.Result{}, fmt.Errorf("cannot edit %q: %w", target, err)
	case !info.Mode().IsRegular():
		return appTool.Result{}, fmt.Errorf("cannot edit %q: not a regular file", target)
	case info.Size() > maxEditBytes:
		return appTool.Result{}, fmt.Errorf("cannot edit %q: %d bytes exceeds the %d-byte limit", target, info.Size(), maxEditBytes)
	}
	raw, err := readLimited(target)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot edit %q: %w", target, err)
	}
	edited, err := replaceLiteral(raw, arguments.OldString, arguments.NewString, arguments.ReplaceAll != nil && *arguments.ReplaceAll, target)
	if err != nil {
		return appTool.Result{}, err
	}
	if err := writeAtomic(target, edited, info.Mode().Perm()); err != nil {
		return appTool.Result{}, fmt.Errorf("cannot edit %q: %w", target, err)
	}
	if arguments.ReplaceAll != nil && *arguments.ReplaceAll {
		return appTool.Text(fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", target)), nil
	}
	return appTool.Text(fmt.Sprintf("The file %s has been updated successfully.", target)), nil
}

func readLimited(path string) ([]byte, error) {
	reader, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }() // read-only; close cannot lose data
	data, err := io.ReadAll(io.LimitReader(reader, maxEditBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEditBytes {
		return nil, fmt.Errorf("content exceeds the %d-byte limit", maxEditBytes)
	}
	return data, nil
}

// replaceLiteral applies a literal edit like the upstream filesystem: text is
// matched with CRLF normalized to LF, the dominant original line ending is
// restored on write-back, and a leading BOM is preserved.
func replaceLiteral(raw []byte, oldString, newString string, replaceAll bool, display string) ([]byte, error) {
	if bytes.IndexByte(raw, 0) >= 0 {
		return nil, fmt.Errorf("cannot edit %q: binary file", display)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("cannot edit %q: invalid UTF-8 text", display)
	}
	bom := bytes.HasPrefix(raw, utf8BOM)
	text := string(bytes.TrimPrefix(raw, utf8BOM))
	sample := text[:min(len(text), lineEndingSample)]
	crlf := strings.Count(sample, "\r\n")
	useCRLF := crlf > strings.Count(sample, "\n")-crlf
	content := strings.ReplaceAll(text, "\r\n", "\n")
	needle := strings.ReplaceAll(oldString, "\r\n", "\n")
	replacement := strings.ReplaceAll(newString, "\r\n", "\n")
	matches := strings.Count(content, needle)
	switch {
	case matches == 0:
		return nil, fmt.Errorf("old_string was not found in %q", display)
	case matches > 1 && !replaceAll:
		return nil, fmt.Errorf("old_string matched %d times in %q; provide a more specific old_string or set replace_all to true", matches, display)
	}
	content = strings.ReplaceAll(content, needle, replacement)
	if useCRLF {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	if bom {
		content = string(utf8BOM) + content
	}
	return []byte(content), nil
}

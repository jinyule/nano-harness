package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// maxEditBytes bounds the file an edit loads into memory.
	maxEditBytes = 10 << 20
	// lineEndingSample is the prefix, in UTF-16 code units like upstream's
	// string slice, used to detect the dominant line ending.
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
		Check: func(ctx context.Context, invocation appTool.Invocation, arguments editArgs) error {
			switch {
			case strings.TrimSpace(arguments.FilePath) == "":
				return errors.New("file_path must be a non-empty string")
			case arguments.OldString == "":
				return errors.New("old_string must be a non-empty string")
			case arguments.OldString == arguments.NewString:
				return errors.New("old_string and new_string must differ")
			}
			mode, err := mutationMode(ctx, invocation, arguments.SandboxPermissions, arguments.Justification)
			if err != nil {
				return err
			}
			// Refuse unsafe, unobserved, or stale targets before asking;
			// execution re-checks them under the target's lock.
			target, err := provider.root.WritableIn(arguments.FilePath, mode)
			if err != nil {
				return classifyPath(arguments.FilePath, fmt.Errorf("cannot edit %q: %w", arguments.FilePath, err))
			}
			_, _, err = provider.observedContent(invocation.SessionID, target)
			return err
		},
		Guidance: appTool.StaticGuidance(appTool.OrderEdit,
			"Read a file before editing it (the default fs-observation-policy requires it), unless you just created or edited it in this session."),
		Approval: func(arguments editArgs) string {
			return mutationReason("edit", arguments.FilePath, arguments.SandboxPermissions, arguments.Justification)
		},
		Execute: provider.edit,
	})
}

// edit applies upstream's guarded edit: the session must have observed the
// target present, and its content must still match that observation before
// the literal replacement is matched against it.
func (provider *Provider) edit(ctx context.Context, invocation appTool.Invocation, arguments editArgs) (appTool.Result, error) {
	if !invocation.Approved {
		return appTool.Result{}, errors.New("edit approval was not granted")
	}
	if err := ctx.Err(); err != nil {
		return appTool.Result{}, fsFailure("FS_ABORTED", fmt.Errorf("edit aborted: %w", err))
	}
	if invocation.Delegated {
		return appTool.Result{}, errors.New("subagents cannot obtain file approval")
	}
	mode, err := mutationMode(ctx, invocation, arguments.SandboxPermissions, arguments.Justification)
	if err != nil {
		return appTool.Result{}, err
	}
	target, err := provider.root.WritableIn(arguments.FilePath, mode)
	if err != nil {
		return appTool.Result{}, classifyPath(arguments.FilePath, fmt.Errorf("cannot edit %q: %w", arguments.FilePath, err))
	}
	defer provider.mutate.lock(target)()
	info, raw, err := provider.observedContent(invocation.SessionID, target)
	if err != nil {
		return appTool.Result{}, err
	}
	edited, err := replaceLiteral(raw, arguments.OldString, arguments.NewString, arguments.ReplaceAll != nil && *arguments.ReplaceAll, target)
	if err != nil {
		return appTool.Result{}, err
	}
	if err := writeAtomic(ctx, target, edited, info.Mode().Perm(), false); err != nil {
		return appTool.Result{}, classifyKnown(fmt.Errorf("cannot edit %q: %w", target, err))
	}
	provider.observed.record(invocation.SessionID, target, observed(edited))
	// Display-only work uses the committed snapshot. Cancellation here leaves
	// publication and observation intact; runtime replaces success with ABORTED.
	meta := editMeta(ctx, target, raw, edited)
	if arguments.ReplaceAll != nil && *arguments.ReplaceAll {
		return appTool.Result{Text: fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", target), Meta: &session.ToolMeta{Edit: &meta}}, nil
	}
	return appTool.Result{Text: fmt.Sprintf("The file %s has been updated successfully.", target), Meta: &session.ToolMeta{Edit: &meta}}, nil
}

// observedContent loads an edit target only when the session observed it
// present and its content is unchanged since then. Missing targets report the
// stale remedy, like upstream's guarded edit.
func (provider *Provider) observedContent(sessionID, target string) (fs.FileInfo, []byte, error) {
	prior, seen := provider.observed.lookup(sessionID, target)
	switch {
	case !seen:
		return nil, nil, errNotRead(target)
	case !prior.present:
		return nil, nil, fsFailure("FS_NOT_FOUND", fmt.Errorf("cannot edit %q: not found", target))
	}
	info, err := lstatFile(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, errStale("edit", target, "file changed since it was read")
	case err != nil:
		return nil, nil, classifyKnown(fmt.Errorf("cannot edit %q: %w", target, err))
	case !info.Mode().IsRegular():
		return nil, nil, fsFailure("FS_NOT_REGULAR_FILE", fmt.Errorf("cannot edit %q: not a regular file", target))
	case info.Size() != prior.size:
		return nil, nil, errStale("edit", target, "file changed since it was read")
	case info.Size() > maxEditBytes:
		return nil, nil, fsFailure("FS_TOO_LARGE", fmt.Errorf("cannot edit %q: %d bytes exceeds the %d-byte limit", target, info.Size(), maxEditBytes))
	}
	raw, err := readLimited(target)
	if err != nil {
		return nil, nil, classifyKnown(fmt.Errorf("cannot edit %q: %w", target, err))
	}
	if digest(raw) != prior.version {
		return nil, nil, errStale("edit", target, "file changed since it was read")
	}
	return info, raw, nil
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
		return nil, fsFailure("FS_TOO_LARGE", fmt.Errorf("content exceeds the %d-byte limit", maxEditBytes))
	}
	return data, nil
}

// replaceLiteral applies a literal edit like the upstream filesystem: text is
// matched with CRLF normalized to LF, the dominant original line ending is
// restored on write-back, and a leading BOM is preserved.
func replaceLiteral(raw []byte, oldString, newString string, replaceAll bool, display string) ([]byte, error) {
	if bytes.IndexByte(raw, 0) >= 0 {
		return nil, fsFailure("FS_NOT_TEXT", fmt.Errorf("cannot edit %q: binary file", display))
	}
	if !utf8.Valid(raw) {
		return nil, fsFailure("FS_NOT_TEXT", fmt.Errorf("cannot edit %q: invalid UTF-8 text", display))
	}
	bom := bytes.HasPrefix(raw, utf8BOM)
	text := string(bytes.TrimPrefix(raw, utf8BOM))
	sample := samplePrefix(text, lineEndingSample)
	crlf := strings.Count(sample, "\r\n")
	useCRLF := crlf > strings.Count(sample, "\n")-crlf
	content := strings.ReplaceAll(text, "\r\n", "\n")
	needle := strings.ReplaceAll(oldString, "\r\n", "\n")
	replacement := strings.ReplaceAll(newString, "\r\n", "\n")
	matches := strings.Count(content, needle)
	switch {
	case matches == 0:
		return nil, fsFailure("FS_EDIT_NOT_FOUND", fmt.Errorf("old_string was not found in %q", display))
	case matches > 1 && !replaceAll:
		return nil, fsFailure("FS_AMBIGUOUS_EDIT", fmt.Errorf("old_string matched %d times in %q; provide a more specific old_string or set replace_all to true", matches, display))
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

// samplePrefix returns the longest prefix of text within units UTF-16 code
// units, never splitting a rune.
func samplePrefix(text string, units int) string {
	for index, char := range text {
		if units -= utf16.RuneLen(char); units < 0 {
			return text[:index]
		}
	}
	return text
}

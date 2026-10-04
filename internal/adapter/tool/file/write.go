package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// newFileMode and newDirectoryMode keep created entries owner-only;
	// replacing a file preserves its existing permission bits.
	newFileMode      fs.FileMode = 0o600
	newDirectoryMode fs.FileMode = 0o700
)

type writeArgs struct {
	FilePath           string  `json:"file_path"`
	Content            string  `json:"content"`
	SandboxPermissions *string `json:"sandbox_permissions"`
	Justification      *string `json:"justification"`
}

func (provider *Provider) writeTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[writeArgs]{
		Name:        "write",
		Description: "Create or fully replace a UTF-8 text file.",
		Parameters: append(appTool.Parameters{
			appTool.Required("file_path", appTool.String("Path to write, resolved by the filesystem backend. Provide `file_path` before `content` in the arguments.")),
			appTool.Required("content", appTool.String("Full UTF-8 text content to write.")),
		}, workspace.EscalationProperties("operation", "file operation")...),
		Check: func(arguments writeArgs) error {
			if strings.TrimSpace(arguments.FilePath) == "" {
				return errors.New("file_path must be a non-empty string")
			}
			if err := checkEscalation(arguments.SandboxPermissions, arguments.Justification); err != nil {
				return err
			}
			// Refuse unsafe targets before asking; execution re-checks them.
			if _, err := provider.root.Writable(arguments.FilePath); err != nil {
				return fmt.Errorf("cannot write %q: %w", arguments.FilePath, err)
			}
			return nil
		},
		Approval: func(arguments writeArgs) string { return fmt.Sprintf("write file %q", arguments.FilePath) },
		Execute:  provider.write,
	})
}

func (provider *Provider) write(ctx context.Context, invocation appTool.Invocation, arguments writeArgs) (appTool.Result, error) {
	if !invocation.Approved {
		return appTool.Result{}, errors.New("write approval was not granted")
	}
	if err := ctx.Err(); err != nil {
		return appTool.Result{}, fmt.Errorf("write aborted: %w", err)
	}
	target, err := provider.root.Writable(arguments.FilePath)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot write %q: %w", arguments.FilePath, err)
	}
	mode, operation := newFileMode, "Created"
	info, err := lstatFile(target)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return appTool.Result{}, fmt.Errorf("cannot write %q: not a regular file", target)
	case err == nil:
		mode, operation = info.Mode().Perm(), "Updated"
	case !errors.Is(err, fs.ErrNotExist):
		return appTool.Result{}, fmt.Errorf("cannot write %q: %w", target, err)
	}
	if err := makeDirs(filepath.Dir(target), newDirectoryMode); err != nil {
		return appTool.Result{}, fmt.Errorf("cannot write %q: %w", target, err)
	}
	if err := writeAtomic(target, []byte(arguments.Content), mode); err != nil {
		return appTool.Result{}, fmt.Errorf("cannot write %q: %w", target, err)
	}
	return appTool.Text(fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s file\n</content>", target, operation)), nil
}

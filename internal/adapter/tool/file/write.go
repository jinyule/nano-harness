package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
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
		Check: func(invocation appTool.Invocation, arguments writeArgs) error {
			if strings.TrimSpace(arguments.FilePath) == "" {
				return errors.New("file_path must be a non-empty string")
			}
			if err := checkEscalation(arguments.SandboxPermissions, arguments.Justification); err != nil {
				return err
			}
			// Refuse unsafe or unobserved targets before asking; execution
			// re-checks both under the target's lock. Check has no context,
			// so it hashes only files edit could load and leaves larger ones
			// to the cancellable execution-point check.
			target, err := provider.root.Writable(arguments.FilePath)
			if err != nil {
				return fmt.Errorf("cannot write %q: %w", arguments.FilePath, err)
			}
			_, _, err = provider.admitWrite(invocation.SessionID, target, maxEditBytes, func() error { return nil })
			return err
		},
		Guidance: appTool.Guidance{Order: appTool.OrderWrite, Text: func(visible func(string) bool) string {
			text := "Read an existing file before overwriting it with write (the default fs-observation-policy requires it)"
			if visible("edit") {
				text += " and prefer edit for targeted changes"
			}
			return text + "."
		}},
		Approval: func(arguments writeArgs) string { return fmt.Sprintf("write file %q", arguments.FilePath) },
		Execute:  provider.write,
	})
}

// write applies upstream's guarded write. A target this session observed
// present is replaced only while its content still matches that observation;
// any other target is created exclusively, so an existing file the session
// has not read is never overwritten.
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
	defer provider.mutate.lock(target)()
	info, exists, err := provider.admitWrite(invocation.SessionID, target, math.MaxInt64, ctx.Err)
	if err != nil {
		return appTool.Result{}, err
	}
	mode, operation := newFileMode, "Created"
	if exists {
		mode, operation = info.Mode().Perm(), "Updated"
	}
	if err := makeDirs(filepath.Dir(target), newDirectoryMode); err != nil {
		return appTool.Result{}, fmt.Errorf("cannot write %q: %w", target, err)
	}
	content := []byte(arguments.Content)
	if err := writeAtomic(ctx, target, content, mode, !exists); err != nil {
		if _, statErr := lstatFile(target); !exists && statErr == nil {
			return appTool.Result{}, errNotRead(target)
		}
		return appTool.Result{}, fmt.Errorf("cannot write %q: %w", target, err)
	}
	provider.observed.record(invocation.SessionID, target, observed(content))
	return appTool.Text(fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s file\n</content>", target, operation)), nil
}

// admitWrite decides upstream's write intent without side effects and
// reports the existing target, if any. The target must be a regular file or
// missing; one observed present must still hold the observed content, and
// any other target may only be created. A size change is stale at once;
// otherwise the content is hashed when it is at most hashLimit bytes, with
// stop checked between reads.
func (provider *Provider) admitWrite(sessionID, target string, hashLimit int64, stop func() error) (fs.FileInfo, bool, error) {
	info, err := lstatFile(target)
	exists := err == nil
	switch {
	case exists && !info.Mode().IsRegular():
		return nil, false, fmt.Errorf("cannot write %q: not a regular file", target)
	case !exists && !errors.Is(err, fs.ErrNotExist):
		return nil, false, fmt.Errorf("cannot write %q: %w", target, err)
	}
	prior, _ := provider.observed.lookup(sessionID, target)
	switch {
	case prior.present && !exists:
		return nil, false, errStale("write", target, "file no longer exists")
	case prior.present && info.Size() != prior.size:
		return nil, false, errStale("write", target, "file changed since it was read")
	case prior.present && info.Size() <= hashLimit:
		current, err := digestFile(target, stop)
		if err != nil {
			return nil, false, fmt.Errorf("cannot write %q: %w", target, err)
		}
		if current != prior.version {
			return nil, false, errStale("write", target, "file changed since it was read")
		}
	case !prior.present && exists:
		return nil, false, errNotRead(target)
	}
	return info, exists, nil
}

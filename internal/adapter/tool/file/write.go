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
	"github.com/jinyule/nano-harness/internal/core/session"
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
		Check: func(ctx context.Context, invocation appTool.Invocation, arguments writeArgs) error {
			if strings.TrimSpace(arguments.FilePath) == "" {
				return errors.New("file_path must be a non-empty string")
			}
			mode, err := mutationMode(ctx, invocation, arguments.SandboxPermissions, arguments.Justification)
			if err != nil {
				return err
			}
			// Refuse unsafe or unobserved targets before asking; execution
			// re-checks both under the target's lock. Larger existing files
			// are hashed only at the execution point.
			target, err := provider.root.WritableIn(arguments.FilePath, mode)
			if err != nil {
				return classifyPath(arguments.FilePath, fmt.Errorf("cannot write %q: %w", arguments.FilePath, err))
			}
			_, _, _, err = provider.admitWrite(invocation.SessionID, target, maxEditBytes, false, ctx.Err)
			return err
		},
		Guidance: appTool.Guidance{Order: appTool.OrderWrite, Text: func(visible func(string) bool) string {
			text := "Read an existing file before overwriting it with write (the default fs-observation-policy requires it)"
			if visible("edit") {
				text += " and prefer edit for targeted changes"
			}
			return text + "."
		}},
		Approval: func(arguments writeArgs) string {
			return mutationReason("write", arguments.FilePath, arguments.SandboxPermissions, arguments.Justification)
		},
		Execute: provider.write,
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
		return appTool.Result{}, fsFailure("FS_ABORTED", fmt.Errorf("write aborted: %w", err))
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
		return appTool.Result{}, classifyPath(arguments.FilePath, fmt.Errorf("cannot write %q: %w", arguments.FilePath, err))
	}
	defer provider.mutate.lock(target)()
	info, exists, before, err := provider.admitWrite(invocation.SessionID, target, math.MaxInt64, len(arguments.Content) < maxEditBytes, ctx.Err)
	if err != nil {
		return appTool.Result{}, err
	}
	permissions, operation := newFileMode, "Created"
	if exists {
		permissions, operation = info.Mode().Perm(), "Updated"
	}
	if err := makeDirs(filepath.Dir(target), newDirectoryMode); err != nil {
		return appTool.Result{}, classifyKnown(fmt.Errorf("cannot write %q: %w", target, err))
	}
	content := []byte(arguments.Content)
	meta := writeMeta(target, exists, before, content)
	if err := writeAtomic(ctx, target, content, permissions, !exists); err != nil {
		var publication *fsError
		if errors.As(err, &publication) && publication.code != "FS_ABORTED" {
			return appTool.Result{}, err
		}
		if _, statErr := lstatFile(target); !exists && statErr == nil {
			return appTool.Result{}, &plainFileError{message: errNotRead(target).Error(), err: err}
		}
		return appTool.Result{}, classifyKnown(fmt.Errorf("cannot write %q: %w", target, err))
	}
	provider.observed.record(invocation.SessionID, target, observed(content))
	return appTool.Result{Text: fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s file\n</content>", target, operation), Meta: &session.ToolMeta{Write: &meta}}, nil
}

// admitWrite decides upstream's write intent without side effects and
// reports the existing target, if any. The target must be a regular file or
// missing; one observed present must still hold the observed content, and
// any other target may only be created. A size change is stale at once;
// otherwise the content is hashed when it is at most hashLimit bytes, with
// stop checked between reads. retain requests a bounded diff basis from the
// same read; Check never retains content and Execute does so under its lock.
func (provider *Provider) admitWrite(sessionID, target string, hashLimit int64, retain bool, stop func() error) (fs.FileInfo, bool, []byte, error) {
	info, err := lstatFile(target)
	exists := err == nil
	switch {
	case exists && !info.Mode().IsRegular():
		return nil, false, nil, fsFailure("FS_NOT_REGULAR_FILE", fmt.Errorf("cannot write %q: not a regular file", target))
	case !exists && !errors.Is(err, fs.ErrNotExist):
		return nil, false, nil, classifyKnown(fmt.Errorf("cannot write %q: %w", target, err))
	}
	var before []byte
	prior, _ := provider.observed.lookup(sessionID, target)
	switch {
	case prior.present && !exists:
		return nil, false, nil, errStale("write", target, "file no longer exists")
	case prior.present && info.Size() != prior.size:
		return nil, false, nil, errStale("write", target, "file changed since it was read")
	case prior.present && info.Size() <= hashLimit:
		current, content, err := digestFile(target, retain && info.Size() < maxEditBytes, stop)
		if err != nil {
			return nil, false, nil, classifyKnown(fmt.Errorf("cannot write %q: %w", target, err))
		}
		before = content
		if current != prior.version {
			return nil, false, nil, errStale("write", target, "file changed since it was read")
		}
	case !prior.present && exists:
		return nil, false, nil, errNotRead(target)
	}
	return info, exists, before, nil
}

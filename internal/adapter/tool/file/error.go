package file

import (
	"context"
	"errors"
	"io/fs"
	"syscall"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// fsError adds the upstream identity without changing the model's text or cause.
type fsError struct {
	code    string
	message string
	err     error
}

func (failure *fsError) Error() string { return failure.message }
func (failure *fsError) Unwrap() error { return failure.err }
func (failure *fsError) ToolError() session.ToolError {
	return session.ToolError{Name: "FsError", Code: failure.code}
}

func fsFailure(code string, err error) error {
	return &fsError{code: code, message: err.Error(), err: err}
}

// ReadableFrom also reads the policy journal; only path resolution errors
// belong to the filesystem classification here.
func classifyPath(display string, err error) error {
	if display != "" || errors.Is(err, workspace.ErrOutsideRoot) || errors.Is(err, workspace.ErrSymlink) {
		return classifyIO(err)
	}
	return err
}

// classifyIO is used only at filesystem boundaries; semantic checks, policy
// journal failures, approval and image normalization keep their own errors.
func classifyIO(err error) error {
	var declared appTool.Failure
	if errors.As(err, &declared) {
		return err
	}
	code := "FS_IO_ERROR"
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = "FS_ABORTED"
	case errors.Is(err, workspace.ErrOutsideRoot), errors.Is(err, workspace.ErrSymlink):
		code = "FS_SANDBOX_DENIED"
	case errors.Is(err, fs.ErrPermission):
		code = "FS_PERMISSION_DENIED"
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		code = "FS_NOT_FOUND"
	case errors.Is(err, errBinary), errors.Is(err, errNotText):
		code = "FS_NOT_TEXT"
	}
	return fsFailure(code, err)
}

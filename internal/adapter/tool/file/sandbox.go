package file

import (
	"context"
	"fmt"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func mutationMode(ctx context.Context, invocation appTool.Invocation, requested, justification *string) (session.SandboxMode, error) {
	if err := workspace.ValidateEscalation(requested, justification); err != nil {
		return "", err
	}
	mode, err := invocation.SandboxMode(ctx)
	if err != nil {
		return "", err
	}
	mode, err = workspace.ResolveEscalation(mode, requested)
	if err != nil {
		return "", err
	}
	if mode == session.SandboxReadOnly {
		return "", fmt.Errorf("%s\n%s", workspace.DenialMarker(string(mode)), workspace.EscalationHint("operation"))
	}
	return mode, nil
}

func mutationReason(tool, path string, requested, justification *string) string {
	if requested != nil {
		return "escalate sandbox to " + *requested + ": " + *justification
	}
	return fmt.Sprintf("%s file %q", tool, path)
}

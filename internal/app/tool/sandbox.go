package tool

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// SandboxMode resolves the standing policy from the calling session at the
// operation boundary. Missing or unreadable history never grants execution.
func (invocation Invocation) SandboxMode(ctx context.Context) (session.SandboxMode, error) {
	reader, ok := invocation.Journal.(interface {
		Events(context.Context) ([]session.Event, error)
	})
	if !ok {
		return "", errors.New("sandbox policy requires a readable session journal")
	}
	events, err := reader.Events(ctx)
	if err != nil {
		return "", fmt.Errorf("read sandbox policy for %s: %w", invocation.SessionID, err)
	}
	return session.EffectiveSandbox(events), nil
}

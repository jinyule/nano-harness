package compaction

import (
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// Durable-only classifications and metadata never count toward the context
// estimate: the model never receives them.
func TestEstimateSurface_IgnoresToolErrorsAndMetadata(t *testing.T) {
	lines := make([]session.ReadLine, 1000)
	for index := range lines {
		lines[index] = session.ReadLine{Number: int64(index + 1), Text: strings.Repeat("x", 100)}
	}
	surface := func(strip bool) []session.SurfaceNode {
		read := &session.ToolResult{CallID: "read", Output: "short", Meta: &session.ToolMeta{Read: &session.ReadMeta{Path: "a", Offset: 1, Lines: lines, TotalLines: 1000}}}
		missing := &session.ToolResult{CallID: "missing", Output: "Error: not found", IsError: true, Error: &session.ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}}
		if strip {
			read.Meta, missing.Error = nil, nil
		}
		return []session.SurfaceNode{{Result: read}, {Result: missing}}
	}
	if with, without := estimateSurface(surface(false)), estimateSurface(surface(true)); with != without || with != 1+4 {
		t.Fatalf("estimate with durable-only data = %d, without = %d", with, without)
	}
}

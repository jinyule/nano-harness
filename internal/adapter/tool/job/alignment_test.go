package job

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestJobKill_ExplicitEmptyReasonReplacesPriorIntent(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		h := newHarness(t)
		release := make(chan struct{})
		id, err := h.jobs.Launch(appJob.Spec{Kind: "bash", Label: "hold", Owner: "root", Run: func(ctx context.Context, _ *appJob.Output) appJob.Outcome {
			<-ctx.Done()
			<-release
			return appJob.Outcome{Status: appJob.StatusKilled, Detail: "signal: SIGTERM"}
		}})
		if err != nil {
			t.Fatal(err)
		}
		h.call(t.Context(), t, "root", "job_kill", map[string]any{"job_id": id, "reason": "stale"})
		arguments := map[string]any{"job_id": id}
		if explicit {
			arguments["reason"] = ""
		}
		h.call(t.Context(), t, "root", "job_kill", arguments)
		close(release)
		result := h.call(t.Context(), t, "root", "job_output", map[string]any{"job_id": id, "wait": true})
		if strings.Contains(result.Output, "stale") == explicit {
			t.Errorf("explicit empty=%v: %q", explicit, result.Output)
		}
	}
}

func TestJobOutput_CancelledWaitDoesNotConsume(t *testing.T) {
	h := newHarness(t)
	id, _ := h.stream(t, "root", appJob.Outcome{})
	provider, _ := New(h.runtime, h.jobs)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := provider.output(ctx, appTool.Invocation{SessionID: "root"}, outputArgs{JobID: id, Wait: new(true)})
	if err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("cancelled wait = %v", err)
	}
	result := h.call(t.Context(), t, "root", "job_output", map[string]any{"job_id": id})
	if !strings.HasPrefix(result.Output, "building\n") {
		t.Fatalf("cancelled wait consumed output: %q", result.Output)
	}
}

func TestJobOutput_BoundsMetadataWithoutLosingFraming(t *testing.T) {
	text := renderOutput(appJob.Read{Lossy: true, Spills: []string{strings.Repeat("p", session.MaxTextBytes)}, Job: appJob.View{Status: appJob.StatusKilled, Detail: strings.Repeat("界", session.MaxTextBytes)}})
	if len(text) > session.MaxTextBytes || !utf8.ValidString(text) || !strings.Contains(text, "full output: (unavailable)]") || !strings.Contains(text, "[status: killed, ") || !strings.HasSuffix(text, "…]") {
		t.Fatalf("metadata envelope: bytes=%d suffix=%q", len(text), text[max(0, len(text)-80):])
	}
}

func TestJobOutput_IdentityErrorsPrecedeInvalidWait(t *testing.T) {
	h := newHarness(t)
	id, _ := h.stream(t, "root", appJob.Outcome{})
	for _, test := range []struct{ owner, id, want string }{
		{"root", "missing", "Error: unknown job missing"},
		{"child", id, "Error: job " + id + " belongs to another session"},
	} {
		result := h.call(t.Context(), t, test.owner, "job_output", map[string]any{"job_id": test.id, "wait": true, "timeout_ms": 0})
		if result.Output != test.want {
			t.Errorf("output(%s,%s) = %q; want %q", test.owner, test.id, result.Output, test.want)
		}
	}
}

func TestJobOutput_InvalidUTF8KeepsStatusAndLossNotice(t *testing.T) {
	h := newHarness(t)
	wrote := make(chan struct{})
	id, err := h.jobs.Launch(appJob.Spec{Kind: "bash", Label: "raw", Owner: "root", Run: func(ctx context.Context, output *appJob.Output) appJob.Outcome {
		_, _ = output.Writer(appJob.Stdout).Write(bytes.Repeat([]byte{0xff, 'a'}, 64<<10))
		close(wrote)
		<-ctx.Done()
		return appJob.Outcome{Status: appJob.StatusKilled}
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-wrote
	result := h.call(t.Context(), t, "root", "job_output", map[string]any{"job_id": id})
	if result.IsError || !utf8.ValidString(result.Output) || len(result.Output) > session.MaxTextBytes || !strings.HasSuffix(result.Output, "[status: running]") || !strings.Contains(result.Output, "[some output was dropped from memory; full output: (unavailable)]") {
		t.Fatalf("output bytes=%d valid=%v status=%v loss=%v", len(result.Output), utf8.ValidString(result.Output), strings.HasSuffix(result.Output, "[status: running]"), strings.Contains(result.Output, "some output was dropped"))
	}
}

func TestJobOutput_LargeValueKeepsFinalEnvelope(t *testing.T) {
	h := newHarness(t)
	id, err := h.jobs.Launch(appJob.Spec{Kind: "subagent", Label: "value", Owner: "root", Run: func(context.Context, *appJob.Output) appJob.Outcome {
		return appJob.Outcome{Status: appJob.StatusCompleted, Result: strings.Repeat("界", session.MaxTextBytes)}
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := h.call(t.Context(), t, "root", "job_output", map[string]any{"job_id": id, "wait": true})
	if result.IsError || !utf8.ValidString(result.Output) || len(result.Output) > session.MaxTextBytes || !strings.HasSuffix(result.Output, "[status: completed]") || !strings.Contains(result.Output, "[output truncated]") {
		t.Fatalf("value bytes=%d suffix=%q", len(result.Output), result.Output[max(0, len(result.Output)-80):])
	}
}

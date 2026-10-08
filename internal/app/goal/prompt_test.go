package goal

import (
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestRoundMessage_RendersTheUpstreamPrompt(t *testing.T) {
	view := View{Goal: session.GoalSnapshot{ID: "goal-1", Revision: 4, Objective: "fix \"all\"\n<docs>", Phase: session.GoalActive, MaxRounds: 256}, RoundsStarted: 2}
	message := RoundMessage(view)
	const want = "<goal_round>\nObjective: \"fix \\\"all\\\"\\n<docs>\"\nRound: 3/256\n\nContinue working toward the objective in this same session. Treat the current workspace, tool results, and durable session state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, read the current goal, and mark it complete. If work remains, leave the goal active for the next round. Follow the configured goal-tool policy before reporting a blocker.\n</goal_round>"
	if session.Text(message) != want {
		t.Fatalf("prompt = %q", session.Text(message))
	}
	if message.Role != session.RoleUser || message.Source != (session.MessageSource{Kind: session.GoalSource, GoalID: "goal-1", GoalRevision: 4, GoalRound: 3}) {
		t.Fatalf("message = %+v", message)
	}
	if err := (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWrapUpMessage_RendersBothClosingInstructions(t *testing.T) {
	complete := WrapUpMessage("ship it", "")
	const wantComplete = "<goal_complete>\nObjective: \"ship it\"\nThe goal is marked complete and this autonomous run is ending. Write the closing message to the user now: state the outcome, summarize what was done and how it was verified, and point to the concrete results (files, commits, or other artifacts). Report only what earlier rounds and tool results in this session actually establish; when a detail is not in the session, say so instead of inventing it. Note anything the user should review or do next. Address the user directly. Do not call any more tools in this run; further work waits for the user's next instruction.\n</goal_complete>"
	if session.Text(complete) != wantComplete || complete.Source.Kind != "tool-goal" || complete.Role != session.RoleUser {
		t.Fatalf("complete = %q %+v", session.Text(complete), complete.Source)
	}
	blocked := WrapUpMessage("ship it", "no key")
	const wantBlocked = "<goal_blocked>\nObjective: \"ship it\"\nBlocked: \"no key\"\nThe goal is marked blocked and this autonomous run is ending. Write the closing message to the user now: state what has been completed so far, describe the concrete blocking condition and what you tried, and say exactly what you need from the user to continue. Report only what earlier rounds and tool results in this session actually establish; when a detail is not in the session, say so instead of inventing it. Address the user directly. Do not call any more tools in this run; further work waits for the user's next instruction.\n</goal_blocked>"
	if session.Text(blocked) != wantBlocked {
		t.Fatalf("blocked = %q", session.Text(blocked))
	}
}

func TestQuote_MatchesJSONStringify(t *testing.T) {
	for value, want := range map[string]string{
		"":                    `""`,
		"plain <&> 中文":        `"plain <&> 中文"`,
		"q\"b\\":              `"q\"b\\"`,
		"\b\f\n\r\t":          `"\b\f\n\r\t"`,
		"\x00\x1f\x7f":        `"\u0000\u001f` + "\x7f" + `"`,
		"line\u2028sep\u2029": "\"line\u2028sep\u2029\"",
		"bad\xffbyte":         "\"bad\ufffdbyte\"",
	} {
		if got := Quote(value); got != want {
			t.Errorf("Quote(%q) = %q, want %q", value, got, want)
		}
	}
}

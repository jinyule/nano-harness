package goal

import (
	"fmt"
	"strings"

	"github.com/jinyule/nano-harness/internal/core/session"
)

const roundInstructions = "Continue working toward the objective in this same session. Treat the current workspace, tool results, and durable session state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, read the current goal, and mark it complete. If work remains, leave the goal active for the next round. Follow the configured goal-tool policy before reporting a blocker.\n"

const (
	// grounding is shared by both closing instructions, worded as upstream.
	grounding = "Report only what earlier rounds and tool results in this session actually establish; when a detail is not in the session, say so instead of inventing it. "
	// WrapUpSource is the message source kind of a closing instruction.
	WrapUpSource = "tool-goal"
)

// RoundMessage renders the upstream goal-round prompt for the next round of
// view, attributed to that exact goal revision and round.
func RoundMessage(view View) session.Message {
	round := view.RoundsStarted + 1
	text := "<goal_round>\n" +
		"Objective: " + Quote(view.Goal.Objective) + "\n" +
		fmt.Sprintf("Round: %d/%d\n\n", round, view.Goal.MaxRounds) +
		roundInstructions +
		"</goal_round>"
	return session.Message{
		Role:    session.RoleUser,
		Source:  session.MessageSource{Kind: session.GoalSource, GoalID: view.Goal.ID, GoalRevision: view.Goal.Revision, GoalRound: round},
		Content: []session.ContentBlock{{Type: session.ContentText, Text: text}},
	}
}

// WrapUpMessage renders the closing instruction delivered after an
// autonomous round marks its goal complete, or blocked when blockedReason
// is non-empty, so the model addresses the user once before the turn ends.
func WrapUpMessage(objective, blockedReason string) session.Message {
	heading := "Objective: " + Quote(objective) + "\n"
	var text string
	if blockedReason == "" {
		text = "<goal_complete>\n" + heading +
			"The goal is marked complete and this autonomous run is ending. Write the closing message to the user now: state the outcome, summarize what was done and how it was verified, and point to the concrete results (files, commits, or other artifacts). " +
			grounding +
			"Note anything the user should review or do next. Address the user directly. Do not call any more tools in this run; further work waits for the user's next instruction.\n" +
			"</goal_complete>"
	} else {
		text = "<goal_blocked>\n" + heading +
			"Blocked: " + Quote(blockedReason) + "\n" +
			"The goal is marked blocked and this autonomous run is ending. Write the closing message to the user now: state what has been completed so far, describe the concrete blocking condition and what you tried, and say exactly what you need from the user to continue. " +
			grounding +
			"Address the user directly. Do not call any more tools in this run; further work waits for the user's next instruction.\n" +
			"</goal_blocked>"
	}
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: WrapUpSource}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

// Quote renders value as JavaScript's JSON.stringify does: only quotes,
// backslashes, and control characters are escaped, so model-visible text
// matches the reference byte for byte. Invalid UTF-8 becomes U+FFFD.
func Quote(value string) string {
	var output strings.Builder
	output.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			output.WriteString(`\"`)
		case '\\':
			output.WriteString(`\\`)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if char < 0x20 {
				fmt.Fprintf(&output, `\u%04x`, char)
			} else {
				output.WriteRune(char)
			}
		}
	}
	output.WriteByte('"')
	return output.String()
}

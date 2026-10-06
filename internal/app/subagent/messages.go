package subagent

import (
	"strconv"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// Message source kinds the service writes into delegated and parent
// transcripts. They attribute provenance only; none grants authority.
const (
	// SourceDelegation marks the task a parent assigned when it created a child.
	SourceDelegation = "delegation"
	// SourceAgentMessage marks a message another agent sent with send_message.
	SourceAgentMessage = "agent-message"
	// SourceSettled marks the service's own account of a background child
	// that finished. It is distinct from SourceAgentMessage so a transcript
	// never credits the child with words it did not write.
	SourceSettled = "subagent-settled"
)

func textMessage(source string, texts ...string) session.Message {
	blocks := make([]session.ContentBlock, 0, len(texts))
	for _, text := range texts {
		if text != "" {
			blocks = append(blocks, session.ContentBlock{Type: session.ContentText, Text: text})
		}
	}
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: source}, Content: blocks}
}

// taskMessage is a child's opening message. A continuable child is also told
// its parent's id and how to report back, because its final answer is not
// copied to the parent automatically; the guidance follows the task so a
// fork's inherited prefix stays unchanged.
func taskMessage(prompt, parentID string, continuable bool) session.Message {
	if !continuable {
		return textMessage(SourceDelegation, prompt)
	}
	// Session IDs are ASCII letters, digits, '.', '_' and '-', so Go and JSON
	// quoting agree.
	quoted := strconv.Quote(parentID)
	guidance := "Your parent agent id is " + quoted + ". Before you finish, send your result to that agent with " +
		"send_message({ agent_id: " + quoted + `, message: "<self-contained result>" }). The parent shares ` +
		"your workspace but does not automatically receive your transcript, tool output, or reasoning. Send " +
		"earlier messages as well when a finding changes what the parent should do next; sending a message " +
		"does not end your turn."
	return textMessage(SourceDelegation, prompt, guidance)
}

// agentMessage is the recipient's view of one send_message call.
func agentMessage(senderID, text string) session.Message {
	return textMessage(SourceAgentMessage, "Agent "+senderID+" sent a message: ", text)
}

// settlementSummary is the one-line account of how a background child's
// last turn ended, in the parent's vocabulary.
func settlementSummary(childID string, outcome session.TurnOutcome) string {
	subject := "Background subagent " + childID
	switch outcome {
	case session.OutcomeCompleted:
		return subject + " finished and will do no further work unless you send it more."
	case session.OutcomeCanceled, session.OutcomeInterrupted:
		return subject + " was stopped before it finished."
	case session.OutcomeError:
		return subject + " failed before it finished."
	case session.OutcomeStepLimit, session.OutcomeMaxTokens:
		// Reported below under its stable outcome name.
	}
	return subject + " ended abnormally (" + string(outcome) + ") before it finished."
}

// settlementMessage tells a parent that a background child finished, with
// the child's closing text when it left any.
func settlementMessage(childID string, outcome session.TurnOutcome, closing string) session.Message {
	if closing == "" {
		return textMessage(SourceSettled, settlementSummary(childID, outcome), "It left no closing message.")
	}
	return textMessage(SourceSettled, settlementSummary(childID, outcome), "Its closing message:", closing)
}

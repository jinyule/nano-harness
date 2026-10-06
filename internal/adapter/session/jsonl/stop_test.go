package jsonl

import (
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestValidateOrder_OutputLimitRequiresClosedAssistantStep(t *testing.T) {
	end := session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeMaxTokens}
	stepEnd := session.Record{Type: session.RecordStepEnd, Turn: 1, Step: 1}
	for name, events := range map[string][]session.Event{
		"no step":      orderPrefix()[:2],
		"open step":    withAssistant(orderPrefix()),
		"no assistant": addOrder(orderPrefix(), stepEnd),
		"pending call": withCall(withAssistant(orderPrefix())),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateOrder(addOrder(events, end), true); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("accepted misplaced limit: %v", err)
			}
		})
	}
	valid := addOrder(addOrder(withAssistant(orderPrefix()), stepEnd), end)
	if _, err := validateOrder(valid, true); err != nil {
		t.Fatal(err)
	}
	// A previous turn's assistant cannot justify the next turn's outcome.
	valid = append(valid, orderedEvent(session.Record{Type: session.RecordTurnStart, Turn: 2}), orderedEvent(session.Record{Type: session.RecordTurnEnd, Turn: 2, Outcome: session.OutcomeMaxTokens}))
	if _, err := validateOrder(valid, true); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("previous completion leaked across turns: %v", err)
	}
}

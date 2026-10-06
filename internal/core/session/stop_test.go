package session

import (
	"errors"
	"testing"
)

func TestRecord_OutputLimitOutcomeIsStrict(t *testing.T) {
	for _, outcome := range []TurnOutcome{OutcomeMaxTokens, "max-tokens", "length", "max_output_tokens", ""} {
		err := (Record{Type: RecordTurnEnd, Turn: 1, Outcome: outcome}).Validate()
		if outcome == OutcomeMaxTokens && err != nil || outcome != OutcomeMaxTokens && !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("outcome %q = %v", outcome, err)
		}
	}
	if err := (Record{Type: RecordStepEnd, Turn: 1, Step: 1, Outcome: OutcomeMaxTokens}).Validate(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("misplaced outcome = %v", err)
	}
}

package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestToolCall_ArgumentBoundaryAndStickyOmission(t *testing.T) {
	arguments := json.RawMessage(`{"v":"` + strings.Repeat("x", MaxArgumentsBytes-8) + `"}`)
	call := ToolCall{ID: "c", Name: "t", Arguments: arguments}
	if limited := call.LimitArguments(); limited.ArgumentsOmitted || string(limited.Arguments) != string(arguments) {
		t.Fatal("arguments at the limit were omitted")
	}
	if !call.AppendArguments("") || call.AppendArguments(" ") || call.AppendArguments("{}") {
		t.Fatal("overflow did not stop accumulation")
	}
	call.Arguments = arguments
	call = call.LimitArguments()
	if !call.ArgumentsOmitted || string(call.Arguments) != "{}" || call.ID != "c" || call.Name != "t" {
		t.Fatalf("omission is not sticky: id=%s name=%s args=%d omitted=%v", call.ID, call.Name, len(call.Arguments), call.ArgumentsOmitted)
	}
	if string(arguments) == "{}" {
		t.Fatal("limiting a call mutated the original argument bytes")
	}
	for _, raw := range []string{`{`, `{"v":"` + strings.Repeat("<", MaxArgumentsBytes/6) + `"}`} {
		limited := (ToolCall{ID: "c", Name: "t", Arguments: json.RawMessage(raw)}).LimitArguments()
		if strings.Contains(raw, "<") {
			if !limited.ArgumentsOmitted {
				t.Fatal("durable escaping bypassed the limit")
			}
		} else if string(limited.Arguments) != raw || limited.ArgumentsOmitted {
			t.Fatal("malformed JSON was hidden by argument limiting")
		}
	}
}

func TestRecord_ValidatesOmittedArguments(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments string
		omitted   bool
		valid     bool
	}{
		{"ordinary", `{"v":1}`, false, true},
		{"omitted", `{}`, true, true},
		{"omitted-with-content", `{"v":1}`, true, false},
		{"omitted-null", `null`, true, false},
		{"too-large-without-marker", `{"v":"` + strings.Repeat("x", MaxArgumentsBytes) + `"}`, false, false},
		{"too-large-after-escaping", `{"v":"` + strings.Repeat("<", MaxArgumentsBytes/6) + `"}`, false, false},
		{"too-large-whitespace", `{}` + strings.Repeat(" ", MaxArgumentsBytes), false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (Record{Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{ID: "c", Name: "t", Arguments: json.RawMessage(test.arguments), ArgumentsOmitted: test.omitted}}).Validate()
			if test.valid && err != nil || !test.valid && !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("valid=%v, err=%v", test.valid, err)
			}
		})
	}
}

package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRecord_SandboxModeContract(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write", "danger-full-access"} {
		var record Record
		if err := json.Unmarshal([]byte(`{"type":"sandbox/mode","sandbox":{"mode":"`+mode+`"}}`), &record); err != nil {
			t.Fatal(err)
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestSandbox_ProjectionValidationAndClone(t *testing.T) {
	if text := SandboxPolicyText("invalid", "/work"); text != "" {
		t.Fatalf("invalid policy text: %s", text)
	}
	events := []Event{{Sequence: 1, Record: Record{Type: RecordSandboxMode, Sandbox: &SandboxModeChange{Mode: SandboxReadOnly}}}, {Sequence: 2, Record: Record{Type: RecordSandboxMode, Sandbox: &SandboxModeChange{Mode: SandboxDangerFullAccess, Source: "delegation"}}}}
	if EffectiveSandbox(nil) != SandboxWorkspaceWrite || SandboxOverride(nil) != "" || SandboxOverride(events[:1]) != SandboxReadOnly || EffectiveSandbox(events) != SandboxDangerFullAccess {
		t.Fatal("mode precedence")
	}
	clone := CloneEvent(events[0])
	clone.Record.Sandbox.Mode = SandboxWorkspaceWrite
	if events[0].Record.Sandbox.Mode != SandboxReadOnly {
		t.Fatal("clone aliases mutable mode")
	}
	for _, mode := range []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess} {
		text := SandboxPolicyText(mode, "/work/\"quoted")
		if !strings.Contains(text, "Current DSH file policy: "+string(mode)+".") {
			t.Fatal(text)
		}
	}
	for _, record := range []Record{
		{Type: RecordSandboxMode},
		{Type: RecordSandboxMode, Turn: 1, Sandbox: &SandboxModeChange{Mode: SandboxReadOnly}},
		{Type: RecordSandboxMode, Step: 1, Sandbox: &SandboxModeChange{Mode: SandboxReadOnly}},
		{Type: RecordSandboxMode, Sandbox: &SandboxModeChange{Mode: "unknown"}},
		{Type: RecordSandboxMode, Sandbox: &SandboxModeChange{Mode: SandboxReadOnly, Source: "model"}},
		{Type: RecordSandboxMode, Sandbox: &SandboxModeChange{Mode: SandboxReadOnly}, Plan: &PlanMode{Active: true}},
		{Type: RecordTurnStart, Turn: 1, Sandbox: &SandboxModeChange{Mode: SandboxReadOnly}},
	} {
		if !errors.Is(record.Validate(), ErrInvalidRecord) {
			t.Fatalf("accepted: %+v", record)
		}
	}
}

func TestSandboxModeChange_StrictDecode(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"mode":null}`, `{"mode":1}`, `{"mode":"read-only","source":null}`, `{"mode":"read-only","source":""}`, `{"mode":"read-only","other":"x"}`, `{"mode":"read-only","mode":"danger-full-access"}`, `{"mode":`} {
		var value SandboxModeChange
		if err := value.UnmarshalJSON([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var value SandboxModeChange
	if err := json.Unmarshal([]byte(`{"source":"delegation","mode":"workspace-write"}`), &value); err != nil || value.Mode != SandboxWorkspaceWrite || value.Source != "delegation" {
		t.Fatalf("value=%+v err=%v", value, err)
	}
}

// Text reviewed against sandbox-policy at the pinned Base reference; changes
// to policy wording are model-visible and must update this expectation.
func TestSandboxPolicyText_BaseContent(t *testing.T) {
	for mode, expected := range map[SandboxMode]string{
		SandboxReadOnly:         "Current DSH file policy: read-only. Any available operation enforced by the DSH file sandbox cannot modify files in the standing mode. Do not refuse a required modification from this policy alone: try an available tool normally and follow any denial and escalation guidance it returns.",
		SandboxWorkspaceWrite:   `Current DSH file policy: workspace-write. Any available operation enforced by the DSH file sandbox may modify files under the session workspace: "/work". Some platform temporary areas may also be writable.`,
		SandboxDangerFullAccess: "Current DSH file policy: danger-full-access. The DSH file sandbox does not restrict file modifications by available operations.",
	} {
		if actual := SandboxPolicyText(mode, "/work"); actual != expected {
			t.Fatalf("%s: %s", mode, actual)
		}
	}
	// Workspace names must remain JSON strings even with OS-valid controls.
	if text := SandboxPolicyText(SandboxWorkspaceWrite, "/work/\a"); !strings.Contains(text, `"/work/\u0007"`) {
		t.Fatal(text)
	}
}

package session

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/jinyule/nano-harness/internal/core/text"
)

// SandboxMode is the session's standing filesystem policy, independent of approval.
type SandboxMode string

const (
	// SandboxReadOnly denies file modifications by confined operations.
	SandboxReadOnly SandboxMode = "read-only"
	// SandboxWorkspaceWrite permits modifications in the workspace; it is the default.
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	// SandboxDangerFullAccess removes the filesystem sandbox.
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

// Valid reports membership in the closed persisted mode vocabulary.
func (mode SandboxMode) Valid() bool {
	return mode == SandboxReadOnly || mode == SandboxWorkspaceWrite || mode == SandboxDangerFullAccess
}

// SandboxModeChange is log-only policy metadata. Source is empty for a human
// switch and delegation for the explicit override captured when creating a child.
type SandboxModeChange struct {
	Mode   SandboxMode `json:"mode"`
	Source string      `json:"source,omitempty"`
}

// UnmarshalJSON rejects unknown, duplicate, missing, and non-string fields.
// Record validation subsequently checks the closed mode and source vocabulary.
func (change *SandboxModeChange) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return invalid("sandbox must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	_, _ = decoder.Token()
	seen := map[string]bool{}
	var value SandboxModeChange
	for decoder.More() {
		// Unmarshal above has established a syntactically valid object.
		key, _ := decoder.Token()
		name := key.(string)
		var raw json.RawMessage
		_ = decoder.Decode(&raw)
		if seen[name] || name != "mode" && name != "source" || len(raw) == 0 || raw[0] != '"' {
			return invalid("invalid sandbox field %q", name)
		}
		seen[name] = true
		var text string
		_ = json.Unmarshal(raw, &text)
		if name == "mode" {
			value.Mode = SandboxMode(text)
		} else {
			if text != "delegation" {
				return invalid("invalid sandbox source %q", text)
			}
			value.Source = text
		}
	}
	if !seen["mode"] {
		return fmt.Errorf("%w: sandbox mode is required", ErrInvalidRecord)
	}
	*change = value
	return nil
}

func (record Record) requireSandbox() error {
	if record.Sandbox == nil || record.Turn != 0 || record.Step != 0 || record.hasExtras("sandbox") || !record.Sandbox.Mode.Valid() || record.Sandbox.Source != "" && record.Sandbox.Source != "delegation" {
		return invalid("sandbox/mode shape is invalid")
	}
	return nil
}

// SandboxOverride folds the latest explicit override, including fork history.
// A child's own delegation record supersedes any older mode in its seed.
// Empty means the composition default applies. Events must be validated.
func SandboxOverride(events []Event) SandboxMode {
	var mode SandboxMode
	for _, event := range events {
		if event.Record.Sandbox != nil {
			mode = event.Record.Sandbox.Mode
		}
	}
	return mode
}

// EffectiveSandbox applies the workspace-write composition default beneath overrides.
func EffectiveSandbox(events []Event) SandboxMode {
	if mode := SandboxOverride(events); mode != "" {
		return mode
	}
	return SandboxWorkspaceWrite
}

// SandboxPolicyText renders Base's sandbox:policy context without inventorying
// tools. An invalid mode yields no policy text.
func SandboxPolicyText(mode SandboxMode, workspace string) string {
	switch mode {
	case SandboxReadOnly:
		return "Current DSH file policy: read-only. Any available operation enforced by the DSH file sandbox cannot modify files in the standing mode. Do not refuse a required modification from this policy alone: try an available tool normally and follow any denial and escalation guidance it returns."
	case SandboxWorkspaceWrite:
		return "Current DSH file policy: workspace-write. Any available operation enforced by the DSH file sandbox may modify files under the session workspace: " + text.Quote(workspace) + ". Some platform temporary areas may also be writable."
	case SandboxDangerFullAccess:
		return "Current DSH file policy: danger-full-access. The DSH file sandbox does not restrict file modifications by available operations."
	}
	return ""
}

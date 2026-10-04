package workspace

import (
	"errors"
	"strings"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

const (
	// ModeWorkspaceWrite is the standing mode: writes are confined to the
	// workspace and the shell's owned temporary directory.
	ModeWorkspaceWrite = "workspace-write"
	// ModeDangerFullAccess runs one approved shell command without the
	// filesystem sandbox. File tools never leave the workspace.
	ModeDangerFullAccess = "danger-full-access"
)

// EscalationProperties returns the upstream sandbox_permissions and
// justification parameters. subject names the denied action in the mode
// description and justified names it in the justification description.
func EscalationProperties(subject, justified string) []appTool.Property {
	return []appTool.Property{
		appTool.Optional("sandbox_permissions", appTool.String(
			"The narrowest wider sandbox mode for a one-shot retry of the exact "+subject+" the sandbox just denied; the retry asks the user for approval.",
			ModeWorkspaceWrite, ModeDangerFullAccess,
		)),
		appTool.Optional("justification", appTool.String(
			"Required with sandbox_permissions: one sentence for the user explaining why this exact "+justified+" needs the wider access. Use the language of the user’s current request.",
		)),
	}
}

// ValidateEscalation enforces the upstream pairing rule: a mode and a
// justification travel together and the justification is a non-empty sentence.
func ValidateEscalation(mode, justification *string) error {
	if mode != nil && justification == nil {
		return errors.New("invalid escalation: sandbox_permissions requires a justification")
	}
	if justification != nil && mode == nil {
		return errors.New("invalid escalation: justification is only valid together with sandbox_permissions")
	}
	if justification != nil && strings.TrimSpace(*justification) == "" {
		return errors.New("invalid justification: expected a non-empty sentence")
	}
	return nil
}

// DenialMarker is the model-facing line for a file effect the sandbox refused.
func DenialMarker(mode string) string {
	return "[sandbox: file access denied under " + mode + " mode]"
}

// EscalationHint tells the model how to retry a denied action once.
func EscalationHint(subject string) string {
	return "[sandbox: escalation available — retry this exact " + subject + " once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]"
}

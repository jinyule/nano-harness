// Package skill defines the model-facing vocabulary of runtime skills: the
// name grammar, the durable session catalog and its replacements, the
// rendering of a loaded skill, and the user-explicit `/name` gesture. The
// templates follow upstream tool-skill; description normalization keeps valid
// UTF-8. Everything here is a pure function of committed session events and discovered
// summaries, so a resumed session derives the same catalog state.
package skill

import (
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/jinyule/nano-harness/internal/core/session"
	coreText "github.com/jinyule/nano-harness/internal/core/text"
)

const (
	// MaxNameBytes bounds one skill name.
	MaxNameBytes = 64
	// MaxDescriptionUnits is the UTF-16 code unit limit rendered in
	// a catalog entry, including the "..." marker of a capped description.
	MaxDescriptionUnits = 500
	// MaxCatalogEntries bounds one catalog so that even the largest escaped
	// entries fit one session text block.
	MaxCatalogEntries = 100

	// SourceCatalog is the message source kind of an initial or replacement
	// skill catalog.
	SourceCatalog = "skill-catalog"
	// SourceInvocation is the message source kind of a skill body injected
	// for a user-explicit `/name` gesture.
	SourceInvocation = "skill-invocation"

	// userSource is the source kind of direct user input, the only kind
	// whose text can carry a gesture.
	userSource = "user"
)

var (
	textEscaper      = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attributeEscaper = strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;")
)

// ValidName reports whether name is a kebab-case skill name of at most
// MaxNameBytes bytes: lowercase ASCII letters and digits in runs joined by
// single hyphens.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameBytes || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for index := range len(name) {
		char := name[index]
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case char == '-' && name[index-1] != '-':
		default:
			return false
		}
	}
	return true
}

// Entry is one published catalog line: a skill name and its normalized,
// capped description.
type Entry struct {
	Name        string
	Description string
}

// NewEntry normalizes description for the catalog: whitespace runs collapse
// to one space, the ends are trimmed, and text longer than
// MaxDescriptionUnits keeps its first 497 UTF-16 units followed by "...".
// A split surrogate becomes U+FFFD so the catalog remains valid UTF-8.
func NewEntry(name, description string) Entry {
	normalized := strings.Join(strings.FieldsFunc(description, coreText.IsSpace), " ")
	units := utf16.Encode([]rune(normalized))
	if len(units) > MaxDescriptionUnits {
		normalized = string(utf16.Decode(units[:MaxDescriptionUnits-3])) + "..."
	}
	return Entry{Name: name, Description: normalized}
}

// TrimSpace removes leading and trailing whitespace as ECMAScript trim does,
// the normalization upstream applies to a skill body after its frontmatter.
func TrimSpace(value string) string { return coreText.TrimSpace(value) }

// CatalogUpdate returns the catalog text to commit before the next model
// step of a session whose committed log is events, given the entries the
// model may currently load, sorted by name. It reports false when the newest
// visible catalog already lists exactly entries, or when no catalog was ever
// published and there is nothing to list. Once a catalog exists, every later
// text is a complete replacement; an empty replacement retires earlier names.
// A catalog hidden by compaction counts as published but not visible, so the
// next update re-establishes the current list.
func CatalogUpdate(events []session.Event, entries []Entry) (string, bool, error) {
	published, current, shown, err := latestCatalog(events)
	if err != nil {
		return "", false, err
	}
	initial, replacement := renderCatalog(entries), renderReplacement(entries)
	switch {
	case shown && (current == initial || current == replacement):
		return "", false, nil
	case published:
		return replacement, true, nil
	case len(entries) == 0:
		return "", false, nil
	default:
		return initial, true, nil
	}
}

// latestCatalog reports whether events hold any catalog and returns the text
// of the newest catalog still visible in the replay surface.
func latestCatalog(events []session.Event) (published bool, current string, shown bool, err error) {
	surface, err := session.Surface(events)
	if err != nil {
		return false, "", false, err
	}
	visible := make(map[uint64]struct{}, len(surface))
	for _, node := range surface {
		visible[node.Sequence] = struct{}{}
	}
	for _, event := range slices.Backward(events) {
		record := event.Record
		if record.Type != session.RecordUserMessage || record.Message.Source.Kind != SourceCatalog {
			continue
		}
		published = true
		if _, ok := visible[event.Sequence]; ok {
			return true, session.Text(*record.Message), true, nil
		}
	}
	return published, "", false, nil
}

func renderCatalog(entries []Entry) string {
	lines := []string{
		"<system-reminder>",
		"A skill is a reusable set of task-specific instructions. The following skills are available in this session:",
		"",
		"<available_skills>",
	}
	lines = append(lines, renderEntries(entries)...)
	lines = append(lines,
		"</available_skills>",
		"",
		"If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions. Load all applicable skills, then follow their full instructions. This catalog contains summaries only; do not infer or follow a skill's instructions until it has been loaded.",
		"A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.",
		"</system-reminder>",
	)
	return strings.Join(lines, "\n")
}

func renderReplacement(entries []Entry) string {
	lines := []string{
		"<system-reminder>",
		"The available skill catalog changed. This complete catalog replaces every earlier available-skills list in this session:",
		"",
		"<available_skills>",
	}
	lines = append(lines, renderEntries(entries)...)
	lines = append(lines, "</available_skills>", "")
	if len(entries) == 0 {
		lines = append(lines,
			"No skills are currently available through the `skill` tool. Do not use names from earlier skill catalogs.",
			"A user may still invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool for it.",
		)
	} else {
		lines = append(lines,
			"Use only names in this replacement catalog. If the user names a listed skill, or the task clearly matches its description, call the `skill` tool with the exact name before acting.",
			"A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.",
		)
	}
	return strings.Join(append(lines, "</system-reminder>"), "\n")
}

// renderEntries escapes descriptions for the pseudo-XML frame; names are
// validated and need no escaping.
func renderEntries(entries []Entry) []string {
	lines := make([]string, len(entries))
	for index, entry := range entries {
		lines[index] = "- `" + entry.Name + "`: " + textEscaper.Replace(entry.Description)
	}
	return lines
}

// RenderContent renders one loaded skill in the canonical <skill_content>
// shape shared by the skill tool result and the gesture injection. directory
// is the base for the skill's relative resources; body is embedded verbatim
// because skills are trusted local instructions.
func RenderContent(name, directory, body string) string {
	return strings.Join([]string{
		`<skill_content name="` + attributeEscaper.Replace(name) + `">`,
		"<skill_resources>",
		"Base directory for this skill: " + textEscaper.Replace(directory),
		"Resolve relative paths mentioned by this skill against the base directory before using them. Load referenced resources only as needed.",
		"</skill_resources>",
		"",
		"<skill_instructions>",
		body,
		"</skill_instructions>",
		"</skill_content>",
	}, "\n")
}

// InvokedNames returns the distinct skill names that direct user input
// committed since the latest turn/start or step/start names with a
// whitespace-bounded `/name` token, in first-seen order. Only text blocks of
// user-sourced messages count, so injected or delegated text cannot forge a
// gesture; paths such as /a/b and fractions such as 5/8 are not tokens.
func InvokedNames(events []session.Event) []string {
	start := 0
	for index, event := range slices.Backward(events) {
		if kind := event.Record.Type; kind == session.RecordTurnStart || kind == session.RecordStepStart {
			start = index + 1
			break
		}
	}
	var names []string
	for _, event := range events[start:] {
		record := event.Record
		if record.Type != session.RecordUserMessage || record.Message.Source.Kind != userSource {
			continue
		}
		for _, block := range record.Message.Content {
			if block.Type != session.ContentText {
				continue
			}
			for _, token := range strings.FieldsFunc(block.Text, coreText.IsSpace) {
				name, ok := strings.CutPrefix(token, "/")
				if ok && ValidName(name) && !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	return names
}

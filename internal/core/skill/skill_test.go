package skill

import (
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestValidName_AcceptsOnlyBoundedKebabCase(t *testing.T) {
	for _, name := range []string{"a", "0", "pdf", "office-docx", "a1-2b-c3", strings.Repeat("a", MaxNameBytes)} {
		if !ValidName(name) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, name := range []string{"", "-a", "a-", "a--b", "A", "Skill", "a_b", "a b", "a.md", "é", "/a", strings.Repeat("a", MaxNameBytes+1)} {
		if ValidName(name) {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestNewEntry_NormalizesAndCapsDescriptions(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"  Use\t\tthis\n\nskill  ", "Use this skill"},
		{"wide\u3000space\u00a0and\ufeffbom\u2028line\u2009thin", "wide space and bom line thin"},
		{"next\u0085line", "next\u0085line"},
		{strings.Repeat("x", MaxDescriptionUnits), strings.Repeat("x", MaxDescriptionUnits)},
		{strings.Repeat("x", MaxDescriptionUnits+1), strings.Repeat("x", MaxDescriptionUnits-3) + "..."},
		{strings.Repeat("界", MaxDescriptionUnits+1), strings.Repeat("界", MaxDescriptionUnits-3) + "..."},
	} {
		if got := NewEntry("name", test.raw); got != (Entry{Name: "name", Description: test.want}) {
			t.Errorf("NewEntry(%q) = %q, want %q", test.raw, got.Description, test.want)
		}
	}
}

func TestNewEntry_UTF16DescriptionLimit(t *testing.T) {
	for _, test := range []struct{ name, raw, want string }{
		{"exact", strings.Repeat("😀", 250), strings.Repeat("😀", 250)},
		{"mixed", strings.Repeat("😀", 200) + strings.Repeat("x", 101), strings.Repeat("😀", 200) + strings.Repeat("x", 97) + "..."},
		{"whole pair", strings.Repeat("x", 495) + "😀yyyy", strings.Repeat("x", 495) + "😀..."},
		{"split pair", strings.Repeat("😀", 251), strings.Repeat("😀", 248) + "\ufffd..."},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NewEntry("name", test.raw).Description; got != test.want {
				t.Fatalf("description=%q want=%q", got, test.want)
			}
		})
	}
}

var upstreamEntries = []Entry{
	NewEntry("a-skill", "Use {{placeholder}} <safely> & carefully."),
	NewEntry("model-only-skill", "Model-only skill."),
}

// The expected texts are the upstream tool-skill templates, line for line.
var (
	upstreamCatalog = strings.Join([]string{
		"<system-reminder>",
		"A skill is a reusable set of task-specific instructions. The following skills are available in this session:",
		"",
		"<available_skills>",
		"- `a-skill`: Use {{placeholder}} &lt;safely&gt; &amp; carefully.",
		"- `model-only-skill`: Model-only skill.",
		"</available_skills>",
		"",
		"If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions. Load all applicable skills, then follow their full instructions. This catalog contains summaries only; do not infer or follow a skill's instructions until it has been loaded.",
		"A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.",
		"</system-reminder>",
	}, "\n")
	upstreamReplacement = strings.Join([]string{
		"<system-reminder>",
		"The available skill catalog changed. This complete catalog replaces every earlier available-skills list in this session:",
		"",
		"<available_skills>",
		"- `a-skill`: Use {{placeholder}} &lt;safely&gt; &amp; carefully.",
		"- `model-only-skill`: Model-only skill.",
		"</available_skills>",
		"",
		"Use only names in this replacement catalog. If the user names a listed skill, or the task clearly matches its description, call the `skill` tool with the exact name before acting.",
		"A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.",
		"</system-reminder>",
	}, "\n")
	upstreamEmptyReplacement = strings.Join([]string{
		"<system-reminder>",
		"The available skill catalog changed. This complete catalog replaces every earlier available-skills list in this session:",
		"",
		"<available_skills>",
		"</available_skills>",
		"",
		"No skills are currently available through the `skill` tool. Do not use names from earlier skill catalogs.",
		"A user may still invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool for it.",
		"</system-reminder>",
	}, "\n")
)

// sessionLog builds a contiguous committed log from records.
func sessionLog(records ...session.Record) []session.Event {
	events := make([]session.Event, len(records))
	for index, record := range records {
		events[index] = session.Event{Sequence: uint64(index + 1), Record: record}
	}
	return events
}

func userMessage(kind string, blocks ...session.ContentBlock) session.Record {
	return session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: kind}, Content: blocks}}
}

func text(value string) session.ContentBlock {
	return session.ContentBlock{Type: session.ContentText, Text: value}
}

func catalogMessage(value string) session.Record { return userMessage(SourceCatalog, text(value)) }

func TestCatalogUpdate_PublishesReplacesAndRetires(t *testing.T) {
	turn := session.Record{Type: session.RecordTurnStart, Turn: 1}
	input := userMessage("user", text("hello"))
	other := []Entry{NewEntry("other", "Another skill.")}
	for _, test := range []struct {
		name    string
		events  []session.Event
		entries []Entry
		want    string
		ok      bool
	}{
		{name: "nothing to publish", events: sessionLog(turn, input)},
		{name: "initial catalog", events: sessionLog(turn, input), entries: upstreamEntries, want: upstreamCatalog, ok: true},
		{name: "unchanged initial", events: sessionLog(turn, input, catalogMessage(upstreamCatalog)), entries: upstreamEntries},
		{name: "unchanged replacement", events: sessionLog(turn, input, catalogMessage(upstreamReplacement)), entries: upstreamEntries},
		{name: "changed membership", events: sessionLog(turn, input, catalogMessage(upstreamCatalog)), entries: other, want: renderReplacement(other), ok: true},
		{name: "retire every name", events: sessionLog(turn, input, catalogMessage(upstreamCatalog)), want: upstreamEmptyReplacement, ok: true},
		{name: "retired stays empty", events: sessionLog(turn, input, catalogMessage(upstreamEmptyReplacement))},
		{name: "newest visible wins", events: sessionLog(turn, input, catalogMessage(upstreamCatalog), catalogMessage(renderReplacement(other))), entries: upstreamEntries, want: upstreamReplacement, ok: true},
		{name: "unrecognized catalog text", events: sessionLog(turn, input, catalogMessage("edited by hand")), entries: upstreamEntries, want: upstreamReplacement, ok: true},
		{name: "compacted catalog", events: sessionLog(turn, input, catalogMessage(upstreamCatalog), session.Record{
			Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "c", ShadowedSeqs: []uint64{2, 3}, ShadowedTokenCount: 10, Summary: []session.ContentBlock{text("summary")}, Provider: "p", Model: "m"},
		}), entries: upstreamEntries, want: upstreamReplacement, ok: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := CatalogUpdate(test.events, test.entries)
			if err != nil || ok != test.ok || got != test.want {
				t.Fatalf("CatalogUpdate = %v %v\n%s\nwant %v\n%s", ok, err, got, test.ok, test.want)
			}
		})
	}
	invalid := sessionLog(turn, session.Record{Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "bad", ShadowedSeqs: []uint64{9}}})
	if _, _, err := CatalogUpdate(invalid, upstreamEntries); err == nil {
		t.Fatal("an invalid surface was accepted")
	}
}

func TestCatalogUpdate_LargestCatalogFitsOneRecord(t *testing.T) {
	entries := make([]Entry, MaxCatalogEntries)
	for index := range entries {
		name := strings.Repeat(string(rune('a'+index%26)), MaxNameBytes-3) + "-" + string(rune('a'+index/26)) + "x"
		entries[index] = NewEntry(name, strings.Repeat("&", MaxDescriptionUnits+10))
	}
	for _, rendered := range []string{renderCatalog(entries), renderReplacement(entries)} {
		record := catalogMessage(rendered)
		if err := record.Validate(); err != nil {
			t.Fatalf("largest catalog (%d bytes) is not a valid record: %v", len(rendered), err)
		}
	}
}

func TestRenderContent_MatchesUpstreamShape(t *testing.T) {
	want := strings.Join([]string{
		`<skill_content name="office-docx">`,
		"<skill_resources>",
		"Base directory for this skill: /home/u/.agents/skills/a&amp;b/&lt;x&gt;",
		"Resolve relative paths mentioned by this skill against the base directory before using them. Load referenced resources only as needed.",
		"</skill_resources>",
		"",
		"<skill_instructions>",
		"Body with <tags> & raw text.",
		"</skill_instructions>",
		"</skill_content>",
	}, "\n")
	if got := RenderContent("office-docx", "/home/u/.agents/skills/a&b/<x>", "Body with <tags> & raw text."); got != want {
		t.Fatalf("RenderContent =\n%s", got)
	}
	if got := RenderContent(`a"<&`, "/d", ""); !strings.HasPrefix(got, `<skill_content name="a&quot;&lt;&amp;">`) {
		t.Fatalf("attribute escaping = %s", got)
	}
}

func TestInvokedNames_ScansOnlyNewDirectUserText(t *testing.T) {
	image := session.ContentBlock{Type: session.ContentImage, Image: &session.Image{}}
	events := sessionLog(
		session.Record{Type: session.RecordTurnStart, Turn: 1},
		userMessage("user", text("/before-step should not count")),
		session.Record{Type: session.RecordStepStart, Turn: 1, Step: 1},
		session.Record{Type: session.RecordStepEnd, Turn: 1, Step: 1},
		userMessage("user", text("/pdf summarize"), image),
		userMessage("user", text("please use /pdf and\u3000/office-docx\tnow")),
		userMessage("user", text("see /a/b, 5/8, foo/bar, /Upper, /trailing- and /x."), text("/second-block")),
		userMessage(SourceCatalog, text("/forged-catalog")),
		userMessage("delegation", text("/forged-delegation")),
	)
	if got := InvokedNames(events); !slices.Equal(got, []string{"pdf", "office-docx", "second-block"}) {
		t.Fatalf("names = %v", got)
	}
	if got := InvokedNames(events[:2]); !slices.Equal(got, []string{"before-step"}) {
		t.Fatalf("turn window = %v", got)
	}
	if got := InvokedNames(sessionLog(userMessage("user", text("/no-turn")))); !slices.Equal(got, []string{"no-turn"}) {
		t.Fatalf("whole log window = %v", got)
	}
	if got := InvokedNames(events[:4]); got != nil {
		t.Fatalf("empty window = %v", got)
	}
}

func TestTrimSpace_UsesECMAScriptWhitespace(t *testing.T) {
	if got := TrimSpace("\ufeff\u3000\n body  \t"); got != "body" {
		t.Fatalf("TrimSpace = %q", got)
	}
	if got := TrimSpace("\u0085body\u0085"); got != "\u0085body\u0085" {
		t.Fatalf("NEL is not ECMAScript whitespace: %q", got)
	}
}

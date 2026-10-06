package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPruneToolOutput_KeepsHeadAndTailCodePoints(t *testing.T) {
	if utf8.RuneCountInString(PruneMarker)+PruneHead+PruneTail > PruneThreshold {
		t.Fatal("pruned output can exceed the threshold")
	}
	for _, text := range []string{"", "short", strings.Repeat("x", PruneThreshold), strings.Repeat("界", PruneThreshold)} {
		if pruned, ok := PruneToolOutput(text); ok || pruned != "" {
			t.Fatalf("output of %d code points was pruned", utf8.RuneCountInString(text))
		}
	}
	ascii := strings.Repeat("h", PruneHead) + strings.Repeat("m", PruneThreshold) + strings.Repeat("t", PruneTail)
	pruned, ok := PruneToolOutput(ascii)
	if !ok || pruned != strings.Repeat("h", PruneHead)+PruneMarker+strings.Repeat("t", PruneTail) {
		t.Fatalf("ASCII prune = %d bytes, %t", len(pruned), ok)
	}
	if again, ok := PruneToolOutput(pruned); ok || again != "" {
		t.Fatal("a pruned output was pruned again")
	}
	justOver, ok := PruneToolOutput(strings.Repeat("a", PruneThreshold+1))
	if !ok || utf8.RuneCountInString(justOver) != PruneHead+utf8.RuneCountInString(PruneMarker)+PruneTail {
		t.Fatalf("threshold+1 prune has %d code points", utf8.RuneCountInString(justOver))
	}
}

func TestPruneToolOutput_CutsOnCodePointBoundaries(t *testing.T) {
	// Astral characters are surrogate pairs upstream and four bytes here; the
	// cut points fall right after and right before them.
	head := strings.Repeat("中", PruneHead-1) + "😀"
	tail := "🎉" + strings.Repeat("é", PruneTail-1)
	middle := strings.Repeat("ß", 5000)
	pruned, ok := PruneToolOutput(head + middle + tail)
	if !ok || pruned != head+PruneMarker+tail || !utf8.ValidString(pruned) {
		t.Fatalf("multi-byte prune kept %d code points, valid=%t", utf8.RuneCountInString(pruned), utf8.ValidString(pruned))
	}
	// Each byte of an invalid sequence counts as one code point.
	invalid := strings.Repeat("\xff", PruneThreshold+1)
	pruned, ok = PruneToolOutput(invalid)
	if !ok || pruned != strings.Repeat("\xff", PruneHead)+PruneMarker+strings.Repeat("\xff", PruneTail) {
		t.Fatalf("invalid UTF-8 prune = %d bytes, %t", len(pruned), ok)
	}
}

func TestPruneToolOutput_KeepsASpillNoticeInTheTail(t *testing.T) {
	// Spill previews end with the recovery notice; a realistic locator keeps
	// it inside the retained tail, as upstream's pruner does.
	path := "/Users/someone/Library/Application Support/nano-harness/spill/" + strings.Repeat("w", 64) + "/session-0123456789abcdef/bash-1.txt"
	notice := "(Omitted 123456 bytes. Full formatted result stored at: " + path + ". Use read with offset/limit, or grep this path to search within it.)"
	preview := strings.Repeat("head line\n", 2500) + "\n\n[... output truncated ...]\n\n" + strings.Repeat("tail line\n", 2500) + "\n\n" + notice
	pruned, ok := PruneToolOutput(preview)
	if !ok || !strings.HasSuffix(pruned, notice) {
		t.Fatalf("pruned spill preview lost its notice: %t", ok)
	}
}

func pruneEvents(output string, isError bool, image *Image) []Event {
	return []Event{
		{Sequence: 1, Record: Record{Type: RecordUserMessage, Turn: 1, Message: textMessage(RoleUser, "go")}},
		{Sequence: 2, Record: Record{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: textMessage(RoleAssistant, "calling")}},
		{Sequence: 3, Record: Record{Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}}},
		{Sequence: 4, Record: Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call", Output: output, IsError: isError, Image: image}}},
	}
}

func TestSurface_AppliesRecordedPrunes(t *testing.T) {
	long := strings.Repeat("r", PruneThreshold*2)
	pruned, _ := PruneToolOutput(long)
	image := &Image{ID: "img", Name: "shot.png", MediaType: "image/png", Bytes: 10, Width: 1, Height: 1}
	events := append(pruneEvents(long, true, image), Event{Sequence: 5, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 4, Output: pruned}}})
	surface, err := Surface(events)
	if err != nil {
		t.Fatal(err)
	}
	result := surface[3].Result
	if surface[3].Sequence != 4 || result.Output != pruned || result.CallID != "call" || !result.IsError || result.Image == nil || result.Image.ID != "img" {
		t.Fatalf("pruned node = %+v / %+v", surface[3], result)
	}
	if events[3].Record.Result.Output != long {
		t.Fatal("the fold rewrote the original event")
	}
	summarized := append(append([]Event(nil), events...), Event{Sequence: 6, Record: Record{Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "c", ShadowedSeqs: []uint64{1, 2, 3, 4}, ShadowedTokenCount: 10, Summary: []ContentBlock{{Type: ContentText, Text: "summary"}}, Provider: "p", Model: "m"}}})
	if surface, err := Surface(summarized); err != nil || len(surface) != 1 || session0(surface) != "summary" {
		t.Fatalf("summary over a pruned node = %+v, %v", surface, err)
	}
	for name, prune := range map[string]Event{
		"tampered output":    {Sequence: 5, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 4, Output: pruned + "x"}}},
		"not a tool result":  {Sequence: 5, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 3, Output: pruned}}},
		"unknown sequence":   {Sequence: 5, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 9, Output: pruned}}},
		"within the budget":  {Sequence: 5, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 4, Output: "short"}}},
		"already summarized": {Sequence: 7, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 4, Output: pruned}}},
	} {
		t.Run(name, func(t *testing.T) {
			base := pruneEvents(long, false, nil)
			if name == "within the budget" {
				base = pruneEvents("short", false, nil)
			}
			if name == "already summarized" {
				base = summarized[:4]
				base = append(base, summarized[5])
			}
			if _, err := Surface(append(base, prune)); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	twice := append(append([]Event(nil), events...), Event{Sequence: 6, Record: Record{Type: RecordCompactionPrune, Turn: 1, Prune: &ToolResultPrune{Seq: 4, Output: pruned}}})
	if _, err := Surface(twice); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("second prune of the same result = %v", err)
	}
}

func session0(surface []SurfaceNode) string { return Text(*surface[0].Message) }

func TestCompactionPrune_ValidateShapeAndClone(t *testing.T) {
	for _, record := range []Record{
		{Type: RecordCompactionPrune, Prune: &ToolResultPrune{Seq: 1, Output: "x"}},
		{Type: RecordCompactionPrune, Turn: 2, Prune: &ToolResultPrune{Seq: 4}},
	} {
		if err := record.Validate(); err != nil {
			t.Errorf("valid %+v: %v", record, err)
		}
	}
	for name, record := range map[string]Record{
		"missing payload": {Type: RecordCompactionPrune, Turn: 1},
		"inside step":     {Type: RecordCompactionPrune, Turn: 1, Step: 1, Prune: &ToolResultPrune{Seq: 1}},
		"zero sequence":   {Type: RecordCompactionPrune, Prune: &ToolResultPrune{}},
		"oversized":       {Type: RecordCompactionPrune, Prune: &ToolResultPrune{Seq: 1, Output: strings.Repeat("x", MaxTextBytes+1)}},
		"extras":          {Type: RecordCompactionPrune, Prune: &ToolResultPrune{Seq: 1}, Outcome: OutcomeCompleted},
		"bare with prune": {Type: RecordTurnStart, Turn: 1, Prune: &ToolResultPrune{Seq: 1}},
		"message extras":  {Type: RecordUserMessage, Turn: 1, Message: textMessage(RoleUser, "x"), Prune: &ToolResultPrune{Seq: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	event := Event{Sequence: 5, Record: Record{Type: RecordCompactionPrune, Prune: &ToolResultPrune{Seq: 4, Output: "x"}}}
	cloned := CloneEvent(event)
	cloned.Record.Prune.Output = "changed"
	if event.Record.Prune.Output != "x" {
		t.Fatal("CloneEvent aliases the prune payload")
	}
}

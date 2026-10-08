package session

import (
	"fmt"
	"unicode/utf8"
)

// Tool-result pruning budgets, in Unicode code points. They are the upstream
// Base composition's tool-result-pruner configuration and part of the
// compaction/prune contract: a recorded replacement must equal
// PruneToolOutput of the text it replaces.
const (
	// PruneThreshold is the largest output kept whole.
	PruneThreshold = 8192
	// PruneHead is the number of leading code points a pruned output keeps.
	PruneHead = 4096
	// PruneTail is the number of trailing code points a pruned output keeps.
	PruneTail = 1024
	// PruneMarker replaces the removed middle.
	PruneMarker = "\n\n[... tool result middle pruned ...]\n\n"
)

// ToolResultPrune is the bounded replacement text compaction recorded for
// one visible tool result. Seq names the tool/result event whose output it
// replaces on the model surface; that event stays in the log unchanged.
type ToolResultPrune struct {
	Seq    uint64 `json:"seq"`
	Output string `json:"output"`
}

// PruneToolOutput returns text's first PruneHead and last PruneTail code
// points joined by PruneMarker, and false when text has at most
// PruneThreshold code points and is kept whole. Slicing by code point never
// splits a UTF-8 sequence, though a grapheme cluster may be cut; each byte
// of an invalid sequence counts as one code point, as utf8 decoding does.
func PruneToolOutput(text string) (string, bool) {
	count := utf8.RuneCountInString(text)
	if count <= PruneThreshold {
		return "", false
	}
	headEnd, tailStart, index := 0, 0, 0
	for offset := range text {
		if index == PruneHead {
			headEnd = offset
		}
		if index == count-PruneTail {
			tailStart = offset
			break
		}
		index++
	}
	return text[:headEnd] + PruneMarker + text[tailStart:], true
}

// applyPrune replaces the output of the visible tool result named by a
// compaction/prune record after checking that the recorded text is exactly
// the pruning of that output.
func applyPrune(nodes []SurfaceNode, event Event) error {
	data := event.Record.Prune
	for index := range nodes {
		node := &nodes[index]
		if node.Result == nil || node.Sequence != data.Seq {
			continue
		}
		pruned, ok := PruneToolOutput(node.Result.Output)
		if !ok || pruned != data.Output {
			return fmt.Errorf("%w: compaction/prune %d does not prune tool result %d", ErrInvalidRecord, event.Sequence, data.Seq)
		}
		node.Result.Output = data.Output
		return nil
	}
	return fmt.Errorf("%w: compaction/prune %d names no visible tool result %d", ErrInvalidRecord, event.Sequence, data.Seq)
}

func (record Record) requirePrune() error {
	if record.Step != 0 || record.Prune == nil || record.hasExtras("prune") {
		return invalid("compaction/prune shape is invalid")
	}
	if record.Prune.Seq == 0 || len(record.Prune.Output) > MaxTextBytes {
		return invalid("compaction/prune fields are invalid")
	}
	return nil
}

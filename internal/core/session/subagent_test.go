package session

import "testing"

func catalogEvent(seq uint64, id string) Event {
	return Event{Sequence: seq, Record: Record{Type: RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &SubagentCatalog{SessionID: id, Mode: SubagentContinuable, Label: id}}}
}

func descriptorEvent(seq, inherited uint64) Event {
	return Event{Sequence: seq, Record: Record{Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentFork, Mode: SubagentOneShot, Label: "fork", Inherited: inherited}}}
}

func TestChildren_ExcludesForkInheritedEntries(t *testing.T) {
	root := []Event{catalogEvent(1, "first"), catalogEvent(2, "second")}
	if children := Children(root); len(children) != 2 || children[0].SessionID != "first" || children[1].SessionID != "second" {
		t.Fatalf("root children = %#v", children)
	}
	if Children(nil) != nil {
		t.Fatal("empty log has children")
	}
	// A fork of a fork inherits both earlier descriptors; only entries after
	// its own (last) descriptor are its children.
	forked := []Event{catalogEvent(1, "parent-child"), descriptorEvent(2, 1), catalogEvent(3, "grand"), descriptorEvent(4, 3), catalogEvent(5, "own")}
	if children := Children(forked); len(children) != 1 || children[0].SessionID != "own" {
		t.Fatalf("forked children = %#v", children)
	}
	if own := OwnEvents(forked); len(own) != 2 || own[0].Sequence != 4 {
		t.Fatalf("own events = %#v", own)
	}
}

func TestFinalAssistantText_PrefersLastMessageThenStreamedText(t *testing.T) {
	message := func(seq uint64, text string) Event {
		return Event{Sequence: seq, Record: Record{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: textMessage(RoleAssistant, text)}}
	}
	chunk := func(seq uint64, kind ChunkKind, text string) Event {
		return Event{Sequence: seq, Record: Record{Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: kind, Text: text}}}
	}
	for name, test := range map[string]struct {
		events []Event
		want   string
	}{
		"none":            {nil, ""},
		"last non-empty":  {[]Event{message(1, "first"), message(2, "second"), message(3, "")}, "second"},
		"partial stream":  {[]Event{chunk(1, ChunkText, "cut "), chunk(2, ChunkReasoning, "thinking"), chunk(3, ChunkText, "off")}, "cut off"},
		"message wins":    {[]Event{chunk(1, ChunkText, "draft"), message(2, "final")}, "final"},
		"empty message":   {[]Event{chunk(1, ChunkText, "kept"), message(2, "")}, "kept"},
		"reasoning alone": {[]Event{chunk(1, ChunkReasoning, "hidden")}, ""},
	} {
		if got := FinalAssistantText(test.events); got != test.want {
			t.Errorf("%s: FinalAssistantText = %q, want %q", name, got, test.want)
		}
	}
}

func TestLastOutcome_ReportsLatestClosedTurn(t *testing.T) {
	if _, ok := LastOutcome([]Event{{Sequence: 1, Record: Record{Type: RecordTurnStart, Turn: 1}}}); ok {
		t.Fatal("open turn reported an outcome")
	}
	events := []Event{
		{Sequence: 1, Record: Record{Type: RecordTurnEnd, Turn: 1, Outcome: OutcomeCompleted}},
		{Sequence: 2, Record: Record{Type: RecordTurnEnd, Turn: 2, Outcome: OutcomeCanceled}},
		{Sequence: 3, Record: Record{Type: RecordApprovalPolicy, Approval: &ApprovalData{Policy: ApprovalNever}}},
	}
	if outcome, ok := LastOutcome(events); !ok || outcome != OutcomeCanceled {
		t.Fatalf("LastOutcome = %q %t", outcome, ok)
	}
}

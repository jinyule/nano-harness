package session

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func textMessage(role MessageRole, text string) *Message {
	return &Message{Role: role, Source: MessageSource{Kind: "test"}, Content: []ContentBlock{{Type: ContentText, Text: text}}}
}

func testImage() *Image {
	data := []byte("jpeg-data")
	digest := sha256.Sum256(data)
	return &Image{ID: "img", Name: "x.jpg", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data), SHA256: hex.EncodeToString(digest[:]), Width: 1, Height: 1}
}

func TestRecordValidate_AllKinds(t *testing.T) {
	call := &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{"x":1}`)}
	result := &ToolResult{CallID: "call", Output: "ok"}
	header := &RequestHeader{Provider: "openai", Model: "model", System: "system", ContextWindow: 8192, Tools: []ToolDefinition{{Name: "tool", Description: "does work", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	valid := []Record{
		{Type: RecordTurnStart, Turn: 1},
		{Type: RecordUserMessage, Turn: 1, Message: textMessage(RoleUser, "hello")},
		{Type: RecordStepStart, Turn: 1, Step: 1},
		{Type: RecordRequestHeader, Turn: 1, Step: 1, Header: header},
		{Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: ChunkText, Text: "x"}},
		{Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: ChunkReasoning, Text: "x"}},
		{Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: ChunkTool, Index: 1, CallID: "call", Name: "tool", Arguments: "{}"}},
		{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: textMessage(RoleAssistant, "answer")},
		{Type: RecordToolCall, Turn: 1, Step: 1, Call: call},
		{Type: RecordApprovalAsked, Turn: 1, Step: 1, Approval: &ApprovalData{ID: "approval", CallID: "call", ToolName: "tool", Reason: "write"}},
		{Type: RecordApprovalDecided, Turn: 1, Step: 1, Approval: &ApprovalData{ID: "approval", Outcome: ApprovalAllowedOnce}},
		{Type: RecordApprovalPolicy, Approval: &ApprovalData{Policy: ApprovalNever, Source: "delegation"}},
		{Type: RecordToolResult, Turn: 1, Step: 1, Result: result},
		{Type: RecordRetry, Turn: 1, Step: 1, Retry: &RetryData{ID: "retry", Provider: "openai", PolicyKey: "route", Attempt: 1, MaxRetries: 2, DelayMS: 1, Failure: "server"}},
		{Type: RecordRetryStarted, Turn: 1, Step: 1, Retry: &RetryData{ID: "retry", Attempt: 1}},
		{Type: RecordCompactionStart, Compaction: &CompactionData{ID: "compact"}},
		{Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: ContentText, Text: "summary"}}, Provider: "openai", Model: "model"}},
		{Type: RecordCompactionEnd, Compaction: &CompactionData{ID: "compact", Error: "failure"}},
		{Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 1, Provider: "in-process", Mode: "continuable", Label: "worker", Tools: []string{"tool"}}},
		{Type: RecordStepEnd, Turn: 1, Step: 1, Usage: &TokenUsage{InputTokens: 1, OutputTokens: 1}},
		{Type: RecordTurnEnd, Turn: 1, Outcome: OutcomeCompleted},
	}
	for _, record := range valid {
		if err := record.Validate(); err != nil {
			t.Errorf("%s: %v", record.Type, err)
		}
	}
	imageMessage := &Message{Role: RoleUser, Source: MessageSource{Kind: "user"}, Content: []ContentBlock{{Type: ContentImage, Image: testImage()}}}
	if err := (Record{Type: RecordUserMessage, Turn: 1, Message: imageMessage}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []TurnOutcome{OutcomeCanceled, OutcomeError, OutcomeStepLimit, OutcomeInterrupted} {
		if err := (Record{Type: RecordTurnEnd, Turn: 1, Outcome: outcome}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, outcome := range []ApprovalOutcome{ApprovalRejected, ApprovalCancelled, ApprovalUnavailable} {
		if err := (Record{Type: RecordApprovalDecided, Turn: 1, Step: 1, Approval: &ApprovalData{ID: "a", Outcome: outcome}}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSurfaceCloneAndText(t *testing.T) {
	call := &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}
	result := &ToolResult{CallID: "call", Output: "result"}
	events := []Event{
		{Sequence: 1, Record: Record{Type: RecordUserMessage, Turn: 1, Message: textMessage(RoleUser, "old")}},
		{Sequence: 2, Record: Record{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: textMessage(RoleAssistant, "answer")}},
		{Sequence: 3, Record: Record{Type: RecordToolCall, Turn: 1, Step: 1, Call: call}},
		{Sequence: 4, Record: Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: result}},
		{Sequence: 5, Record: Record{Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "c", ShadowedSeqs: []uint64{1, 2}, ShadowedTokenCount: 2, Summary: []ContentBlock{{Type: ContentText, Text: "summary"}}, Provider: "p", Model: "m"}}},
	}
	surface, err := Surface(events)
	if err != nil || len(surface) != 3 || Text(*surface[0].Message) != "summary" {
		t.Fatalf("surface=%#v err=%v", surface, err)
	}
	cloned := CloneEvent(events[4])
	cloned.Record.Compaction.Summary[0].Text = "changed"
	if events[4].Record.Compaction.Summary[0].Text != "summary" {
		t.Fatal("CloneEvent aliases source")
	}
	copySurface := cloneSurface(surface)
	copySurface[0].Message.Content[0].Text = "changed"
	if Text(*surface[0].Message) != "summary" {
		t.Fatal("cloneSurface aliases source")
	}
	if _, err := Surface([]Event{{Sequence: 1, Record: events[4].Record}}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("missing shadow error=%v", err)
	}
	if !slices.Equal(cloneContent(nil), []ContentBlock(nil)) || cloneMessage(nil) != nil {
		t.Fatal("nil clones changed")
	}
}

func TestRecordValidateRejectsEveryInvalidShape(t *testing.T) {
	validMessage := textMessage(RoleUser, "hello")
	validCall := &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}
	validResult := &ToolResult{CallID: "call", Output: "ok"}
	validHeader := &RequestHeader{Provider: "p", Model: "m", Tools: []ToolDefinition{{Name: "tool", Description: "description", Parameters: json.RawMessage(`{}`)}}}
	validApproval := &ApprovalData{ID: "approval", CallID: "call", ToolName: "tool", Reason: "reason"}
	validRetry := &RetryData{ID: "retry", Provider: "p", PolicyKey: "route", Attempt: 1, Failure: "server"}
	validCompaction := &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: ContentText, Text: "summary"}}, Provider: "p", Model: "m"}
	cases := map[string]Record{
		"missing type":              {},
		"missing turn":              {Type: RecordTurnStart},
		"unknown type":              {Type: "unknown", Turn: 1},
		"bare step":                 {Type: RecordTurnStart, Turn: 1, Step: 1},
		"bare extras":               {Type: RecordTurnStart, Turn: 1, Message: validMessage},
		"negative usage":            {Type: RecordStepEnd, Turn: 1, Step: 1, Usage: &TokenUsage{InputTokens: -1}},
		"message shape":             {Type: RecordUserMessage, Turn: 1},
		"message count":             {Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Source: MessageSource{Kind: "test"}}},
		"message source":            {Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "x"}}}},
		"message content":           {Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Source: MessageSource{Kind: "test"}, Content: []ContentBlock{{Type: "bad"}}}},
		"header shape":              {Type: RecordRequestHeader, Turn: 1, Header: validHeader},
		"header provider":           {Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "", Model: "m"}},
		"header model":              {Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: ""}},
		"header limits":             {Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m", ContextWindow: -1}},
		"header duplicate tool":     {Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m", Tools: []ToolDefinition{{Name: "t", Description: "x", Parameters: json.RawMessage(`{}`)}, {Name: "t", Description: "x", Parameters: json.RawMessage(`{}`)}}}},
		"header bad tool":           {Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m", Tools: []ToolDefinition{{Name: "", Description: "x", Parameters: json.RawMessage(`{}`)}}}},
		"chunk shape":               {Type: RecordAssistantChunk, Turn: 1, Step: 1},
		"chunk limits":              {Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: ChunkText, Text: strings.Repeat("x", MaxArgumentsBytes+1)}},
		"text chunk fields":         {Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: ChunkText, Text: "x", CallID: "c"}},
		"tool chunk text":           {Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: ChunkTool, Text: "x"}},
		"unknown chunk":             {Type: RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &AssistantChunk{Kind: "bad"}},
		"call shape":                {Type: RecordToolCall, Turn: 1, Step: 1},
		"call ID":                   {Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{Name: "tool", Arguments: json.RawMessage(`{}`)}},
		"call name":                 {Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{ID: "call", Arguments: json.RawMessage(`{}`)}},
		"call arguments":            {Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{ID: "call", Name: "tool"}},
		"call arguments array":      {Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`[]`)}},
		"result shape":              {Type: RecordToolResult, Turn: 1, Step: 1},
		"result call ID":            {Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{}},
		"result output":             {Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call", Output: strings.Repeat("x", MaxTextBytes+1)}},
		"approval shape":            {Type: RecordApprovalAsked, Turn: 1, Step: 1},
		"approval asked":            {Type: RecordApprovalAsked, Turn: 1, Step: 1, Approval: &ApprovalData{}},
		"approval decided":          {Type: RecordApprovalDecided, Turn: 1, Step: 1, Approval: validApproval},
		"approval policy":           {Type: RecordApprovalPolicy, Approval: &ApprovalData{Policy: "bad"}},
		"retry shape":               {Type: RecordRetry, Turn: 1, Step: 1},
		"retry identity":            {Type: RecordRetry, Turn: 1, Step: 1, Retry: &RetryData{}},
		"retry started extras":      {Type: RecordRetryStarted, Turn: 1, Step: 1, Retry: validRetry},
		"retry fields":              {Type: RecordRetry, Turn: 1, Step: 1, Retry: &RetryData{ID: "retry", Provider: "p", Attempt: 1}},
		"compaction shape":          {Type: RecordCompactionStart},
		"compaction ID":             {Type: RecordCompactionStart, Compaction: &CompactionData{}},
		"compaction start extras":   {Type: RecordCompactionStart, Compaction: validCompaction},
		"compaction summary fields": {Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "compact"}},
		"compaction shadow seq":     {Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{0}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: ContentText, Text: "s"}}, Provider: "p", Model: "m"}},
		"compaction content":        {Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: "bad"}}, Provider: "p", Model: "m"}},
		"compaction end fields":     {Type: RecordCompactionEnd, Compaction: &CompactionData{ID: "compact", Provider: "p"}},
		"subagent shape":            {Type: RecordSubagentDescriptor},
		"subagent fields":           {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{}},
		"subagent tool":             {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 1, Provider: "in-process", Mode: "continuable", Label: "worker", Tools: []string{""}}},
		"turn end extras":           {Type: RecordTurnEnd, Turn: 1, Outcome: OutcomeCompleted, Result: validResult},
		"turn end outcome":          {Type: RecordTurnEnd, Turn: 1, Outcome: "bad"},
		"message extras":            {Type: RecordUserMessage, Turn: 1, Message: validMessage, Call: validCall},
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	contentFailures := []ContentBlock{
		{Type: ContentText, Image: testImage()},
		{Type: ContentImage, Text: "x", Image: testImage()},
		{Type: "unknown"},
	}
	for _, block := range contentFailures {
		if validateContent(block) == nil {
			t.Fatalf("content accepted: %#v", block)
		}
	}
	images := []Image{
		{},
		{ID: "id", Name: "", MediaType: "image/jpeg", Width: 1, Height: 1, SHA256: strings.Repeat("0", 64), Data: "eA=="},
		{ID: "id", Name: "x", MediaType: "image/gif", Width: 1, Height: 1, SHA256: strings.Repeat("0", 64), Data: "eA=="},
		{ID: "id", Name: "x", MediaType: "image/jpeg", Width: 0, Height: 1, SHA256: strings.Repeat("0", 64), Data: "eA=="},
		{ID: "id", Name: "x", MediaType: "image/jpeg", Width: 1, Height: 1, SHA256: strings.Repeat("0", 64), Data: "!"},
		{ID: "id", Name: "x", MediaType: "image/jpeg", Width: 1, Height: 1, SHA256: strings.Repeat("0", 64), Data: "eA=="},
	}
	for _, image := range images {
		if validateImage(image) == nil {
			t.Fatalf("image accepted: %#v", image)
		}
	}
	toolFailures := []ToolDefinition{
		{Name: "tool", Description: "", Parameters: json.RawMessage(`{}`)},
		{Name: "tool", Description: "description", Parameters: nil},
		{Name: "tool", Description: "description", Parameters: json.RawMessage(`[]`)},
	}
	for _, tool := range toolFailures {
		if validateToolDefinition(tool) == nil {
			t.Fatalf("tool accepted: %#v", tool)
		}
	}
	if validateIdentifier("id", " bad ", 10) == nil {
		t.Fatal("untrimmed identifier accepted")
	}
}

func TestCloneEventDetachesEveryMutableField(t *testing.T) {
	event := Event{Sequence: 1, Record: Record{
		Message:    &Message{Role: RoleUser, Source: MessageSource{Kind: "user"}, Content: []ContentBlock{{Type: ContentImage, Image: testImage()}}},
		Chunk:      &AssistantChunk{Kind: ChunkText, Text: "x"},
		Call:       &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)},
		Result:     &ToolResult{CallID: "call", Output: "ok"},
		Header:     &RequestHeader{Provider: "p", Model: "m", Tools: []ToolDefinition{{Name: "tool", Parameters: json.RawMessage(`{}`)}}},
		Usage:      &TokenUsage{InputTokens: 1},
		Retry:      &RetryData{ID: "retry"},
		Approval:   &ApprovalData{ID: "approval"},
		Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, Summary: []ContentBlock{{Type: ContentImage, Image: testImage()}}},
		Subagent:   &SubagentDescriptor{Version: 1, Tools: []string{"tool"}},
	}}
	cloned := CloneEvent(event)
	cloned.Record.Message.Content[0].Image.Name = "changed"
	cloned.Record.Chunk.Text = "changed"
	cloned.Record.Call.Arguments[0] = '['
	cloned.Record.Result.Output = "changed"
	cloned.Record.Header.Tools[0].Parameters[0] = '['
	cloned.Record.Usage.InputTokens = 2
	cloned.Record.Retry.ID = "changed"
	cloned.Record.Approval.ID = "changed"
	cloned.Record.Compaction.ShadowedSeqs[0] = 2
	cloned.Record.Compaction.Summary[0].Image.Name = "changed"
	cloned.Record.Subagent.Tools[0] = "changed"
	if event.Record.Message.Content[0].Image.Name == "changed" || event.Record.Chunk.Text == "changed" || event.Record.Call.Arguments[0] == '[' || event.Record.Result.Output == "changed" || event.Record.Header.Tools[0].Parameters[0] == '[' || event.Record.Usage.InputTokens == 2 || event.Record.Retry.ID == "changed" || event.Record.Approval.ID == "changed" || event.Record.Compaction.ShadowedSeqs[0] == 2 || event.Record.Compaction.Summary[0].Image.Name == "changed" || event.Record.Subagent.Tools[0] == "changed" {
		t.Fatal("CloneEvent aliases source")
	}
}

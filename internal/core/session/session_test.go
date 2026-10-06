package session

import (
	"crypto/sha256"
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
	digest := sha256.Sum256([]byte("jpeg-data"))
	return &Image{ID: ImageID(hex.EncodeToString(digest[:])), Name: "x.jpg", MediaType: "image/jpeg", Bytes: 9, Width: 1, Height: 1}
}

func TestRecordValidate_AllKinds(t *testing.T) {
	call := &ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{"x":1}`)}
	result := &ToolResult{CallID: "call", Output: "ok"}
	header := &RequestHeader{Provider: "openai", Model: "model", Effort: EffortMax, System: "system", ContextWindow: 8192, Tools: []ToolDefinition{{Name: "tool", Description: "does work", Parameters: json.RawMessage(`{"type":"object"}`)}}}
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
		{Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: ContentText, Text: "summary"}}, Provider: "openai", Model: "model", Effort: EffortMax}},
		{Type: RecordCompactionEnd, Compaction: &CompactionData{ID: "compact", Error: "failure"}},
		{Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentSpawn, Mode: SubagentContinuable, Label: "worker", Tools: []string{"tool"}}},
		{Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentFork, Mode: SubagentOneShot, Label: strings.Repeat("x", 128<<10), Inherited: 9}},
		{Type: RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &SubagentCatalog{SessionID: "child", Mode: SubagentOneShot, Label: "worker"}},
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
	imageResult := &ToolResult{CallID: "call", Output: "<type>image</type>", Image: testImage()}
	if err := (Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: imageResult}).Validate(); err != nil {
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
	result := &ToolResult{CallID: "call", Output: "result", Image: testImage()}
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
	copySurface[2].Result.Image.Name = "changed"
	if Text(*surface[0].Message) != "summary" || surface[2].Result.Image.Name != "x.jpg" {
		t.Fatal("cloneSurface aliases source")
	}
	surface[2].Result.Image.Name = "folded"
	if result.Image.Name != "x.jpg" {
		t.Fatal("Surface aliases the committed result image")
	}
	if _, err := Surface([]Event{{Sequence: 1, Record: events[4].Record}}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("missing shadow error=%v", err)
	}
	if !slices.Equal(cloneContent(nil), []ContentBlock(nil)) || cloneMessage(nil) != nil || cloneResult(nil) != nil {
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
	validCompaction := &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: ContentText, Text: "summary"}}, Provider: "p", Model: "m", Effort: EffortMax}
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
		"header effort":             {Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m", Effort: "extreme"}},
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
		"result error image":        {Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call", Output: "Error: x", IsError: true, Image: testImage()}},
		"result image ID":           {Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call", Output: "ok", Image: &Image{ID: "img", Name: "x.jpg", MediaType: "image/jpeg", Bytes: 1, Width: 1, Height: 1}}},
		"approval shape":            {Type: RecordApprovalAsked, Turn: 1, Step: 1},
		"approval asked":            {Type: RecordApprovalAsked, Turn: 1, Step: 1, Approval: &ApprovalData{}},
		"approval decided":          {Type: RecordApprovalDecided, Turn: 1, Step: 1, Approval: validApproval},
		"decision call reference":   {Type: RecordApprovalDecided, Turn: 1, Step: 1, Approval: &ApprovalData{ID: "approval", CallID: "other", Outcome: ApprovalAllowedOnce}},
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
		"compaction effort":         {Type: RecordCompactionSummary, Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []ContentBlock{{Type: ContentText, Text: "s"}}, Provider: "p", Model: "m", Effort: "extreme"}},
		"compaction end fields":     {Type: RecordCompactionEnd, Compaction: &CompactionData{ID: "compact", Provider: "p"}},
		"subagent shape":            {Type: RecordSubagentDescriptor},
		"subagent fields":           {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{}},
		"subagent tool":             {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentSpawn, Mode: SubagentContinuable, Label: "worker", Tools: []string{""}}},
		"subagent version 1":        {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 1, Provider: SubagentSpawn, Mode: SubagentContinuable, Label: "worker"}},
		"subagent old provider":     {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: "in-process", Mode: SubagentContinuable, Label: "worker"}},
		"subagent spawn inherits":   {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentSpawn, Mode: SubagentContinuable, Label: "worker", Inherited: 1}},
		"subagent mode":             {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentFork, Mode: "resident", Label: "worker"}},
		"subagent long label":       {Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentFork, Mode: SubagentOneShot, Label: strings.Repeat("x", (128<<10)+1)}},
		"subagent step":             {Type: RecordSubagentDescriptor, Step: 1, Subagent: &SubagentDescriptor{Version: 2, Provider: SubagentSpawn, Mode: SubagentOneShot, Label: "worker"}},
		"catalog shape":             {Type: RecordSubagentCatalog, Turn: 1, Step: 1},
		"catalog step":              {Type: RecordSubagentCatalog, Turn: 1, Catalog: &SubagentCatalog{SessionID: "child", Mode: SubagentOneShot, Label: "worker"}},
		"catalog turn":              {Type: RecordSubagentCatalog, Step: 1, Catalog: &SubagentCatalog{SessionID: "child", Mode: SubagentOneShot, Label: "worker"}},
		"catalog extras":            {Type: RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &SubagentCatalog{SessionID: "child", Mode: SubagentOneShot, Label: "worker"}, Message: validMessage},
		"catalog session":           {Type: RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &SubagentCatalog{SessionID: " child", Mode: SubagentOneShot, Label: "worker"}},
		"catalog mode":              {Type: RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &SubagentCatalog{SessionID: "child", Mode: "unknown", Label: "worker"}},
		"catalog label":             {Type: RecordSubagentCatalog, Turn: 1, Step: 1, Catalog: &SubagentCatalog{SessionID: "child", Mode: SubagentContinuable, Label: strings.Repeat("x", (128<<10)+1)}},
		"bare catalog":              {Type: RecordTurnStart, Turn: 1, Catalog: &SubagentCatalog{}},
		"turn end extras":           {Type: RecordTurnEnd, Turn: 1, Outcome: OutcomeCompleted, Result: validResult},
		"turn end outcome":          {Type: RecordTurnEnd, Turn: 1, Outcome: "bad"},
		"message extras":            {Type: RecordUserMessage, Turn: 1, Message: validMessage, Call: validCall},
	}
	for _, effort := range []Effort{EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax} {
		if !ValidEffort(effort) {
			t.Fatalf("valid effort rejected: %s", effort)
		}
	}
	if ValidEffort("") || ValidEffort("extreme") {
		t.Fatal("invalid effort accepted")
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
	valid := *testImage()
	images := []Image{}
	for _, mutate := range []func(*Image){
		func(image *Image) { image.ID = "" },
		func(image *Image) { image.ID = "sha256:" + strings.Repeat("0", 63) },
		func(image *Image) { image.ID = "sha256:" + strings.Repeat("A", 64) },
		func(image *Image) { image.ID = "md5:" + strings.Repeat("0", 64) },
		func(image *Image) { image.Name = "" },
		func(image *Image) { image.Name = "a\nb" },
		func(image *Image) { image.MediaType = "image/gif" },
		func(image *Image) { image.Bytes = 0 },
		func(image *Image) { image.Bytes = MaxImageBytes + 1 },
		func(image *Image) { image.Width = 0 },
		func(image *Image) { image.Height = 4097 },
	} {
		image := valid
		mutate(&image)
		images = append(images, image)
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
		Result:     &ToolResult{CallID: "call", Output: "ok", Image: testImage()},
		Header:     &RequestHeader{Provider: "p", Model: "m", Tools: []ToolDefinition{{Name: "tool", Parameters: json.RawMessage(`{}`)}}},
		Usage:      &TokenUsage{InputTokens: 1},
		Retry:      &RetryData{ID: "retry"},
		Approval:   &ApprovalData{ID: "approval"},
		Compaction: &CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, Summary: []ContentBlock{{Type: ContentImage, Image: testImage()}}},
		Subagent:   &SubagentDescriptor{Version: 1, Tools: []string{"tool"}},
		Catalog:    &SubagentCatalog{SessionID: "child"},
	}}
	cloned := CloneEvent(event)
	cloned.Record.Message.Content[0].Image.Name = "changed"
	cloned.Record.Chunk.Text = "changed"
	cloned.Record.Call.Arguments[0] = '['
	cloned.Record.Result.Output = "changed"
	cloned.Record.Result.Image.Name = "changed"
	cloned.Record.Header.Tools[0].Parameters[0] = '['
	cloned.Record.Usage.InputTokens = 2
	cloned.Record.Retry.ID = "changed"
	cloned.Record.Approval.ID = "changed"
	cloned.Record.Compaction.ShadowedSeqs[0] = 2
	cloned.Record.Compaction.Summary[0].Image.Name = "changed"
	cloned.Record.Subagent.Tools[0] = "changed"
	cloned.Record.Catalog.SessionID = "changed"
	if event.Record.Catalog.SessionID == "changed" {
		t.Fatal("CloneEvent aliases the catalog entry")
	}
	if event.Record.Message.Content[0].Image.Name == "changed" || event.Record.Chunk.Text == "changed" || event.Record.Call.Arguments[0] == '[' || event.Record.Result.Output == "changed" || event.Record.Result.Image.Name == "changed" || event.Record.Header.Tools[0].Parameters[0] == '[' || event.Record.Usage.InputTokens == 2 || event.Record.Retry.ID == "changed" || event.Record.Approval.ID == "changed" || event.Record.Compaction.ShadowedSeqs[0] == 2 || event.Record.Compaction.Summary[0].Image.Name == "changed" || event.Record.Subagent.Tools[0] == "changed" {
		t.Fatal("CloneEvent aliases source")
	}
}

func TestImageID_RoundTripsOnlyCanonicalDigests(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	if got, ok := ImageDigest(ImageID(digest)); !ok || got != digest {
		t.Fatalf("round trip = %q %v", got, ok)
	}
	for _, id := range []string{"", digest, "sha256:", "sha256:" + digest[:63], "sha256:" + digest + "0", "sha256:" + strings.ToUpper(digest), "sha256:" + strings.Repeat("g", 64)} {
		if _, ok := ImageDigest(id); ok {
			t.Errorf("ImageDigest(%q) accepted", id)
		}
	}
}

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// sizedImage is a schema-valid image whose data decodes to size bytes.
func sizedImage(size int) *session.Image {
	data := make([]byte, size)
	digest := sha256.Sum256(data)
	return &session.Image{ID: "img-test", Name: "shot.png", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data), SHA256: hex.EncodeToString(digest[:]), Width: 1, Height: 1}
}

func imageMessage(text string, image *session.Image) session.Message {
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}, {Type: session.ContentImage, Image: image}}}
}

func encodedNeed(t *testing.T, record session.Record) int64 {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(encoded)) + recordFramingBytes
}

func TestImageRoom_KeepsTheReserveFree(t *testing.T) {
	log, memory := turnJournal()
	record := session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &session.ToolResult{CallID: "call", Output: "ok", Image: sizedImage(1000)}}
	need := encodedNeed(t, record)
	memory.remaining = imageReserveBytes + need
	if got, available, fits := imageRoom(log, record); !fits || got != need || available != need {
		t.Fatalf("exact fit = %d/%d %v", got, available, fits)
	}
	memory.remaining = imageReserveBytes + need - 1
	if _, available, fits := imageRoom(log, record); fits || available != need-1 {
		t.Fatalf("one byte short = %d %v", available, fits)
	}
	memory.remaining = 10
	if _, available, fits := imageRoom(log, record); fits || available != 0 {
		t.Fatalf("inside the reserve = %d %v", available, fits)
	}
	if need, _, fits := imageRoom(log, session.Record{Type: session.RecordToolResult, Result: &session.ToolResult{Output: strings.Repeat("x", 1<<20)}}); !fits || need != 0 {
		t.Fatal("a text-only record was measured against the image reserve")
	}
	unencodable := session.Record{Result: &session.ToolResult{Image: sizedImage(1)}, Call: &session.ToolCall{Arguments: json.RawMessage("{")}}
	if _, _, fits := imageRoom(log, unencodable); fits {
		t.Fatal("an unencodable record fit")
	}
}

func TestFitImages_ReplaceOnlyWhatDoesNotFit(t *testing.T) {
	log, memory := turnJournal()
	result := session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &session.ToolResult{CallID: "call", Output: "envelope", Image: sizedImage(4000)}}
	message := imageMessage("look", sizedImage(4000))
	steer := session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}
	if fitResultImage(log, result).Result != result.Result || fitMessageImages(log, steer).Message != steer.Message {
		t.Fatal("fitting records were copied")
	}
	memory.remaining = imageReserveBytes + 100
	refused := fitResultImage(log, result).Result
	if refused.Image != nil || !refused.IsError || refused.CallID != "call" || !strings.HasPrefix(refused.Output, "Error: the image was not kept: it needs about ") || !strings.HasSuffix(refused.Output, "this session can hold only 100 more bytes of images; start a new session to read more images") {
		t.Fatalf("refused result = %+v", refused)
	}
	stripped := fitMessageImages(log, steer).Message
	if len(stripped.Content) != 2 || stripped.Content[0].Text != "look" || stripped.Content[1].Text != "[images omitted: this session cannot hold more images; start a new session to attach them]" {
		t.Fatalf("stripped message = %+v", stripped.Content)
	}
	if result.Result.Image == nil || len(message.Content) != 2 || message.Content[1].Image == nil {
		t.Fatal("fitting changed the original records")
	}
	err := checkMessageImages(log, message)
	if !errors.Is(err, ErrImageCapacity) || !strings.Contains(err.Error(), "this session can hold only 100 more bytes of images; start a new session to attach them") {
		t.Fatalf("message check = %v", err)
	}
}

func TestEngine_KeepsTurnsWorkingWhenImagesDoNotFit(t *testing.T) {
	call := session.ToolCall{ID: "call-1", Name: "look", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("looking", call)},
		modelAction{completion: assistantCompletion("seen")},
	)
	look := appTool.Define(appTool.Spec[struct{}]{Name: "look", Description: "returns an image", Execute: func(context.Context, appTool.Invocation, struct{}) (appTool.Result, error) {
		return appTool.Result{Text: "envelope", Image: sizedImage(64 << 10)}, nil
	}})
	toolScope := &plugin.Scope{}
	if err := harness.tools.Register(look, toolScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = toolScope.Close(context.Background()) })
	journal, log := turnJournal()
	log.remaining = imageReserveBytes + 32<<10
	steer := imageMessage("steer", sizedImage(64<<10))
	drained := false
	result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "first"), drain: func() []session.Message {
		if drained {
			return nil
		}
		drained = true
		return []session.Message{steer}
	}})
	if result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "seen" {
		t.Fatalf("turn = %+v", result)
	}
	var toolResult *session.ToolResult
	var steered *session.Message
	for _, event := range log.events {
		switch {
		case event.Record.Type == session.RecordToolResult:
			toolResult = event.Record.Result
		case event.Record.Type == session.RecordUserMessage && strings.HasPrefix(session.Text(*event.Record.Message), "steer"):
			steered = event.Record.Message
		}
	}
	if toolResult == nil || !toolResult.IsError || toolResult.Image != nil || !strings.HasPrefix(toolResult.Output, "Error: the image was not kept") {
		t.Fatalf("tool result = %+v", toolResult)
	}
	if steered == nil || slices.ContainsFunc(steered.Content, func(block session.ContentBlock) bool { return block.Type == session.ContentImage }) {
		t.Fatalf("steer = %+v", steered)
	}

	// An opening message whose images do not fit commits nothing.
	before := len(log.events)
	refused := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: imageMessage("attach", sizedImage(64<<10)), drain: noMessages})
	if !errors.Is(refused.Err, ErrImageCapacity) || refused.Turn != 0 || refused.Outcome != session.OutcomeError || len(log.events) != before {
		t.Fatalf("refused turn = %+v events %d -> %d", refused, before, len(log.events))
	}
}

func TestAgent_RefusesAttachmentsTheSessionCannotHold(t *testing.T) {
	harness := startEngineHarness(t, 1, modelAction{completion: assistantCompletion("done")})
	repository, policy := newMemoryRepository(), newMemoryPolicy()
	repository.newLog = func(options transcript.OpenOptions) *memoryLog {
		return &memoryLog{header: session.Header{SessionID: options.SessionID, Cwd: options.Cwd}, path: "/" + options.SessionID, remaining: imageReserveBytes + 1024}
	}
	registry, _ := startRegistry(t, harness, repository, policy)
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	message := imageMessage("look", sizedImage(64<<10))
	if _, err := root.Submit(context.Background(), message); !errors.Is(err, ErrImageCapacity) {
		t.Fatalf("Submit() error = %v", err)
	}
	if err := root.Steer(context.Background(), message); !errors.Is(err, ErrImageCapacity) {
		t.Fatalf("Steer() error = %v", err)
	}
	results, err := root.Submit(context.Background(), imageMessage("small", sizedImage(16)))
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("small attachment turn = %+v", result)
	}
}

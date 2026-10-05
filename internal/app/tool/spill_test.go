package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// memorySpill is an in-memory SpillStore that records every artifact.
type memorySpill struct {
	mu        sync.Mutex
	saved     []spilled
	createErr error
	writeErr  error
	commitErr error
	discarded int
	// locator overrides the generated locator when set.
	locator string
}

type spilled struct {
	session, name, content string
}

func (store *memorySpill) Create(_ context.Context, sessionID, name string) (SpillFile, error) {
	if store.createErr != nil {
		return nil, store.createErr
	}
	return &memoryFile{store: store, session: sessionID, name: name}, nil
}

type memoryFile struct {
	store         *memorySpill
	session, name string
	content       strings.Builder
}

func (file *memoryFile) Write(data []byte) (int, error) {
	if file.store.writeErr != nil {
		return 0, file.store.writeErr
	}
	return file.content.Write(data)
}

func (file *memoryFile) Locator() string { return "/spill/" + file.session + "/" + file.name }

func (file *memoryFile) Commit() (SpillRef, error) {
	if file.store.commitErr != nil {
		return SpillRef{}, file.store.commitErr
	}
	file.store.mu.Lock()
	defer file.store.mu.Unlock()
	file.store.saved = append(file.store.saved, spilled{file.session, file.name, file.content.String()})
	locator := file.Locator()
	if file.store.locator != "" {
		locator = file.store.locator
	}
	return SpillRef{Locator: locator, Bytes: file.content.Len(), Hint: "Use read."}, nil
}

func (file *memoryFile) Discard() error {
	file.store.discarded++
	return nil
}

func TestEstimateTokens_MatchesUpstreamDensity(t *testing.T) {
	for text, want := range map[string]int{"": 4, "abcd": 5, "abcde": 6, "😀": 5, "😀😀a": 6, "é": 5} {
		if got := estimateTokens(text); got != want {
			t.Errorf("estimateTokens(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestFitText_CutsOnRuneBoundariesWithinBudget(t *testing.T) {
	// A budget of 5 admits four UTF-16 units; the emoji needs two.
	for _, test := range []struct {
		text       string
		budget     int
		tail       bool
		want       string
		wantSuffix bool
	}{
		{text: "abcdef", budget: 5, want: "abcd"},
		{text: "abc😀d", budget: 5, want: "abc"},
		{text: "ab😀d", budget: 5, want: "ab😀"},
		{text: "abcdef", budget: 5, tail: true, want: "cdef"},
		{text: "a😀bcd", budget: 5, tail: true, want: "bcd"},
		{text: "abc", budget: 4, want: ""},
		{text: "abc", budget: 0, tail: true, want: ""},
		{text: "abc", budget: 100, tail: true, want: "abc"},
		{text: "abc", budget: 100, want: "abc"},
	} {
		if got := fitText(test.text, test.budget, test.tail); got != test.want {
			t.Errorf("fitText(%q, %d, %v) = %q, want %q", test.text, test.budget, test.tail, got, test.want)
		}
	}
}

func TestSpillPreview_MatchesUpstreamRetention(t *testing.T) {
	// Expected digests come from a line-by-line Python port of upstream's
	// spill-policy retention, which slices UTF-16 code units.
	ref := SpillRef{Locator: "/spill/s/abc-t.txt", Hint: "Use read."}
	for _, test := range []struct {
		name, text, digest, suffix string
		headUnits, tailUnits       int
	}{
		{
			name: "aligned", text: strings.Repeat("ab😀", 14000), headUnits: 24920, tailUnits: 24916,
			digest: "93d5bb6900ebbe0902f694535538ca824792bdc1680107abda938bf42422dfd3",
			suffix: "\n\n(Omitted 9246 bytes. Full formatted result stored at: /spill/s/abc-t.txt. Use read.)",
		},
		{
			name: "surrogate at the cut", text: "a" + strings.Repeat("ab😀", 14000), headUnits: 24919, tailUnits: 24916,
			digest: "56e4fea7e8553c4ed6d19a2e6ec93988cb5808ba3767872cd2ce2d2e165aaebf",
			suffix: "\n\n(Omitted 9250 bytes. Full formatted result stored at: /spill/s/abc-t.txt. Use read.)",
		},
		{
			name: "multibyte", text: strings.Repeat("é", 60000), headUnits: 24920, tailUnits: 24916,
			digest: "5aeb8633825fb6a678b8fb3c5d26982c47b2d424b4d8477a51416700c76ddb60",
			suffix: "\n\n(Omitted 20328 bytes. Full formatted result stored at: /spill/s/abc-t.txt. Use read.)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			preview, ok := spillPreview(test.text, ref, spillInlineTokens)
			sum := sha256.Sum256([]byte(preview))
			if !ok || hex.EncodeToString(sum[:]) != test.digest || !strings.HasSuffix(preview, test.suffix) {
				t.Fatalf("preview ok=%v digest=%x suffix=%q", ok, sum, preview[max(0, len(preview)-120):])
			}
			head, rest, _ := strings.Cut(preview, spillGap)
			tail, _, _ := strings.Cut(rest, "\n\n(Omitted")
			if utf16Length(head) != test.headUnits || utf16Length(tail) != test.tailUnits {
				t.Fatalf("head=%d tail=%d units", utf16Length(head), utf16Length(tail))
			}
			if estimateTokens(preview) > spillInlineTokens {
				t.Fatalf("preview costs %d tokens", estimateTokens(preview))
			}
		})
	}
	small := SpillRef{Locator: "/l", Hint: "H."}
	text := strings.Repeat("x", 400)
	for budget, want := range map[int]string{
		40: "xxxxxxxxxxxx" + spillGap + "xxxxxxxx\n\n(Omitted 380 bytes. Full formatted result stored at: /l. H.)",
		30: "(Omitted 400 bytes. Full formatted result stored at: /l. H.)",
		26: "(Omitted 400 bytes. Full formatted result stored at: /l. H.)",
	} {
		if got, ok := spillPreview(text, small, budget); !ok || got != want {
			t.Errorf("budget %d = %q, %v", budget, got, ok)
		}
	}
	if _, ok := spillPreview(text, small, 10); ok {
		t.Fatal("a notice larger than the budget replaced the content")
	}
}

func TestRuntime_SpillsOversizedResultsThroughTheRegisteredStore(t *testing.T) {
	runtime, _ := startRuntime(t, &fakeApprover{outcome: session.ApprovalAllowedOnce})
	store := &memorySpill{}
	scope := &plugin.Scope{}
	large := strings.Repeat("x", 60000)
	output := func(context.Context, Invocation) (Result, error) { return Text(large), nil }
	tools := []*Tool{
		simpleTool("big", false, "", output),
		simpleTool("small", false, "", func(context.Context, Invocation) (Result, error) { return Text("tiny"), nil }),
		simpleTool("failed", false, "", func(context.Context, Invocation) (Result, error) { return Result{}, errors.New(large) }),
		Define(Spec[noArguments]{Name: "inline", Description: "keeps its text", KeepInline: true, Execute: func(ctx context.Context, invocation Invocation, _ noArguments) (Result, error) {
			return output(ctx, invocation)
		}}),
	}
	for _, candidate := range tools {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	execute := func(sessionID string) []session.ToolResult {
		calls := make([]session.ToolCall, len(tools))
		for index, candidate := range tools {
			name := candidate.Definition().Name
			calls[index] = session.ToolCall{ID: name, Name: name, Arguments: json.RawMessage(`{}`)}
		}
		return runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: sessionID, Turn: 1, Step: 1, Calls: calls, Journal: fakeJournal{}})
	}
	// Without a store every result keeps its text.
	if results := execute("s"); results[0].Output != large {
		t.Fatalf("unspilled = %d bytes", len(results[0].Output))
	}
	spillScope := &plugin.Scope{}
	if err := runtime.UseSpill(store, spillScope); err != nil {
		t.Fatal(err)
	}
	results := execute("s")
	if !strings.HasPrefix(results[0].Output, strings.Repeat("x", 100)) || !strings.HasSuffix(results[0].Output, ". Full formatted result stored at: /spill/s/big.txt. Use read.)") || estimateTokens(results[0].Output) > spillInlineTokens {
		t.Fatalf("spilled = %q", results[0].Output[len(results[0].Output)-200:])
	}
	if results[1].Output != "tiny" || results[3].Output != large || !results[2].IsError || !strings.HasPrefix(results[2].Output, "Error: xxx") || len(results[2].Output) != len(large)+len("Error: ") {
		t.Fatalf("results = %d %q %d %d", len(results[0].Output), results[1].Output, len(results[2].Output), len(results[3].Output))
	}
	if len(store.saved) != 1 || store.saved[0] != (spilled{"s", "big.txt", large}) {
		t.Fatalf("saved = %d artifacts", len(store.saved))
	}
	// A call without a session or a failed save keeps the original text.
	if results := execute(""); results[0].Output != large {
		t.Fatal("a session-less call spilled")
	}
	store.commitErr = errors.New("disk full")
	if results := execute("s"); results[0].Output != large {
		t.Fatal("a failed save replaced the result")
	}
	// A locator too long for the notice to fit keeps the original as well.
	store.commitErr, store.locator = nil, strings.Repeat("/", 4*spillInlineTokens)
	if results := execute("s"); results[0].Output != large || len(store.saved) != 2 {
		t.Fatal("an unfittable notice replaced the result")
	}
	store.locator = ""
	if err := spillScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if results := execute("s"); results[0].Output != large || len(store.saved) != 2 {
		t.Fatal("the store stayed in use after its scope closed")
	}
}

func TestRuntime_UseSpillOwnsOneStorePerScope(t *testing.T) {
	store := &memorySpill{}
	inactive, _ := New(&fakeApprover{})
	if err := inactive.UseSpill(store, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive = %v", err)
	}
	runtime, runtimeScope := startRuntime(t, &fakeApprover{})
	if err := runtime.UseSpill(nil, &plugin.Scope{}); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("nil store = %v", err)
	}
	if err := runtime.UseSpill(store, nil); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("nil scope = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := runtime.UseSpill(store, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}
	first, second := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.UseSpill(store, first); err != nil {
		t.Fatalf("after rollback = %v", err)
	}
	if err := runtime.UseSpill(&memorySpill{}, second); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("second store = %v", err)
	}
	// Closing the runtime drops the store; a late scope close is harmless.
	if err := runtimeScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.spill != nil {
		t.Fatal("store retained after shutdown")
	}
}

func TestInvocation_SpillRequiresStoreAndSessionAndDiscardsFailedWrites(t *testing.T) {
	ctx := context.Background()
	if _, err := (Invocation{SessionID: "s"}).SaveText(ctx, "x.txt", "x"); !errors.Is(err, ErrSpillUnavailable) {
		t.Fatalf("no store = %v", err)
	}
	store := &memorySpill{}
	if _, err := (Invocation{spill: store}).CreateSpill(ctx, "x.txt"); !errors.Is(err, ErrSpillUnavailable) {
		t.Fatalf("no session = %v", err)
	}
	invocation := Invocation{SessionID: "s", spill: store}
	store.createErr = errors.New("create")
	if _, err := invocation.SaveText(ctx, "x.txt", "x"); !errors.Is(err, store.createErr) {
		t.Fatalf("create = %v", err)
	}
	store.createErr, store.writeErr = nil, errors.New("too large")
	if _, err := invocation.SaveText(ctx, "x.txt", "x"); !errors.Is(err, store.writeErr) || store.discarded != 1 {
		t.Fatalf("write = %v discarded=%d", err, store.discarded)
	}
	store.writeErr = nil
	if ref, err := invocation.SaveText(ctx, "x.txt", "héllo"); err != nil || ref.Bytes != 6 || ref.Locator != "/spill/s/x.txt" {
		t.Fatalf("saved = %+v, %v", ref, err)
	}
}

func TestRuntime_ImageResultsStayInlineAndCarryTheRoute(t *testing.T) {
	runtime, _ := startRuntime(t, &fakeApprover{outcome: session.ApprovalAllowedOnce})
	store := &memorySpill{}
	if err := runtime.UseSpill(store, &plugin.Scope{}); err != nil {
		t.Fatal(err)
	}
	data := []byte("jpeg")
	digest := sha256.Sum256(data)
	image := &session.Image{ID: session.ImageID(hex.EncodeToString(digest[:])), Name: "x.png", MediaType: "image/jpeg", Bytes: len(data), Width: 1, Height: 1}
	large := strings.Repeat("y", 60000)
	var seen Route
	scope := &plugin.Scope{}
	if err := runtime.Register(simpleTool("look", false, "", func(_ context.Context, invocation Invocation) (Result, error) {
		seen = invocation.Route
		return Result{Text: large + "\xff", Image: image}, nil
	}), scope); err != nil {
		t.Fatal(err)
	}
	route := Route{Provider: "openai", Model: "vision", ImageInput: true}
	results := runtime.ExecuteBatch(context.Background(), BatchRequest{
		SessionID: "s", Route: route, Turn: 1, Step: 1, Journal: fakeJournal{},
		Calls: []session.ToolCall{{ID: "call", Name: "look", Arguments: json.RawMessage(`{}`)}},
	})
	if seen != route {
		t.Fatalf("route = %+v", seen)
	}
	if len(store.saved) != 0 || results[0].Output != large+"�" || results[0].Image != image || results[0].IsError {
		t.Fatalf("result = %d bytes image=%v saved=%d", len(results[0].Output), results[0].Image, len(store.saved))
	}
	if err := (session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &results[0]}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRetainInline_KeepsTheNameHintWithinTheStoreAlphabet(t *testing.T) {
	store := &memorySpill{}
	long := strings.Repeat("t", 64)
	text := strings.Repeat("x", 60000)
	if got := retainInline(context.Background(), Invocation{SessionID: "s", spill: store}, long, text); got == text {
		t.Fatal("a 64-character tool name did not spill")
	}
	if name := store.saved[0].name; len(name) > 64 || name != strings.Repeat("t", 60)+".txt" {
		t.Fatalf("name hint = %q (%d bytes)", name, len(name))
	}
}

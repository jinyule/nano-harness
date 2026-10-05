package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/transcript"
	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

// parentPrefix returns a closed prefix of a real log, as a fork copies it.
func parentPrefix(t *testing.T, manager *Manager) []coresession.Event {
	t.Helper()
	parent, err := manager.Open(context.Background(), OpenOptions{SessionID: "parent", Create: true, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	appendClosedTurn(t, parent, 1)
	events, err := parent.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestOpen_SeedsANewSessionAtomically(t *testing.T) {
	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	seed := parentPrefix(t, manager)
	child, err := manager.OpenSession(context.Background(), transcript.OpenOptions{SessionID: "child", Create: true, Cwd: "/workspace", ParentSessionID: "parent", DelegationDepth: 1, Seed: seed})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := coresession.Record{Type: coresession.RecordSubagentDescriptor, Subagent: &coresession.SubagentDescriptor{Version: 2, Provider: coresession.SubagentFork, Mode: coresession.SubagentOneShot, Label: "fork", Inherited: uint64(len(seed))}}
	event, err := child.Append(context.Background(), descriptor)
	if err != nil || event.Sequence != uint64(len(seed)+1) {
		t.Fatalf("descriptor = %#v, %v", event, err)
	}
	if err := child.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The seed and the parent prefix are byte-identical event lines.
	parentBytes, _ := os.ReadFile(manager.root + "/parent.jsonl")
	childBytes, _ := os.ReadFile(manager.root + "/child.jsonl")
	parentLines, childLines := bytes.Split(parentBytes, []byte("\n")), bytes.Split(childBytes, []byte("\n"))
	for index := 1; index <= len(seed); index++ {
		if !bytes.Equal(parentLines[index], childLines[index]) {
			t.Fatalf("line %d differs:\n%s\n%s", index, parentLines[index], childLines[index])
		}
	}
	header, events, err := manager.Inspect(context.Background(), "child")
	if err != nil || header.ParentSessionID != "parent" || len(events) != len(seed)+1 {
		t.Fatalf("Inspect = %#v %d %v", header, len(events), err)
	}
	if own := coresession.OwnEvents(events); len(own) != 1 || own[0].Record.Type != coresession.RecordSubagentDescriptor {
		t.Fatalf("own events = %#v", own)
	}
}

func TestOpen_RejectsInvalidSeeds(t *testing.T) {
	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	seed := parentPrefix(t, manager)
	shifted := cloneEvents(seed)
	shifted[0].Sequence = 2
	invalid := cloneEvents(seed)
	invalid[1].Record.Message = nil
	for name, test := range map[string]struct {
		options OpenOptions
		want    error
	}{
		"resume":       {OpenOptions{SessionID: "child", Cwd: "/workspace", Seed: seed}, ErrInvalidConfig},
		"sequence gap": {OpenOptions{SessionID: "child", Create: true, Cwd: "/workspace", Seed: shifted}, ErrCorruptSession},
		"bad record":   {OpenOptions{SessionID: "child", Create: true, Cwd: "/workspace", Seed: invalid}, ErrCorruptSession},
		"open turn":    {OpenOptions{SessionID: "child", Create: true, Cwd: "/workspace", Seed: seed[:3]}, ErrCorruptSession},
	} {
		if _, err := manager.Open(context.Background(), test.options); !errors.Is(err, test.want) {
			t.Errorf("%s: Open = %v, want %v", name, err, test.want)
		}
	}
	if _, err := os.Stat(manager.root + "/child.jsonl"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected seed left a transcript: %v", err)
	}
}

func TestWriteHeader_BoundsAndEncodesSeed(t *testing.T) {
	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	seed := parentPrefix(t, manager)
	header := coresession.Header{SessionID: "child", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/workspace"}
	original := marshalJSON
	t.Cleanup(func() { marshalJSON = original })
	file, err := os.CreateTemp(t.TempDir(), "seed")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for name, test := range map[string]struct {
		marshal func(any) ([]byte, error)
		want    error
	}{
		"encode failure": {func(value any) ([]byte, error) {
			if _, ok := value.(coresession.Event); ok {
				return nil, errors.New("marshal")
			}
			return json.Marshal(value)
		}, nil},
		"record limit": {func(value any) ([]byte, error) {
			if _, ok := value.(coresession.Event); ok {
				return make([]byte, maxRecordBytes), nil
			}
			return json.Marshal(value)
		}, ErrSessionSize},
		"session limit": {func(value any) ([]byte, error) {
			if _, ok := value.(coresession.Event); ok {
				return make([]byte, maxSessionBytes/len(seed)+1), nil
			}
			return json.Marshal(value)
		}, ErrSessionSize},
	} {
		marshalJSON = test.marshal
		_, err := writeHeader(file, header, seed)
		if err == nil || test.want != nil && !errors.Is(err, test.want) {
			t.Errorf("%s: writeHeader = %v, want %v", name, err, test.want)
		}
	}
	if info, _ := file.Stat(); info.Size() != 0 {
		t.Fatalf("rejected seed wrote %d bytes", info.Size())
	}
}

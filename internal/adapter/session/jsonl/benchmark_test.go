package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

// BenchmarkSessionReplay measures strict disk read plus the retained model surface.
// Fixture creation is excluded; the filesystem cache is warm after the first sample.
func BenchmarkSessionReplay(b *testing.B) {
	for _, turns := range []int{10, 1000} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			manager, scope := startManager(b)
			b.Cleanup(func() {
				if err := scope.Close(context.Background()); err != nil {
					b.Error(err)
				}
			})
			fixture, err := os.ReadFile("testdata/session-v2.jsonl")
			if err != nil {
				b.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSpace(fixture), []byte{'\n'})
			var data bytes.Buffer
			data.Write(lines[0])
			data.WriteByte('\n')
			encoder := json.NewEncoder(&data)
			for turn := 1; turn <= turns; turn++ {
				for index, line := range lines[1:] {
					var event coresession.Event
					if err := json.Unmarshal(line, &event); err != nil {
						b.Fatal(err)
					}
					event.Sequence = uint64((turn-1)*11 + index + 1)
					event.Record.Turn = uint64(turn)
					if event.Record.Approval != nil {
						event.Record.Approval.ID = fmt.Sprintf("approval-%d", turn)
					}
					if err := encoder.Encode(event); err != nil {
						b.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(manager.config.Root, "fixture.jsonl"), data.Bytes(), 0o600); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(data.Len()))
			b.ResetTimer()
			for b.Loop() {
				_, events, err := manager.Inspect(b.Context(), "fixture")
				if err != nil {
					b.Fatal(err)
				}
				surface, err := coresession.Surface(events)
				if err != nil {
					b.Fatal(err)
				}
				if len(events) != 11*turns || len(surface) != 4*turns {
					b.Fatal("incomplete replay")
				}
				runtime.KeepAlive(events)
				runtime.KeepAlive(surface)
			}
		})
	}
}

// BenchmarkSessionDurableTurn includes create, eleven validated fsync appends,
// close and lock release. Removing the completed synthetic file is untimed.
func BenchmarkSessionDurableTurn(b *testing.B) {
	manager, scope := startManager(b)
	b.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			b.Error(err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		log, err := manager.Open(b.Context(), OpenOptions{SessionID: "benchmark", Create: true, Cwd: "/synthetic/workspace"})
		if err != nil {
			b.Fatal(err)
		}
		appendClosedTurn(b, log, 1)
		if err := log.Close(b.Context()); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := os.Remove(log.Path()); err != nil {
			b.Fatal(err)
		}
		if _, err := os.Stat(log.Path() + ".lock"); !os.IsNotExist(err) {
			b.Fatalf("writer lock remains: %v", err)
		}
		b.StartTimer()
	}
}

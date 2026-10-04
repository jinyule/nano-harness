package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// BenchmarkTranscriptUpdate measures event projection, wrapping and viewport
// rendering. It excludes terminal I/O, input latency, providers and app startup.
func BenchmarkTranscriptUpdate(b *testing.B) {
	for _, lines := range []int{100, 4000} {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			current := newModel(context.Background(), nil, nil)
			initial := make([]string, lines)
			for i := range initial {
				initial[i] = strings.Repeat("synthetic 中文 transcript ", 8)
			}
			event := session.Event{Sequence: 1, Record: session.Record{Type: session.RecordAssistantMessage, Message: &session.Message{Role: session.RoleAssistant, Content: []session.ContentBlock{{Type: session.ContentText, Text: "final marker"}}}}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				current.lines = append([]string(nil), initial...)
				current.refresh()
				b.StartTimer()
				current.applyEvent(event, true)
				current.refresh()
				rendered := current.viewport.View()
				if !strings.Contains(rendered, "final marker") {
					b.Fatal("latest transcript output missing")
				}
			}
		})
	}
}

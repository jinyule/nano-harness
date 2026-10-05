package job

import (
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// chunk is one retained append at an absolute byte offset.
type chunk struct {
	at      int64
	channel Channel
	data    []byte
}

// ring is one job's bounded output. Offsets are absolute and never move:
// trimming only advances earliest, so a reader whose cursor fell below it
// knows bytes were lost.
type ring struct {
	chunks   []chunk
	retained int
	total    int64
	earliest int64
}

func (ring *ring) append(channel Channel, data []byte, limit int) {
	ring.chunks = append(ring.chunks, chunk{at: ring.total, channel: channel, data: data})
	ring.total += int64(len(data))
	ring.retained += len(data)
	ring.trim(limit)
}

// trim drops whole head chunks until the ring fits limit; a single
// oversized chunk keeps only its tail, cut at a rune boundary.
func (ring *ring) trim(limit int) {
	for ring.retained > limit && len(ring.chunks) > 1 {
		ring.retained -= len(ring.chunks[0].data)
		ring.chunks = ring.chunks[1:]
	}
	if len(ring.chunks) == 1 && ring.retained > limit {
		single := &ring.chunks[0]
		cut := len(single.data) - limit
		for cut < len(single.data) && !utf8.RuneStart(single.data[cut]) {
			cut++
		}
		single.at += int64(cut)
		single.data = single.data[cut:]
		ring.retained = len(single.data)
	}
	ring.earliest = ring.total
	if len(ring.chunks) > 0 {
		ring.earliest = ring.chunks[0].at
	}
}

// readFrom joins every retained chunk at or after from, per channel, and
// reports whether bytes between from and the retained window were lost.
// Readers resume from a previous total, which is always a chunk boundary.
func (ring *ring) readFrom(from int64) (stdout, stderr string, lossy bool) {
	var streams [2]strings.Builder
	for _, current := range ring.chunks {
		if current.at >= from {
			_, _ = streams[current.channel].Write(current.data) // strings.Builder never fails
		}
	}
	return streams[Stdout].String(), streams[Stderr].String(), from < ring.earliest
}

// Output is a producer's writer face of one job's ring.
type Output struct {
	service *Service
	record  *record
	id      string
	// pending holds a trailing incomplete UTF-8 sequence per channel so a
	// character split across process writes is appended whole.
	pending [2][]byte
}

// ID returns the job ID issued at launch.
func (output *Output) ID() string { return output.id }

// Writer returns the io.Writer for one channel. Each channel's writer must
// be used from one goroutine at a time; the two may run concurrently.
func (output *Output) Writer(channel Channel) io.Writer {
	return channelWriter{output: output, channel: channel}
}

type channelWriter struct {
	output  *Output
	channel Channel
}

// Write appends the complete UTF-8 prefix of the pending bytes plus data
// and keeps an incomplete trailing sequence for the next write. It never
// fails; writes after settlement are dropped.
func (writer channelWriter) Write(data []byte) (int, error) {
	pending := slices.Concat(writer.output.pending[writer.channel], data)
	complete := len(pending) - incompleteSuffix(pending)
	writer.output.pending[writer.channel] = slices.Clone(pending[complete:])
	writer.output.service.write(writer.output.record, writer.channel, pending[:complete])
	return len(data), nil
}

// Advertise names the file that holds the complete stream of channel, or
// withdraws it with an empty locator when that file can no longer hold it.
// Reads list advertised files after a loss.
func (output *Output) Advertise(channel Channel, locator string) {
	output.service.advertise(output.record, channel, locator)
}

// flush appends bytes still held as incomplete sequences; rendering
// replaces them with the replacement character.
func (output *Output) flush() {
	for channel, pending := range output.pending {
		output.service.write(output.record, Channel(channel), pending)
		output.pending[channel] = nil
	}
}

// incompleteSuffix measures a trailing UTF-8 sequence that could still be
// completed by later bytes. Invalid bytes are never held back.
func incompleteSuffix(data []byte) int {
	for size := 1; size < utf8.UTFMax && size <= len(data); size++ {
		start := len(data) - size
		if utf8.RuneStart(data[start]) {
			if utf8.FullRune(data[start:]) {
				return 0
			}
			return size
		}
	}
	return 0
}

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// imageReserveBytes is the part of a session's remaining capacity that
	// images may never use. Text steps, compaction, and closing records keep
	// at least this much room after the last admitted image, so a full log
	// is reached through ordinary growth rather than a failed image append.
	imageReserveBytes = 8 << 20
	// recordFramingBytes bounds the sequence envelope and line ending a log
	// adds around one encoded record.
	recordFramingBytes = 64
)

// ErrImageCapacity reports images that the session log cannot hold. Nothing
// is committed for the refused images.
var ErrImageCapacity = errors.New("session cannot hold more images")

// imageRoom checks a record that carries images against the session's
// remaining capacity minus the image reserve. It returns the bytes the
// record needs and the bytes still available to images; records without
// images always fit.
func imageRoom(log *journal, record session.Record) (need, available int64, fits bool) {
	if !carriesImage(record) {
		return 0, 0, true
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return 0, 0, false
	}
	need = int64(len(encoded)) + recordFramingBytes
	available = max(0, log.Remaining()-imageReserveBytes)
	return need, available, need <= available
}

func carriesImage(record session.Record) bool {
	if record.Result != nil && record.Result.Image != nil {
		return true
	}
	return record.Message != nil && slices.ContainsFunc(record.Message.Content, func(block session.ContentBlock) bool {
		return block.Type == session.ContentImage
	})
}

// checkMessageImages refuses user input whose images the session cannot hold.
func checkMessageImages(log *journal, message session.Message) error {
	need, available, fits := imageRoom(log, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message})
	if fits {
		return nil
	}
	return fmt.Errorf("%w: the attached images need about %d bytes, but this session can hold only %d more bytes of images; start a new session to attach them", ErrImageCapacity, need, available)
}

// fitResultImage keeps a tool-result image only while the session can hold
// it. A refused image turns the result into an error the model and the user
// both see, instead of an append failure that would end the turn.
func fitResultImage(log *journal, record session.Record) session.Record {
	need, available, fits := imageRoom(log, record)
	if fits {
		return record
	}
	result := *record.Result
	result.Image, result.IsError = nil, true
	result.Output = fmt.Sprintf("Error: the image was not kept: it needs about %d bytes, but this session can hold only %d more bytes of images; start a new session to read more images", need, available)
	record.Result = &result
	return record
}

// fitMessageImages replaces the images of mid-turn input that the session
// can no longer hold with a notice, so the rest of the input still arrives.
func fitMessageImages(log *journal, record session.Record) session.Record {
	if _, _, fits := imageRoom(log, record); fits {
		return record
	}
	message := *record.Message
	message.Content = slices.DeleteFunc(slices.Clone(message.Content), func(block session.ContentBlock) bool {
		return block.Type == session.ContentImage
	})
	message.Content = append(message.Content, session.ContentBlock{Type: session.ContentText, Text: "[images omitted: this session cannot hold more images; start a new session to attach them]"})
	record.Message = &message
	return record
}

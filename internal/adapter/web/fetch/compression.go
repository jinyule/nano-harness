package fetch

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	maxContentEncodings = 5
	decodeChunkBytes    = 32 * 1024
)

var errDecompressionLimit = errors.New("decompression resource limit exceeded")

// decompress reverses the declared encoding order and bounds intermediate
// streams. The caller bounds the final output and closes all returned decoders,
// including when a later decoder cannot be constructed.
func decompress(ctx context.Context, source io.Reader, header string) (io.Reader, []io.Closer, error) {
	var decoders []io.Closer
	source = &contextReader{ctx: ctx, source: source}
	if header == "" {
		return source, decoders, nil
	}
	codings := strings.Split(header, ",")
	if len(codings) > maxContentEncodings {
		return nil, decoders, fmt.Errorf("response exceeds the maximum of %d content encodings: %w", maxContentEncodings, errDecompressionLimit)
	}
	// Reject the whole declaration before reading any potentially encoded body.
	for _, coding := range codings {
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "identity", "gzip", "x-gzip", "deflate":
		default:
			return nil, decoders, fmt.Errorf("unsupported content encoding %q", coding)
		}
	}
	for _, coding := range slices.Backward(codings) {
		if strings.EqualFold(strings.TrimSpace(coding), "identity") {
			continue
		}
		// A preceding decoder's output becomes this decoder's input. Bound
		// that intermediate stream, including framing consumed without output.
		if len(decoders) > 0 {
			source = &expansionReader{cappedReader{source: source, remaining: maxResponseBytes}}
		}
		var decoder io.ReadCloser
		var err error
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip", "x-gzip":
			decoder, err = gzip.NewReader(source)
		case "deflate":
			buffered := bufio.NewReader(source)
			first, peekErr := buffered.Peek(1)
			if peekErr != nil {
				return nil, decoders, fmt.Errorf("read deflate header: %w", peekErr)
			}
			// Match Undici's zlib/raw-deflate choice without retrying a body
			// whose decompressor may already have consumed network bytes.
			if first[0]&0x0f == 8 {
				decoder, err = zlib.NewReader(buffered)
			} else {
				decoder = flate.NewReader(buffered)
			}
		}
		if err != nil {
			return nil, decoders, fmt.Errorf("decode %s response: %w", coding, err)
		}
		decoders = append(decoders, decoder)
		source = &contextReader{ctx: ctx, source: decoder}
	}
	return source, decoders, nil
}

// contextReader belongs to one fetch operation. Checking both sides of each
// bounded read also stops buffered decoders after the network body has ended.
type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) > decodeChunkBytes {
		buffer = buffer[:decodeChunkBytes]
	}
	count, err := reader.source.Read(buffer)
	if ctxErr := reader.ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	return count, err
}

// expansionReader rejects oversized intermediate streams instead of turning
// their truncated compressed data into a misleading codec-integrity failure.
type expansionReader struct{ cappedReader }

func (reader *expansionReader) Read(buffer []byte) (int, error) {
	count, err := reader.cappedReader.Read(buffer)
	if reader.truncated {
		return 0, fmt.Errorf("decompressed intermediate response exceeds the maximum of %d bytes: %w", maxResponseBytes, errDecompressionLimit)
	}
	return count, err
}

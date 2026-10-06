package fetch

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"slices"
	"strings"
)

// decompress reverses the declared encoding order. The caller closes all
// returned decoders, including when a later decoder cannot be constructed.
func decompress(source io.Reader, header string) (io.Reader, []io.Closer, error) {
	var decoders []io.Closer
	if header == "" {
		return source, decoders, nil
	}
	codings := strings.Split(header, ",")
	// Reject the whole declaration before reading any potentially encoded body.
	for _, coding := range codings {
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "identity", "gzip", "x-gzip", "deflate":
		default:
			return nil, decoders, fmt.Errorf("unsupported content encoding %q", coding)
		}
	}
	for _, coding := range slices.Backward(codings) {
		var decoder io.ReadCloser
		var err error
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "identity":
			continue
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
		source = decoder
	}
	return source, decoders, nil
}

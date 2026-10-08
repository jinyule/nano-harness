package fetch

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/web"
)

func TestDecompress_CapsExpandedBytesAtEverySupportedEncoding(t *testing.T) {
	for _, coding := range []string{"gzip", "x-gzip", "deflate", "raw-deflate", "gzip, deflate"} {
		for _, size := range []int{maxResponseBytes, maxResponseBytes + 1} {
			t.Run(coding+"/"+map[bool]string{false: "exact", true: "extra"}[size > maxResponseBytes], func(t *testing.T) {
				data := bytes.Repeat([]byte{0}, size)
				header := coding
				switch coding {
				case "x-gzip":
					data = compressed(t, "gzip", data)
				case "raw-deflate":
					data = compressed(t, coding, data)
					header = "deflate"
				case "gzip, deflate":
					data = compressed(t, "deflate", compressed(t, "gzip", data))
				default:
					data = compressed(t, coding, data)
				}
				source, decoders, err := decompress(t.Context(), bytes.NewReader(data), header)
				t.Cleanup(func() {
					for _, decoder := range decoders {
						_ = decoder.Close()
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				bounded := &cappedReader{source: source, remaining: maxResponseBytes}
				body, err := io.ReadAll(bounded)
				if err != nil || len(body) != maxResponseBytes || bounded.truncated != (size > maxResponseBytes) || bytes.Count(body, []byte{0}) != maxResponseBytes {
					t.Fatalf("decoded bytes=%d truncated=%v err=%v", len(body), bounded.truncated, err)
				}
			})
		}
	}
}

func TestFetch_StopsDecompressionBeforeBombTrailer(t *testing.T) {
	for _, coding := range []string{"gzip", "deflate"} {
		t.Run(coding, func(t *testing.T) {
			data := compressed(t, coding, make([]byte, maxResponseBytes*2))
			// A corrupt trailer beyond the byte cap proves read does not fully
			// inflate the bomb before truncating its returned text.
			data[len(data)-1] ^= 0xff
			current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/plain")
				writer.Header().Set("Content-Encoding", coding)
				_, _ = writer.Write(data)
			})
			result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
			if err != nil || !result.Truncated || result.Content != strings.Repeat("\x00", maxBodyUnits) {
				t.Fatalf("bytes=%d truncated=%v err=%v", len(result.Content), result.Truncated, err)
			}
		})
	}
}

func TestDecompress_RejectsMalformedHeadersAndPreservesCauses(t *testing.T) {
	failure := errors.New("wire read failed")
	for _, test := range []struct {
		name   string
		coding string
		source io.Reader
		cause  error
	}{
		{"gzip", "gzip", strings.NewReader("invalid gzip"), gzip.ErrHeader},
		{"zlib", "deflate", bytes.NewReader([]byte{0x78, 0, 0}), zlib.ErrHeader},
		{"empty", "deflate", strings.NewReader(""), io.EOF},
		{"read-failure", "deflate", errorReader{failure}, failure},
		{"inner-header", "gzip, deflate", bytes.NewReader(compressed(t, "deflate", []byte("invalid gzip"))), gzip.ErrHeader},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, decoders, err := decompress(t.Context(), test.source, test.coding)
			for _, decoder := range decoders {
				_ = decoder.Close()
			}
			if !errors.Is(err, test.cause) {
				t.Fatalf("err=%v want cause=%v", err, test.cause)
			}
		})
	}
	if _, _, err := decompress(t.Context(), errorReader{failure}, "gzip, br"); err == nil || !strings.Contains(err.Error(), "unsupported content encoding") || errors.Is(err, failure) {
		t.Fatalf("unsupported declaration consumed body: %v", err)
	}
}

func TestFetch_RejectsCorruptCompressedBodies(t *testing.T) {
	for _, coding := range []string{"gzip", "deflate", "raw-deflate"} {
		t.Run(coding, func(t *testing.T) {
			data := compressed(t, coding, []byte("hello"))
			data = data[:len(data)-1]
			header := coding
			if coding == "raw-deflate" {
				header = "deflate"
			}
			current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/plain")
				writer.Header().Set("Content-Encoding", header)
				_, _ = writer.Write(data)
			})
			_, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
			expectCode(t, err, web.CodeProviderError)
		})
	}
}

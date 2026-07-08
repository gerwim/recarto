package syncserver

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLimitedReaderErrorsRatherThanSilentlyTruncating pins the bug the
// decompressed-body limit used to have: io.LimitReader returns a plain
// io.EOF once its budget is exhausted, indistinguishable from a legitimately
// short body — a bufio.Scanner (as internal/fullexport.Read uses to stream
// /v1/import) treats io.EOF as a clean end of input, so an oversized archive
// could be silently truncated and still report success. limitedReader must
// instead surface a distinct, non-EOF error once the body exceeds its limit.
func TestLimitedReaderErrorsRatherThanSilentlyTruncating(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)

	// Exactly at the limit: every byte must still be readable, ending in a
	// plain io.EOF (not the size error) — the boundary case must not
	// false-positive.
	lr := &limitedReader{r: bytes.NewReader(data), n: int64(len(data))}
	got, err := io.ReadAll(lr)
	if err != nil {
		t.Fatalf("exactly-at-limit read failed: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("exactly-at-limit read = %d bytes, want %d", len(got), len(data))
	}

	// One byte over the limit: must fail with errDecompressedTooLarge, not
	// silently succeed with a truncated 100-byte result.
	lr = &limitedReader{r: bytes.NewReader(data), n: int64(len(data) - 1)}
	_, err = io.ReadAll(lr)
	if !errors.Is(err, errDecompressedTooLarge) {
		t.Fatalf("over-limit read err = %v, want errDecompressedTooLarge", err)
	}
}

// TestGzipRequestBodyRejectsOversizedDecompressedBody exercises the full
// gzipRequestBody path (real gzip.Reader wrapped by limitedReader) end to
// end: a compressed body that expands past decompressedLimit must fail with
// errDecompressedTooLarge while being read, not silently hand back a
// truncated-but-otherwise-normal-looking stream.
func TestGzipRequestBodyRejectsOversizedDecompressedBody(t *testing.T) {
	plain := bytes.Repeat([]byte("y"), 1000)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/import", bytes.NewReader(gz.Bytes()))
	req.Header.Set("Content-Encoding", "gzip")
	rw := httptest.NewRecorder()

	body, err := gzipRequestBody(rw, req, int64(gz.Len())+1, 999) // decompressed cap one byte short of the real 1000
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()

	_, err = io.ReadAll(body)
	if !errors.Is(err, errDecompressedTooLarge) {
		t.Fatalf("reading an over-limit decompressed body: err = %v, want errDecompressedTooLarge", err)
	}
}

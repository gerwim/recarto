package syncserver

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxDecompressedBytes bounds a /changes push request body AFTER gzip
// decompression, independent of maxPushBytes (which bounds the compressed
// wire size). A compressed body can expand far beyond its wire size (a
// degenerate "bomb"), so both limits apply: the wire size is capped first via
// http.MaxBytesReader, then the decompressed stream is capped again. Sync
// payloads are always small, text-only memory rows (never the code graph),
// so 64MB decompressed is generous headroom for legitimate use. /v1/import
// bounds its (much larger, legitimately multi-project) body with its own,
// separate pair of limits — see maxImportWireBytes/maxImportBytes.
const maxDecompressedBytes = 64 << 20

// errDecompressedTooLarge is returned once a decompressed request body
// exceeds its configured limit. Unlike io.LimitReader — which silently
// returns io.EOF at the limit, indistinguishable from a legitimately short
// body — this makes an oversized body loudly fail: a bufio.Scanner (as
// internal/fullexport.Read uses) treats io.EOF as a clean end of input and
// would otherwise report success on a body that was actually truncated
// mid-stream.
var errDecompressedTooLarge = errors.New("decompressed request body exceeds limit")

// gzipRequestBody prepares r.Body for reading: the wire size is always capped
// at wireLimit, and when the client marked the body gzip-compressed
// (Content-Encoding: gzip — set unconditionally by internal/remotesync.Client
// for both pushes and imports), it is transparently decompressed with the
// expansion capped at decompressedLimit. Both limits are enforced
// independently (wire size via http.MaxBytesReader before any decompression
// happens at all, decompressed size via a further limitedReader on the gzip
// stream) so a small compressed body cannot "bomb" past decompressedLimit.
func gzipRequestBody(w http.ResponseWriter, r *http.Request, wireLimit, decompressedLimit int64) (io.ReadCloser, error) {
	body := http.MaxBytesReader(w, r.Body, wireLimit)
	if r.Header.Get("Content-Encoding") != "gzip" {
		return body, nil
	}
	gzr, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	return limitedGzipReader{r: &limitedReader{r: gzr, n: decompressedLimit}, gz: gzr}, nil
}

// limitedGzipReader closes the underlying gzip.Reader (which in turn closes
// nothing else — the wrapped http.MaxBytesReader closes with the request).
type limitedGzipReader struct {
	r  io.Reader
	gz *gzip.Reader
}

func (l limitedGzipReader) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l limitedGzipReader) Close() error               { return l.gz.Close() }

// limitedReader mirrors the read-one-byte-past-the-limit technique
// http.MaxBytesReader uses, so exceeding n is reported as errDecompressedTooLarge
// rather than silently surfacing as a plain io.EOF.
type limitedReader struct {
	r   io.Reader
	n   int64 // bytes remaining before the limit is hit
	err error // sticky error once set
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Read at most n+1 bytes: however many of them come back tells us
	// whether we're exactly at the limit (fine) or past it (error).
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	if int64(n) <= l.n {
		l.n -= int64(n)
		l.err = err
		return n, err
	}
	n = int(l.n)
	l.n = 0
	l.err = errDecompressedTooLarge
	return n, l.err
}

// gzipResponseWriter wraps an http.ResponseWriter so writes are transparently
// gzip-compressed.
type gzipResponseWriter struct {
	http.ResponseWriter
	gw *gzip.Writer
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) { return g.gw.Write(p) }

// withGzipResponse wraps next so its response body is gzip-compressed
// whenever the request's Accept-Encoding advertises gzip support (set
// unconditionally by internal/remotesync.Client) — the main win for a large
// bootstrap pull, whose payload is JSON memory records that compress well.
func withGzipResponse(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gw := gzip.NewWriter(w)
		defer gw.Close()
		next(&gzipResponseWriter{ResponseWriter: w, gw: gw}, r)
	}
}

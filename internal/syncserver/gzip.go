package syncserver

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// maxDecompressedBytes bounds a request body AFTER gzip decompression,
// independent of maxPushBytes (which bounds the compressed wire size). A
// compressed body can expand far beyond its wire size (a degenerate "bomb"),
// so both limits apply: the wire size is capped first via
// http.MaxBytesReader, then the decompressed stream is capped again. Sync
// payloads are always small, text-only memory rows (never the code graph),
// so 64MB decompressed is generous headroom for legitimate use.
const maxDecompressedBytes = 64 << 20

// gzipRequestBody prepares r.Body for reading: the wire size is always capped
// at maxPushBytes, and when the client marked the body gzip-compressed
// (Content-Encoding: gzip — set unconditionally by internal/remotesync.Client
// for pushes), it is transparently decompressed with the expansion capped at
// maxDecompressedBytes.
func gzipRequestBody(w http.ResponseWriter, r *http.Request) (io.ReadCloser, error) {
	body := http.MaxBytesReader(w, r.Body, maxPushBytes)
	if r.Header.Get("Content-Encoding") != "gzip" {
		return body, nil
	}
	gzr, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	return limitedGzipReader{r: io.LimitReader(gzr, maxDecompressedBytes), gz: gzr}, nil
}

// limitedGzipReader closes the underlying gzip.Reader (which in turn closes
// nothing else — the wrapped http.MaxBytesReader closes with the request).
type limitedGzipReader struct {
	r  io.Reader
	gz *gzip.Reader
}

func (l limitedGzipReader) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l limitedGzipReader) Close() error               { return l.gz.Close() }

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

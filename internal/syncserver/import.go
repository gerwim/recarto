package syncserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gerwim/recarto/internal/fullexport"
	"github.com/gerwim/recarto/internal/syncwire"
)

// maxImportWireBytes / maxImportBytes bound a POST /v1/import body,
// mirroring maxPushBytes/maxDecompressedBytes's compressed/decompressed pair
// for /changes but sized for a legitimately much larger, multi-project bulk
// migration archive (recarto export --push) rather than one repo's
// incremental push. Both are enforced independently by gzipRequestBody: the
// compressed wire size first (before any decompression happens at all), then
// the decompressed stream again — so a small compressed body cannot "bomb"
// past maxImportBytes regardless of how far it would otherwise expand.
const (
	maxImportWireBytes = 64 << 20  // compressed request body
	maxImportBytes     = 256 << 20 // decompressed archive content
)

// handleImport answers POST /v1/import: a bulk, multi-project archive (see
// internal/fullexport) is fully read into memory (bounded, and rejected
// atomically if it exceeds that bound — see the io.ReadAll comment below),
// then parsed line-by-line and each entry applied to its tagged repo key's
// store via the SAME applyRecord/SyncApplyTx merge core handlePostChanges
// uses — the server is, structurally, a headless recarto store per repo key,
// and a bulk import is just many repos' worth of ordinary pushes read from
// one request body instead of many. Idempotent exactly like a push:
// re-importing an unchanged archive accepts 0 for every entry.
//
// Unlike /v1/repos/{key}/changes, the repo key travels per-entry in the body
// rather than in the URL path, since one archive spans many repos; storeFor
// still validates every key against keyPattern before it is ever joined into
// a filesystem path.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	s.doImport(w, r, maxImportWireBytes, maxImportBytes)
}

// doImport is handleImport's body, parameterized on the wire/decompressed
// limits so tests can exercise the over-limit-rejects-atomically behavior
// against small archives instead of ones sized in the hundreds of megabytes.
func (s *Server) doImport(w http.ResponseWriter, r *http.Request, wireLimit, decompressedLimit int64) {
	body, err := gzipRequestBody(w, r, wireLimit, decompressedLimit)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			httpError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
			return
		}
		httpError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	defer body.Close()

	// Spool the whole bounded, decompressed archive into memory BEFORE
	// applying anything. fullexport.Read applies each entry to its store as
	// it parses the stream, so discovering an over-limit body only when the
	// reader trips (partway through the archive) would leave every entry
	// read before that point already committed. Reading the entire body up
	// front means a size violation — wire (compressed) or decompressed,
	// gzipRequestBody enforces both on this same reader — is caught and
	// rejected atomically, with no entries applied at all.
	data, err := io.ReadAll(body)
	if err != nil {
		status, msg := importErrorStatus(err)
		httpError(w, status, msg)
		return
	}

	ctx := r.Context()
	resp := syncwire.ImportResponse{Repos: map[string]syncwire.RepoImportStat{}}
	_, err = fullexport.Read(bytes.NewReader(data), func(e syncwire.ExportEntry) error {
		rs, serr := s.storeFor(e.RepoKey)
		if serr != nil {
			return serr
		}
		accepted, aerr := s.applyRecord(ctx, rs, e.Record)
		if aerr != nil {
			return aerr
		}
		stat := resp.Repos[e.RepoKey]
		stat.Entries++
		resp.TotalEntries++
		if accepted {
			stat.Accepted++
			resp.TotalAccepted++
		}
		resp.Repos[e.RepoKey] = stat
		return nil
	})
	if err != nil {
		status, msg := importErrorStatus(err)
		httpError(w, status, msg)
		return
	}
	writeJSON(w, resp)
}

// importErrorStatus maps an error surfaced while spooling or parsing a POST
// /v1/import body to the HTTP status and message handleImport should respond
// with. A compressed body that exceeds maxImportWireBytes can trip the
// http.MaxBytesReader wrapped around r.Body mid-read (not just up front in
// gzipRequestBody) and surfaces here as *http.MaxBytesError — that MUST map
// to 413 like the decompressed-size limit does, not fall through to the 500
// default.
func importErrorStatus(err error) (int, string) {
	if errors.Is(err, errBadRepoKey) {
		return http.StatusBadRequest, err.Error()
	}
	if errors.Is(err, errDecompressedTooLarge) {
		return http.StatusRequestEntityTooLarge, err.Error()
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge, "request body exceeds limit"
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return http.StatusBadRequest, "bad request body: " + err.Error()
	}
	return http.StatusInternalServerError, err.Error()
}

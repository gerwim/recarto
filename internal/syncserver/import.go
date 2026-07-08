package syncserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"git.gerwim.nl/gerwim/recarto/internal/fullexport"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
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
// internal/fullexport) is streamed line-by-line and each entry is applied to
// its tagged repo key's store via the SAME applyRecord/SyncApplyTx merge core
// handlePostChanges uses — the server is, structurally, a headless recarto
// store per repo key, and a bulk import is just many repos' worth of
// ordinary pushes read from one request body instead of many. Idempotent
// exactly like a push: re-importing an unchanged archive accepts 0 for every
// entry.
//
// Unlike /v1/repos/{key}/changes, the repo key travels per-entry in the body
// rather than in the URL path, since one archive spans many repos; storeFor
// still validates every key against keyPattern before it is ever joined into
// a filesystem path.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	body, err := gzipRequestBody(w, r, maxImportWireBytes, maxImportBytes)
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

	ctx := r.Context()
	resp := syncwire.ImportResponse{Repos: map[string]syncwire.RepoImportStat{}}
	_, err = fullexport.Read(body, func(e syncwire.ExportEntry) error {
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

// importErrorStatus maps an error surfaced while streaming
// fullexport.Read over a POST /v1/import body to the HTTP status and message
// handleImport should respond with. A compressed body that exceeds
// maxImportWireBytes can trip the http.MaxBytesReader wrapped around r.Body
// mid-stream (not just up front in gzipRequestBody) and surfaces here as
// *http.MaxBytesError — that MUST map to 413 like the decompressed-size
// limit does, not fall through to the 500 default.
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

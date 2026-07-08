package syncserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// importArchive builds a minimal recarto-export/v1 NDJSON archive (header +
// entries) and gzip-compresses it, mirroring what internal/fullexport writes.
func importArchive(t *testing.T, entries []syncwire.ExportEntry) []byte {
	t.Helper()
	var sb strings.Builder
	header, _ := json.Marshal(syncwire.ExportHeader{Format: syncwire.ExportFormat})
	sb.Write(header)
	sb.WriteByte('\n')
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}
	return gzipCompress(t, []byte(sb.String()))
}

func doImport(t *testing.T, client *http.Client, base, bearer string, entries []syncwire.ExportEntry) *http.Response {
	t.Helper()
	body := importArchive(t, entries)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/import", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestImportAppliesEntriesAcrossRepos(t *testing.T) {
	client, base := testServer(t, "s3cret")

	entries := []syncwire.ExportEntry{
		{RepoKey: "repoA", Record: syncwire.Record{ID: "a1", Kind: "learning", Title: "t", Body: "a full-history learning from repo A", CreatedAt: 100, UpdatedAt: 100}},
		{RepoKey: "repoB", Record: syncwire.Record{ID: "b1", Kind: "decision", Title: "t", Body: "a full-history decision from repo B", CreatedAt: 200, UpdatedAt: 200}},
	}
	resp := doImport(t, client, base, "s3cret", entries)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var ir syncwire.ImportResponse
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		t.Fatal(err)
	}
	if ir.TotalEntries != 2 || ir.TotalAccepted != 2 {
		t.Fatalf("import response = %+v, want 2/2", ir)
	}
	if ir.Repos["repoA"].Accepted != 1 || ir.Repos["repoB"].Accepted != 1 {
		t.Fatalf("per-repo stats = %+v, want 1 accepted each", ir.Repos)
	}

	for key, wantBody := range map[string]string{
		"repoA": "a full-history learning from repo A",
		"repoB": "a full-history decision from repo B",
	} {
		respC := doReq(t, client, http.MethodGet, base+"/v1/repos/"+key+"/changes?since=0", "s3cret", nil)
		var cr syncwire.ChangesResponse
		if err := json.NewDecoder(respC.Body).Decode(&cr); err != nil {
			t.Fatal(err)
		}
		respC.Body.Close()
		if len(cr.Records) != 1 || cr.Records[0].Body != wantBody {
			t.Fatalf("repo %s changes = %+v, want body %q", key, cr.Records, wantBody)
		}
	}
}

func TestImportIsIdempotent(t *testing.T) {
	client, base := testServer(t, "s3cret")
	entries := []syncwire.ExportEntry{
		{RepoKey: "repoA", Record: syncwire.Record{ID: "a1", Kind: "learning", Title: "t", Body: "idempotent import body", CreatedAt: 100, UpdatedAt: 100}},
	}

	resp := doImport(t, client, base, "s3cret", entries)
	var first syncwire.ImportResponse
	json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()
	if first.TotalAccepted != 1 {
		t.Fatalf("first import accepted = %d, want 1", first.TotalAccepted)
	}

	resp = doImport(t, client, base, "s3cret", entries)
	var second syncwire.ImportResponse
	json.NewDecoder(resp.Body).Decode(&second)
	resp.Body.Close()
	if second.TotalAccepted != 0 {
		t.Fatalf("re-importing an identical archive must accept 0, got %d", second.TotalAccepted)
	}

	respC := doReq(t, client, http.MethodGet, base+"/v1/repos/repoA/changes?since=0", "s3cret", nil)
	defer respC.Body.Close()
	var cr syncwire.ChangesResponse
	json.NewDecoder(respC.Body).Decode(&cr)
	if len(cr.Records) != 1 {
		t.Fatalf("changes after re-import = %+v, want exactly 1 record", cr.Records)
	}
}

func TestImportTombstoneDeletesAndDoesNotResurrect(t *testing.T) {
	client, base := testServer(t, "s3cret")

	insert := []syncwire.ExportEntry{
		{RepoKey: "repoA", Record: syncwire.Record{ID: "a1", Kind: "learning", Title: "t", Body: "will be deleted via import", CreatedAt: 100, UpdatedAt: 100}},
	}
	resp := doImport(t, client, base, "s3cret", insert)
	resp.Body.Close()

	tomb := []syncwire.ExportEntry{
		{RepoKey: "repoA", Record: syncwire.Record{ID: "a1", Deleted: true, UpdatedAt: 200}},
	}
	resp = doImport(t, client, base, "s3cret", tomb)
	var ir syncwire.ImportResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	resp.Body.Close()
	if ir.TotalAccepted != 1 {
		t.Fatalf("tombstone import accepted = %d, want 1", ir.TotalAccepted)
	}

	respC := doReq(t, client, http.MethodGet, base+"/v1/repos/repoA/changes?since=0", "s3cret", nil)
	var cr syncwire.ChangesResponse
	json.NewDecoder(respC.Body).Decode(&cr)
	respC.Body.Close()
	var found bool
	for _, r := range cr.Records {
		if r.ID == "a1" {
			found = true
			if !r.Deleted {
				t.Fatalf("expected a1 to be a tombstone: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("tombstone must appear in changes: %+v", cr.Records)
	}

	// An older, stale non-deleted re-import (e.g. from a second, out-of-date
	// archive) must not resurrect the memory.
	stale := []syncwire.ExportEntry{
		{RepoKey: "repoA", Record: syncwire.Record{ID: "a1", Kind: "learning", Title: "t", Body: "stale resurrection attempt", CreatedAt: 100, UpdatedAt: 150}},
	}
	resp = doImport(t, client, base, "s3cret", stale)
	var ir2 syncwire.ImportResponse
	json.NewDecoder(resp.Body).Decode(&ir2)
	resp.Body.Close()
	if ir2.TotalAccepted != 0 {
		t.Fatalf("stale non-deleted re-import must not resurrect (accepted should be 0), got %d", ir2.TotalAccepted)
	}
}

func TestImportRequiresAuth(t *testing.T) {
	client, base := testServer(t, "s3cret")
	for _, bearer := range []string{"", "wrong"} {
		resp := doImport(t, client, base, bearer, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bearer=%q: status = %d, want 401", bearer, resp.StatusCode)
		}
	}
}

func TestImportRejectsInvalidRepoKey(t *testing.T) {
	client, base := testServer(t, "s3cret")
	entries := []syncwire.ExportEntry{
		{RepoKey: "bad!key", Record: syncwire.Record{ID: "a1", Kind: "learning", Title: "t", Body: "should never apply", CreatedAt: 100, UpdatedAt: 100}},
	}
	resp := doImport(t, client, base, "s3cret", entries)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid repo_key: status = %d, want 400", resp.StatusCode)
	}
}

func TestImportSkipsFeedbackReservedKeyLikeAnyOtherRepo(t *testing.T) {
	client, base := testServer(t, "s3cret")
	entries := []syncwire.ExportEntry{
		{RepoKey: "_recarto_feedback", Record: syncwire.Record{ID: "fb1", Kind: "feedback", Title: "t", Body: "a recarto issue report", CreatedAt: 100, UpdatedAt: 100}},
	}
	resp := doImport(t, client, base, "s3cret", entries)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var ir syncwire.ImportResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	if ir.TotalAccepted != 1 {
		t.Fatalf("import response = %+v, want 1 accepted", ir)
	}
}

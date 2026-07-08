package syncserver

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

func testServer(t *testing.T, key string) (*http.Client, string) {
	t.Helper()
	srv := New(key, filepath.Join(t.TempDir(), "data"))
	t.Cleanup(func() { _ = srv.Close() })
	return unixHTTPClient(t, srv.Handler())
}

func doReq(t *testing.T, client *http.Client, method, url, bearer string, body []byte) *http.Response {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	client, base := testServer(t, "s3cret")
	resp := doReq(t, client, http.MethodGet, base+"/healthz", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestChangesRequiresAuth(t *testing.T) {
	client, base := testServer(t, "s3cret")
	for _, tc := range []struct {
		name   string
		bearer string
	}{
		{"missing", ""},
		{"wrong", "nope"},
	} {
		resp := doReq(t, client, http.MethodGet, base+"/v1/repos/repo1/changes?since=0", tc.bearer, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, resp.StatusCode)
		}
	}
}

// TestPushRejectsInvalidRepoKey exercises keyPattern's own validation. A
// slash- or dot-segment traversal attempt (e.g. "../../etc") never reaches
// storeFor at all — net/http's ServeMux cleans "." / ".." path segments (and
// the {key} wildcard matches only a single "/"-free segment) before routing —
// so this instead uses a single-segment key containing a character outside
// keyPattern's safe charset, which IS routed to the handler and must be
// rejected there.
func TestPushRejectsInvalidRepoKey(t *testing.T) {
	client, base := testServer(t, "s3cret")
	body, _ := json.Marshal(syncwire.PushRequest{})
	resp := doReq(t, client, http.MethodPost, base+"/v1/repos/bad!key/changes", "s3cret", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid key: status = %d, want 400", resp.StatusCode)
	}
}

func TestPushThenPullRoundTrip(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes"

	push := syncwire.PushRequest{Records: []syncwire.Record{
		{ID: "m1", Kind: "learning", Title: "t", Body: "a durable learning about retries",
			CreatedAt: 100, UpdatedAt: 100},
	}}
	body, _ := json.Marshal(push)
	resp := doReq(t, client, http.MethodPost, url, "s3cret", body)
	var pr syncwire.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if pr.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", pr.Accepted)
	}
	if pr.Cursor <= 0 {
		t.Fatalf("cursor = %d, want > 0", pr.Cursor)
	}

	resp = doReq(t, client, http.MethodGet, url+"?since=0", "s3cret", nil)
	var cr syncwire.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(cr.Records) != 1 || cr.Records[0].ID != "m1" {
		t.Fatalf("changes = %+v, want one record m1", cr)
	}
	if cr.Records[0].Body != "a durable learning about retries" {
		t.Fatalf("body mismatch: %+v", cr.Records[0])
	}
	if cr.More {
		t.Fatalf("More = true, want false (only one record)")
	}
	if cr.Cursor != pr.Cursor {
		t.Fatalf("changes cursor %d != push cursor %d", cr.Cursor, pr.Cursor)
	}

	// A second /changes?since=<cursor> call must return nothing new.
	resp = doReq(t, client, http.MethodGet, url+"?since="+strconv.FormatInt(cr.Cursor, 10), "s3cret", nil)
	var cr2 syncwire.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr2); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(cr2.Records) != 0 {
		t.Fatalf("expected no new records past the cursor, got %+v", cr2.Records)
	}
}

func TestPushIdempotentDoesNotBumpSeqTwice(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes"

	rec := syncwire.Record{ID: "m1", Kind: "learning", Title: "t", Body: "idempotent push body", CreatedAt: 100, UpdatedAt: 100}
	body, _ := json.Marshal(syncwire.PushRequest{Records: []syncwire.Record{rec}})

	resp := doReq(t, client, http.MethodPost, url, "s3cret", body)
	var first syncwire.PushResponse
	json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()

	resp = doReq(t, client, http.MethodPost, url, "s3cret", body)
	var second syncwire.PushResponse
	json.NewDecoder(resp.Body).Decode(&second)
	resp.Body.Close()

	if second.Accepted != 0 {
		t.Fatalf("re-pushing the identical record must accept 0 (Noop), got %d", second.Accepted)
	}
	if second.Cursor != first.Cursor {
		t.Fatalf("an idempotent re-push must not advance the cursor: first=%d second=%d", first.Cursor, second.Cursor)
	}
}

func TestPushTombstonePropagatesAsDeletedChange(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes"

	insert := syncwire.PushRequest{Records: []syncwire.Record{
		{ID: "m1", Kind: "learning", Title: "t", Body: "will be deleted", CreatedAt: 100, UpdatedAt: 100},
	}}
	body, _ := json.Marshal(insert)
	resp := doReq(t, client, http.MethodPost, url, "s3cret", body)
	resp.Body.Close()

	tomb := syncwire.PushRequest{Records: []syncwire.Record{
		{ID: "m1", Deleted: true, UpdatedAt: 200},
	}}
	body, _ = json.Marshal(tomb)
	resp = doReq(t, client, http.MethodPost, url, "s3cret", body)
	var pr syncwire.PushResponse
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.Accepted != 1 {
		t.Fatalf("tombstone push accepted = %d, want 1", pr.Accepted)
	}

	resp = doReq(t, client, http.MethodGet, url+"?since=0", "s3cret", nil)
	var cr syncwire.ChangesResponse
	json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	var found bool
	for _, r := range cr.Records {
		if r.ID == "m1" {
			found = true
			if !r.Deleted {
				t.Fatalf("expected m1 to be a tombstone record: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("tombstone must appear in changes: %+v", cr.Records)
	}
}

func TestChangesPaging(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes"

	var recs []syncwire.Record
	for i := 0; i < 5; i++ {
		recs = append(recs, syncwire.Record{
			ID: "m" + strconv.FormatInt(int64(i), 10), Kind: "learning", Title: "t",
			Body:      "distinct body number " + strconv.FormatInt(int64(i), 10) + " about topic " + strconv.FormatInt(int64(i*7), 10),
			CreatedAt: int64(100 + i), UpdatedAt: int64(100 + i),
		})
	}
	body, _ := json.Marshal(syncwire.PushRequest{Records: recs})
	resp := doReq(t, client, http.MethodPost, url, "s3cret", body)
	var pr syncwire.PushResponse
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.Accepted != 5 {
		t.Fatalf("accepted = %d, want 5", pr.Accepted)
	}

	resp = doReq(t, client, http.MethodGet, url+"?since=0&limit=2", "s3cret", nil)
	var page1 syncwire.ChangesResponse
	json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()
	if len(page1.Records) != 2 || !page1.More {
		t.Fatalf("page1 = %+v, want 2 records and More=true", page1)
	}

	resp = doReq(t, client, http.MethodGet, url+"?since="+strconv.FormatInt(page1.Cursor, 10)+"&limit=2", "s3cret", nil)
	var page2 syncwire.ChangesResponse
	json.NewDecoder(resp.Body).Decode(&page2)
	resp.Body.Close()
	if len(page2.Records) != 2 || !page2.More {
		t.Fatalf("page2 = %+v, want 2 records and More=true", page2)
	}

	resp = doReq(t, client, http.MethodGet, url+"?since="+strconv.FormatInt(page2.Cursor, 10)+"&limit=2", "s3cret", nil)
	var page3 syncwire.ChangesResponse
	json.NewDecoder(resp.Body).Decode(&page3)
	resp.Body.Close()
	if len(page3.Records) != 1 || page3.More {
		t.Fatalf("page3 = %+v, want 1 record and More=false", page3)
	}
}

func gzipCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestGzipCompressedPushBodyIsDecompressed exercises the server side of what
// internal/remotesync.Client.Push does: a gzip-compressed push body with
// Content-Encoding: gzip must be transparently decompressed and applied
// exactly like an uncompressed one.
func TestGzipCompressedPushBodyIsDecompressed(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes"

	body, _ := json.Marshal(syncwire.PushRequest{Records: []syncwire.Record{
		{ID: "m1", Kind: "learning", Title: "t", Body: "gzip-compressed push body", CreatedAt: 100, UpdatedAt: 100},
	}})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(gzipCompress(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var pr syncwire.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	if pr.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1 (gzip body must be decompressed and applied)", pr.Accepted)
	}

	// Confirm it actually landed, via an ordinary (non-gzip) pull.
	resp2 := doReq(t, client, http.MethodGet, url+"?since=0", "s3cret", nil)
	defer resp2.Body.Close()
	var cr syncwire.ChangesResponse
	if err := json.NewDecoder(resp2.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if len(cr.Records) != 1 || cr.Records[0].Body != "gzip-compressed push body" {
		t.Fatalf("changes after gzip push = %+v", cr.Records)
	}
}

// TestGzipResponseWhenAcceptEncodingRequested exercises the server side of
// what internal/remotesync.Client.do expects on the read path: a request
// that advertises Accept-Encoding: gzip gets a gzip-compressed response body
// with Content-Encoding: gzip set (mirroring the pull/bootstrap path, which
// is where compression matters most).
func TestGzipResponseWhenAcceptEncodingRequested(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes"

	body, _ := json.Marshal(syncwire.PushRequest{Records: []syncwire.Record{
		{ID: "m1", Kind: "learning", Title: "t", Body: "response gzip body", CreatedAt: 100, UpdatedAt: 100},
	}})
	resp := doReq(t, client, http.MethodPost, url, "s3cret", body)
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, url+"?since=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Accept-Encoding", "gzip")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp2.Header.Get("Content-Encoding"))
	}
	gzr, err := gzip.NewReader(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer gzr.Close()
	raw, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatal(err)
	}
	var cr syncwire.ChangesResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		t.Fatal(err)
	}
	if len(cr.Records) != 1 || cr.Records[0].Body != "response gzip body" {
		t.Fatalf("decompressed changes = %+v", cr.Records)
	}
}

// TestRateLimitReturns429AfterBurstExceeded proves the per-IP token bucket
// throttles requests BEFORE auth runs — the first two requests (using a
// wrong key) are still processed far enough to return 401, but the third is
// rejected outright with 429, never reaching the auth check.
func TestRateLimitReturns429AfterBurstExceeded(t *testing.T) {
	srv := New("s3cret", filepath.Join(t.TempDir(), "data"))
	srv.limiter = newRateLimiter(0, 2) // no refill; exactly 2 requests allowed
	t.Cleanup(func() { _ = srv.Close() })
	client, base := unixHTTPClient(t, srv.Handler())

	url := base + "/v1/repos/repo1/changes?since=0"
	for i := range 2 {
		resp := doReq(t, client, http.MethodGet, url, "wrongkey", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 (not yet rate limited)", i, resp.StatusCode)
		}
	}
	resp := doReq(t, client, http.MethodGet, url, "wrongkey", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request: status = %d, want 429", resp.StatusCode)
	}
}

// TestRateLimitAppliesPerIndependentServer confirms a fresh Server's default
// limiter has enough burst for ordinary test/CLI traffic (a handful of
// requests), guarding against an overly aggressive default that would make
// legitimate low-traffic sync flaky.
func TestRateLimitDefaultBurstAllowsNormalTraffic(t *testing.T) {
	client, base := testServer(t, "s3cret")
	url := base + "/v1/repos/repo1/changes?since=0"
	for i := range 10 {
		resp := doReq(t, client, http.MethodGet, url, "s3cret", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (default burst should not throttle normal traffic)", i, resp.StatusCode)
		}
	}
}

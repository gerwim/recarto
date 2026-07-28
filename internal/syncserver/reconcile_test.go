package syncserver

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// TestSyncIndexIsRebuiltAfterAStoreRepair is the recovery contract for the one
// piece of server state store.Repair deliberately does not salvage.
//
// sync_index is server-only, so a rebuilt store comes back with every memory but
// an empty index — and a pull is driven entirely by that index, so without a
// rebuild the server would answer every client, forever, with "nothing here".
// Reopening must therefore reindex, and because the seq counter itself survives
// the repair, the fresh seqs land above any client's stored cursor: an existing
// client re-pulls once, and a new one (since=0) sees everything too.
func TestSyncIndexIsRebuiltAfterAStoreRepair(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")

	srv := New("s3cret", dataDir)
	client, base := unixHTTPClient(t, srv.Handler())
	url := base + "/v1/repos/repo1/changes"

	push := syncwire.PushRequest{Records: []syncwire.Record{
		{ID: "m1", Kind: "learning", Title: "t1", Body: "a durable learning about retry backoff",
			CreatedAt: 100, UpdatedAt: 100},
		{ID: "m2", Kind: "decision", Title: "t2", Body: "we keep the cache outside the worktree",
			CreatedAt: 101, UpdatedAt: 101},
		{ID: "m3", Kind: "learning", Title: "t3", Body: "wal mode lets many readers coexist with one writer",
			CreatedAt: 102, UpdatedAt: 102},
	}}
	body, _ := json.Marshal(push)
	resp := doReq(t, client, http.MethodPost, url, "s3cret", body)
	var pr syncwire.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if pr.Accepted != 3 {
		t.Fatalf("accepted = %d, want 3", pr.Accepted)
	}

	tomb := syncwire.PushRequest{Records: []syncwire.Record{{ID: "m3", Deleted: true, UpdatedAt: 200}}}
	body, _ = json.Marshal(tomb)
	resp = doReq(t, client, http.MethodPost, url, "s3cret", body)
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A client that has already caught up to here.
	clientCursor := pr.Cursor
	if clientCursor <= 0 {
		t.Fatalf("cursor = %d, want > 0", clientCursor)
	}
	resp = doReq(t, client, http.MethodGet,
		url+"?since="+strconv.FormatInt(clientCursor, 10), "s3cret", nil)
	var caughtUp syncwire.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&caughtUp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(caughtUp.Records) != 0 {
		t.Fatalf("client is not actually caught up: %+v", caughtUp.Records)
	}

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// Corruption strikes and the store is rebuilt: memories and tombstones are
	// salvaged, sync_index is not.
	dbPath := filepath.Join(dataDir, "repo1", "store.db")
	stats, err := store.Repair(dbPath)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if stats.Tables["memories"].Salvaged != 2 {
		t.Fatalf("salvaged %d memories, want 2", stats.Tables["memories"].Salvaged)
	}
	if stats.Tables["memory_tombstones"].Salvaged != 1 {
		t.Fatalf("salvaged %d tombstones, want 1", stats.Tables["memory_tombstones"].Salvaged)
	}

	srv2 := New("s3cret", dataDir)
	t.Cleanup(func() { _ = srv2.Close() })
	client2, base2 := unixHTTPClient(t, srv2.Handler())
	url2 := base2 + "/v1/repos/repo1/changes"

	assertRecovered := func(name, since string) {
		t.Helper()
		resp := doReq(t, client2, http.MethodGet, url2+"?since="+since, "s3cret", nil)
		var cr syncwire.ChangesResponse
		if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		got := map[string]syncwire.Record{}
		for _, r := range cr.Records {
			got[r.ID] = r
		}
		for _, id := range []string{"m1", "m2", "m3"} {
			if _, ok := got[id]; !ok {
				t.Fatalf("%s: %s missing from the rebuilt index: %+v", name, id, cr.Records)
			}
		}
		if got["m1"].Body != "a durable learning about retry backoff" {
			t.Errorf("%s: m1 body mismatch: %+v", name, got["m1"])
		}
		if got["m1"].Deleted || got["m2"].Deleted {
			t.Errorf("%s: live records marked deleted: %+v", name, cr.Records)
		}
		if !got["m3"].Deleted {
			t.Errorf("%s: the tombstoned record must come back as a delete: %+v", name, got["m3"])
		}
	}

	// The pre-existing client: its cursor still points into the old seq space, and
	// the rebuilt entries must sit above it.
	assertRecovered("existing client", strconv.FormatInt(clientCursor, 10))
	// A fresh client starting from scratch.
	assertRecovered("fresh client", "0")
}

// TestReconcileSyncIndexIsIdempotent: reopening a healthy store must not
// reindex anything, or every restart would re-push the whole repo to every
// client.
func TestReconcileSyncIndexIsIdempotent(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")

	srv := New("s3cret", dataDir)
	client, base := unixHTTPClient(t, srv.Handler())
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
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	srv2 := New("s3cret", dataDir)
	t.Cleanup(func() { _ = srv2.Close() })
	client2, base2 := unixHTTPClient(t, srv2.Handler())

	resp = doReq(t, client2, http.MethodGet,
		base2+"/v1/repos/repo1/changes?since="+strconv.FormatInt(pr.Cursor, 10), "s3cret", nil)
	var cr syncwire.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(cr.Records) != 0 {
		t.Fatalf("a healthy store was reindexed on reopen: %+v", cr.Records)
	}
}

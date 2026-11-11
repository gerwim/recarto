package syncserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/store"
	"github.com/gerwim/recarto/internal/syncwire"
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

// TestReconcileSyncIndexTombstoneWinsOverASameIdMemory pins the ordering that
// makes deletes stick.
//
// The abnormal-but-possible state is an id present in BOTH memory_tombstones and
// memories with no sync_index entry (e.g. a salvage that recovered a memory row
// whose delete had not yet been checkpointed). Because bumpSyncIndexTx is an
// upsert, indexing the memory after the tombstone would overwrite deleted=1 with
// deleted=0 and a higher seq — silently resurrecting deleted data for every
// client. The tombstone pass must therefore be WRITTEN before the memories set
// is even queried, so the memories query's NOT IN excludes it.
func TestReconcileSyncIndexTombstoneWinsOverASameIdMemory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	srv := New("s3cret", dataDir)
	t.Cleanup(func() { _ = srv.Close() })
	client, base := unixHTTPClient(t, srv.Handler())

	rs, err := srv.storeFor("repo1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := rs.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,status)
			 VALUES ('ghost','learning','deleted but still present','body','deleted body',100,100,'active')`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memory_tombstones(id,deleted_at,pushed) VALUES ('ghost',200,1)`); err != nil {
			return err
		}
		// A second, ordinary memory, so the memories pass is not empty and really
		// does run after the tombstone write.
		_, err := c.ExecContext(ctx,
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,status)
			 VALUES ('live','learning','a live one','body','live body',101,101,'active')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	n, err := reconcileSyncIndex(ctx, rs.st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reindexed %d, want 2 (the tombstone and the live memory, the ghost counted once)", n)
	}

	var entries, deleted int
	if err := rs.st.DB.QueryRow(
		`SELECT count(*), COALESCE(sum(deleted),0) FROM sync_index WHERE memory_id='ghost'`).
		Scan(&entries, &deleted); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("sync_index has %d entries for 'ghost', want exactly 1", entries)
	}
	if deleted != 1 {
		t.Fatal("the memories pass overwrote the tombstone: 'ghost' is indexed as live, resurrecting a delete")
	}

	// And over the wire, which is what a client actually sees.
	resp := doReq(t, client, http.MethodGet, base+"/v1/repos/repo1/changes?since=0", "s3cret", nil)
	var cr syncwire.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := map[string]syncwire.Record{}
	for _, r := range cr.Records {
		got[r.ID] = r
	}
	if rec, ok := got["ghost"]; !ok || !rec.Deleted {
		t.Fatalf("'ghost' must reach clients as a delete, got %+v (all: %+v)", rec, cr.Records)
	}
	if rec, ok := got["live"]; !ok || rec.Deleted {
		t.Fatalf("'live' must reach clients as a live record, got %+v", rec)
	}

	// Reconciling again must be a no-op: already-indexed entries keep their seqs,
	// or every restart would re-push the whole repo.
	before := indexSnapshot(t, rs)
	n2, err := reconcileSyncIndex(ctx, rs.st)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second reconcile reindexed %d, want 0", n2)
	}
	if after := indexSnapshot(t, rs); after != before {
		t.Fatalf("second reconcile changed the index:\n before %v\n after  %v", before, after)
	}
}

// indexSnapshot renders sync_index as a comparable string.
func indexSnapshot(t *testing.T, rs *repoStore) string {
	t.Helper()
	rows, err := rs.st.DB.Query(`SELECT memory_id, seq, deleted FROM sync_index ORDER BY memory_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id string
		var seq int64
		var del int
		if err := rows.Scan(&id, &seq, &del); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s:%d:%d ", id, seq, del)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

package remotesync

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/fullexport"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/repokey"
	"github.com/gerwim/recarto/internal/store"
)

// TestClientImportRoundTrip exercises Client.Import directly against a real
// handler (via the unix-socket harness): a hand-built archive posts and the
// response's per-repo counts must match.
func TestClientImportRoundTrip(t *testing.T) {
	httpClient := newSharedServer(t)
	client := &Client{BaseURL: "http://unix", Key: "s3cret", HTTP: httpClient}
	ctx := context.Background()

	cacheDir := t.TempDir()
	stA, err := store.Open(filepath.Join(cacheDir, "dirA", "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stA.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('root_commit', 'commit-import-1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	engA := memory.New(stA, ledger.New(stA), clock.Real{})
	if _, _, _, err := engA.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "t", Body: "a directly-imported learning"}); err != nil {
		t.Fatal(err)
	}
	stA.Close()

	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	stats, err := fullexport.ExportAll(ctx, cacheDir, clock.Real{}, gw, fullexport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 {
		t.Fatalf("export stats = %+v, want 1 record", stats)
	}

	resp, err := client.Import(ctx, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalEntries != 1 || resp.TotalAccepted != 1 {
		t.Fatalf("import response = %+v, want 1/1", resp)
	}
	key := repokey.Hash("commit-import-1")
	if resp.Repos[key].Accepted != 1 {
		t.Fatalf("import response repos = %+v, want 1 accepted under %s", resp.Repos, key)
	}
}

// TestFullHistoryExportImportConverges is the end-to-end proof behind the
// owner's ID/key question: a machine's PRE-EXISTING history (never pushed via
// ordinary incremental sync, since that only ever ships the local outbox) is
// exported, bulk-imported to the server, and a second, fresh machine for the
// SAME repo (same root commit, a totally different local gitctx.RepoID and
// cache dir) converges to it on an ORDINARY pull — proving the root-commit
// keying, UUID row ids, and content-merge core all round-trip a full history
// exactly like they round-trip incremental changes.
func TestFullHistoryExportImportConverges(t *testing.T) {
	httpClient := newSharedServer(t)
	ctx := context.Background()
	const rootCommit = "deadbeefcafef00d-fullhistory"

	// Machine A: a per-repo cache-dir store seeded directly (bypassing Record's
	// normal outbox bookkeeping is irrelevant here — fullexport reads every row
	// regardless of outbox state) with memories that predate any sync
	// configuration.
	cacheDir := t.TempDir()
	stA, err := store.Open(filepath.Join(cacheDir, "dirA", "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stA.WriteTx(ctx, func(c store.Conn) error {
		for _, kv := range [][2]string{{"repo_name", "myrepo"}, {"root_commit", rootCommit}} {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	engA := memory.New(stA, ledger.New(stA), clock.Real{})
	const n = 5
	for i := 0; i < n; i++ {
		if _, _, _, err := engA.Record(ctx, memory.NewMemory{
			Kind: memory.KindLearning, Title: "t",
			Body: fmt.Sprintf("pre-existing learning number %d about topic %d", i, i*7),
		}); err != nil {
			t.Fatal(err)
		}
	}
	stA.Close()

	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	if _, err := fullexport.ExportAll(ctx, cacheDir, clock.Real{}, gw, fullexport.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	client := &Client{BaseURL: "http://unix", Key: "s3cret", HTTP: httpClient}
	resp, err := client.Import(ctx, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalAccepted != n {
		t.Fatalf("import accepted = %d, want %d", resp.TotalAccepted, n)
	}

	// Machine B: a fresh store for a DIFFERENT local RepoID (different machine,
	// or a fresh ForgeBoard microVM clone) but the SAME root commit — an
	// ordinary Pull must converge to A's full pre-existing history.
	repoKey := repokey.Hash(rootCommit)
	b := newMachine(t)
	syncB := b.syncer(t, httpClient, repoKey)
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}

	recs, err := b.eng.List(ctx, memory.ListFilter{Kind: memory.KindLearning})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("machine B converged to %d records, want %d (A's full pre-existing history)", len(recs), n)
	}
}

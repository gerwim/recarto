// Package fullexport implements the one-shot bulk migration path that moves a
// machine's entire pre-existing memory history — every per-repo store under
// the cache dir, plus the global feedback store — to the HTTP sync server.
// It is the engine behind `recarto export` / `recarto import`.
//
// This is deliberately separate from internal/snapshot (the committed
// .recarto/memory.jsonl team file, active-only, merged via ImportMemory) and
// internal/export (markdown rendering): a full export ships EVERY status
// (including archived/done/superseded) and every delete tombstone, using
// syncwire.Record — the same shape the sync server's merge core
// (memory.SyncApplyTx) already consumes — because the ordinary incremental
// HTTP sync only ever ships a store's outbox (writes made after sync was
// configured), and can never move memories recorded before that.
package fullexport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/repokey"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// Options configures ExportAll.
type Options struct {
	// Only, when non-empty, restricts the export to the single per-repo cache
	// dir with this name (a gitctx.RepoID) — recarto export --repo.
	Only string
	// OnlyKey, when set alongside Only, overrides server-key resolution for
	// that single project instead of deriving it from the store's cached
	// root_commit meta. recarto export --repo resolves this live (via
	// gitctx + remotesync.RepoKey, which honors RECARTO_SYNC_REPO) before
	// calling ExportAll, so a shallow clone — one with no reachable root
	// commit, and thus no resolvable key from cache-dir metadata alone — can
	// still be exported by scoping to it directly and setting the env
	// override, exactly like an ordinary `recarto sync` from that checkout.
	// It is meaningless (and ignored) for a whole-cache-dir sweep: applying
	// one override to every unrelated project it enumerates would collide
	// their buckets, so an unscoped export always derives each project's key
	// from its own stored root_commit and skips what it can't resolve.
	OnlyKey string
}

// Stats tallies one ExportAll run.
type Stats struct {
	Repos        int
	Records      int
	Tombstones   int
	Skipped      int
	SkippedRepos []string // names of skipped repos, for a human-readable summary
}

// project is one cache-dir store ExportAll has resolved a server key for.
type project struct {
	dirName    string
	repoName   string
	repoKey    string
	rootCommit string
}

// ExportAll walks every per-repo store under cacheDir and writes a
// recarto-export/v1 archive to w: a header line (every project's key/name),
// then one syncwire.ExportEntry line per memory row and tombstone, grouped
// store-by-store. Each store is opened only long enough to read it, and only
// one is ever open at a time.
//
// Header generation is a separate, lightweight first pass (open each store
// just far enough to read its meta row and close it again) so the header can
// list every project up front while the actual record streaming that follows
// still touches one store at a time, bounding memory to one store's content.
func ExportAll(ctx context.Context, cacheDir string, clk clock.Clock, w io.Writer, opts Options) (Stats, error) {
	var stats Stats
	projects, err := resolveProjects(ctx, cacheDir, opts, &stats)
	if err != nil {
		return stats, err
	}

	header := syncwire.ExportHeader{
		Format: syncwire.ExportFormat, CreatedAt: clk.Now().UnixMilli(), RecartoVersion: version.Version,
	}
	for _, p := range projects {
		header.Repos = append(header.Repos, syncwire.ExportRepoMeta{Key: p.repoKey, Name: p.repoName, RootCommit: p.rootCommit})
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(&header); err != nil {
		return stats, err
	}

	for _, p := range projects {
		st, err := store.Open(filepath.Join(cacheDir, p.dirName, "store.db"))
		if err != nil {
			return stats, fmt.Errorf("reopen %s: %w", p.dirName, err)
		}
		eng := memory.New(st, ledger.New(st), clk)
		records, tombstones, err := exportStore(ctx, eng, p.repoKey, enc)
		st.Close()
		if err != nil {
			return stats, fmt.Errorf("export %s: %w", p.dirName, err)
		}
		stats.Repos++
		stats.Records += records
		stats.Tombstones += tombstones
	}
	return stats, nil
}

// resolveProjects enumerates cacheDir's per-repo store directories, resolving
// each one's server key. A store with no resolvable key (a shallow clone with
// no root commit, or a non-git working directory — see serverKeyFor) is
// counted and named in stats.SkippedRepos rather than failing the whole
// export: the rest of the machine's history should still travel.
func resolveProjects(ctx context.Context, cacheDir string, opts Options, stats *Stats) ([]project, error) {
	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // nothing has ever been recorded on this machine: an empty export, not an error
	}
	if err != nil {
		return nil, err
	}
	var projects []project
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		if opts.Only != "" && de.Name() != opts.Only {
			continue
		}
		dbPath := filepath.Join(cacheDir, de.Name(), "store.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", dbPath, err)
		}
		repoName, rootCommit, err := readMeta(ctx, st)
		st.Close()
		if err != nil {
			return nil, fmt.Errorf("read meta %s: %w", dbPath, err)
		}

		var key string
		var ok bool
		if opts.Only != "" && opts.OnlyKey != "" && de.Name() == opts.Only {
			key, ok = opts.OnlyKey, true
		} else {
			key, ok = serverKeyFor(de.Name(), rootCommit)
		}
		if !ok {
			stats.Skipped++
			name := repoName
			if name == "" {
				name = de.Name()
			}
			stats.SkippedRepos = append(stats.SkippedRepos, name)
			continue
		}
		if repoName == "" {
			if de.Name() == feedback.StoreID {
				repoName = "feedback (Recarto issue reports)"
			} else {
				repoName = de.Name()
			}
		}
		projects = append(projects, project{dirName: de.Name(), repoName: repoName, repoKey: key, rootCommit: rootCommit})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].dirName < projects[j].dirName })
	return projects, nil
}

// serverKeyFor resolves the server bucket key for a per-repo cache-dir store,
// mirroring remotesync.RepoKey's resolution exactly (via the shared
// repokey.Hash formula) so an export lands in the SAME per-repo-key bucket an
// ordinary client push from that repo would use. dirName is the local
// cache-dir id (gitctx.RepoID, itself path-based and machine-local — it is
// NEVER used as the server key) except for the one reserved non-git store,
// the global feedback store, whose dir name IS its server key verbatim
// (feedback.StoreID already satisfies the server's keyPattern).
func serverKeyFor(dirName, rootCommit string) (string, bool) {
	if dirName == feedback.StoreID {
		return feedback.StoreID, true
	}
	if rootCommit == "" {
		return "", false
	}
	return repokey.Hash(rootCommit), true
}

// readMeta reads a store's repo_name/root_commit meta rows (written by
// app.writeMeta). Either may be absent (e.g. the feedback store, which never
// goes through app.Open) — that is not an error, just an empty value.
func readMeta(ctx context.Context, st *store.Store) (repoName, rootCommit string, err error) {
	if repoName, err = readMetaKey(ctx, st, "repo_name"); err != nil {
		return "", "", err
	}
	if rootCommit, err = readMetaKey(ctx, st, "root_commit"); err != nil {
		return "", "", err
	}
	return repoName, rootCommit, nil
}

func readMetaKey(ctx context.Context, st *store.Store, key string) (string, error) {
	var v string
	err := st.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// exportStore writes every memory row (any status) and every tombstone in
// eng's store as ExportEntry lines tagged repoKey, returning the counts.
func exportStore(ctx context.Context, eng *memory.Engine, repoKey string, enc *json.Encoder) (records, tombstones int, err error) {
	recs, err := eng.AllRecords(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, r := range recs {
		entry := syncwire.ExportEntry{RepoKey: repoKey, Record: memory.ToWireRecord(r)}
		if err := enc.Encode(&entry); err != nil {
			return records, tombstones, err
		}
		records++
	}

	tombs, err := eng.AllTombstones(ctx)
	if err != nil {
		return records, tombstones, err
	}
	for _, t := range tombs {
		entry := syncwire.ExportEntry{RepoKey: repoKey, Record: t}
		if err := enc.Encode(&entry); err != nil {
			return records, tombstones, err
		}
		tombstones++
	}
	return records, tombstones, nil
}

package graph

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"sync"

	"git.gerwim.nl/gerwim/recarto/internal/parse"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// indexBatchSize is how many parsed files share one write transaction. Grouping
// files amortizes the per-transaction overhead (connection checkout, BEGIN
// IMMEDIATE/COMMIT, statement preparation) that dominated the old one-tx-per-file
// path on large codebases. Parsing happens before the transaction opens, so the
// write lock is held only for the inserts.
const indexBatchSize = 128

// checkpointEvery is how many files may be written between WAL truncations during
// a reconcile. A large first index would otherwise let the WAL grow unbounded
// (every inserted row buffered until the next passive auto-checkpoint); flushing
// it periodically keeps peak on-disk size in check.
const checkpointEvery = 2000

// parsedFile is a file whose graph has been fully built (in a parse worker) and
// is awaiting its batched write. The built rows (graph) carry everything the
// writer needs, alongside the files-row metadata; the source bytes are not
// retained past construction, keeping the in-flight queue light.
type parsedFile struct {
	path        string
	lang        string
	graph       fileGraph
	hash        string
	mtime, size int64
}

// Reconcile makes the worktree's graph match src: it (re)indexes new and changed
// files (mtime/size pre-check, then content-hash confirm), deletes removed files,
// and resolves name references. Unchanged files are skipped cheaply.
// graphTokenKey prefixes the meta key holding a worktree's freshness token.
const graphTokenKey = "graph_token:"

// resolvePendingKey is the meta key marking that worktree has committed graph
// writes (or dropped indexes) whose resolution has not completed. It is set
// durably before the first destructive step of a mutating reconcile and cleared
// only after resolution succeeds, so a reconcile killed at ANY point — client
// cancellation, crash, power loss — leaves evidence the next reconcile acts on.
// Without it, an abort after the last file write produces an empty worklist next
// run, which would otherwise be indistinguishable from "nothing to do".
func resolvePendingKey(worktree string) string { return "graph_resolve_pending:" + worktree }

// resolvePending reports whether worktree has an interrupted reconcile to finish.
func (e *Engine) resolvePending(ctx context.Context, worktree string) bool {
	var v string
	_ = e.st.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key=?`, resolvePendingKey(worktree)).Scan(&v)
	return v != ""
}

func (e *Engine) setResolvePending(ctx context.Context, worktree string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, '1')
			 ON CONFLICT(key) DO UPDATE SET value = '1'`, resolvePendingKey(worktree))
		return err
	})
}

func (e *Engine) clearResolvePending(ctx context.Context, worktree string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`DELETE FROM meta WHERE key=?`, resolvePendingKey(worktree))
		return err
	})
}

func (e *Engine) Reconcile(ctx context.Context, worktree string, src Source, opts ...ReconcileOption) error {
	var o reconcileOptions
	o.resolve(opts)

	// An interrupted previous reconcile (durable marker), a store whose secondary
	// indexes are physically missing (aborted bulk load, possibly under an older
	// binary that wrote no marker), or an explicit WithForce all force the
	// rebuild-indexes + full-resolve path below even when no file changed.
	pending := o.force || e.resolvePending(ctx, worktree) || e.graphIndexesMissing(ctx)

	entries, err := src.List()
	if err != nil {
		return err
	}

	// Load current index state for this worktree.
	type rec struct {
		hash        string
		mtime, size int64
	}
	have := map[string]rec{}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT path, content_hash, mtime, size FROM files WHERE worktree=?`, worktree)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		var r rec
		if err := rows.Scan(&p, &r.hash, &r.mtime, &r.size); err != nil {
			rows.Close()
			return err
		}
		have[p] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// Pass 1: a cheap mtime/size pre-check splits source files into those that
	// may have changed (the worklist, which we report as the progress total) and
	// those skipped without any IO. Reading + hashing happens in pass 2.
	seen := map[string]bool{}
	var worklist []FileEntry
	for _, fe := range entries {
		seen[fe.Path] = true
		prev, known := have[fe.Path]
		if known && prev.mtime == fe.Mtime && prev.size == fe.Size {
			continue // cheap pre-check: unchanged
		}
		worklist = append(worklist, fe)
	}

	// The dirty set drives incremental resolve: the paths this reconcile rewrites
	// (worklist) plus the paths it deletes. A reconcile over an already-indexed
	// worktree (have non-empty) that touches only a bounded number of files can
	// re-resolve just the affected edges instead of the whole worktree (see
	// ResolveIncremental); a first index, or a sweeping change, takes the full path.
	dirtyPaths := make([]string, 0, len(worklist))
	for _, fe := range worklist {
		dirtyPaths = append(dirtyPaths, fe.Path)
	}
	deletions := 0
	for p := range have {
		if !seen[p] {
			dirtyPaths = append(dirtyPaths, p)
			deletions++
		}
	}
	// Incremental resolve trusts the store's existing resolution as its baseline;
	// a pending recovery means that baseline is incomplete (Resolve nulls call
	// edges before rebuilding them), so recovery always takes the full path.
	incremental := !pending && len(have) > 0 && len(dirtyPaths) > 0 && len(dirtyPaths) <= maxIncrementalDirty
	// Pre-edit signals, captured BEFORE the rewrite deletes the old rows: the names
	// a dirty file held (so a removed/renamed symbol still re-resolves the edges
	// that pointed at it) and its `extends` edges (so a structural change falls back
	// to a full resolve — see ResolveIncremental).
	var oldNames, oldImports []string
	var oldExtends map[string]struct{}
	if incremental {
		var err error
		if oldNames, oldImports, err = e.namesForPaths(ctx, worktree, dirtyPaths); err != nil {
			return err
		}
		if oldExtends, err = e.extendsSigForPaths(ctx, worktree, dirtyPaths); err != nil {
			return err
		}
	}

	// Pass 2: read + parse worklist files concurrently (tree-sitter parsing is
	// CPU-bound and embarrassingly parallel), then write the extracted graph in
	// serialized batches. A worker pool feeds a single writer, since SQLite has one
	// writer; this keeps every core busy parsing while the lock is held only for
	// inserts. mutated records whether the graph actually changed (so an unchanged
	// worktree can skip the expensive Resolve pass).
	// A fresh index of a large repo bulk-writes ~1M rows. Dropping the secondary
	// indexes first and rebuilding them once over the finished tables (below, before
	// Resolve) avoids per-row index maintenance across all those inserts — ~40%
	// faster writes / ~12% faster first index on graphql-platform. Gated to a true
	// first index (no rows yet) of a large repo, so incremental reindexes — which
	// would pay a full rebuild to touch a few files — keep maintaining indexes inline.
	// Set the pending marker durably BEFORE the first destructive step (the bulk
	// index drop, or the first batch write inside indexWorklist), so an abort at
	// any later point is detectable next run. A worklist of only-touched files
	// that all skip on content hash still pays the marker (it is cleared, without
	// a resolve, at the end); actual mutation cannot be known before parsing.
	willMutate := len(worklist) > 0 || deletions > 0
	if willMutate && !pending {
		if err := e.setResolvePending(ctx, worktree); err != nil {
			return err
		}
	}

	// Bulk-load only when the WHOLE store is empty, not just this worktree: the
	// secondary indexes are shared by every worktree in the per-repo store, so
	// dropping them during the first index of an ADDITIONAL worktree would make
	// every other worktree's queries full-scan for the entire run — and the
	// rebuild would pay for all worktrees' rows, not just this one's. (The ~40%
	// write win was measured on a truly empty store.)
	storeEmpty := false
	if len(have) == 0 {
		var one int
		storeEmpty = errors.Is(
			e.st.DB.QueryRowContext(ctx, `SELECT 1 FROM files LIMIT 1`).Scan(&one),
			sql.ErrNoRows)
	}
	bulkLoad := storeEmpty && len(worklist) > bulkLoadMinFiles
	if bulkLoad {
		if err := e.dropGraphSecondaryIndexes(ctx); err != nil {
			return err
		}
	}

	o.progress.Start(len(worklist))
	mutated := false
	if len(worklist) > 0 {
		wrote, err := e.indexWorklist(ctx, worktree, src, worklist,
			func(path string) (prevHash string, known bool) {
				r, ok := have[path]
				return r.hash, ok
			}, o.progress)
		if err != nil {
			return err
		}
		mutated = wrote
	}

	// Delete files that disappeared (CASCADE removes their nodes/edges; FTS rows
	// are dropped explicitly since FTS5 is not linked).
	for p := range have {
		if seen[p] {
			continue
		}
		if err := e.deleteFile(ctx, worktree, p); err != nil {
			return err
		}
		mutated = true
	}
	// Resolution depends only on the stored nodes/edges. If this reconcile changed
	// nothing (e.g. the freshness token moved but no source file did), the existing
	// resolution is still correct — skip the full-worktree re-resolve. A pending
	// recovery (or WithForce) overrides that: the store's resolution is suspect, so
	// resolve even with an empty worklist.
	if mutated || pending {
		// Ensure the secondary indexes exist before resolving (Resolve keys on
		// idx_nodes_name / idx_type_bindings_file_name / ...). After a bulk first
		// index this rebuilds, in one pass each, the indexes dropped above; otherwise
		// it is a cheap IF NOT EXISTS no-op that also self-heals a store left without
		// them by a crash between the drop and this rebuild.
		if err := e.createGraphSecondaryIndexes(ctx); err != nil {
			return err
		}
		// Incremental resolve is exact only while the worktree's interface/base
		// structure is unchanged (ResolveIncremental); a dirty file that gained or
		// lost an `extends` edge can redirect interface dispatch in OTHER files,
		// which the name-scoped affected set cannot see — so re-resolve fully then
		// (and when a Python module others name appeared or went: see
		// structureChanged).
		useIncremental := incremental
		if useIncremental {
			newExtends, err := e.extendsSigForPaths(ctx, worktree, dirtyPaths)
			if err != nil {
				return err
			}
			full, err := e.structureChanged(ctx, worktree, oldExtends, newExtends)
			if err != nil {
				return err
			}
			useIncremental = !full
		}
		if useIncremental {
			// The incremental path has no sub-phases of its own; announce the base
			// "resolving references" label so observers (progress bar, MCP freshness
			// hint, the crash-safety cancel hook) still see the resolve stage begin.
			// A full Resolve emits the base label (and its sub-phases) itself.
			o.progress.Phase("resolving references")
			if err := e.ResolveIncremental(ctx, worktree, dirtyPaths, oldNames, oldImports); err != nil {
				return err
			}
		} else if err := e.Resolve(ctx, worktree, o.progress); err != nil {
			return err
		}
	}
	// Every committed write is now resolved (or nothing was written at all):
	// drop the pending marker. Clearing any earlier would reopen the crash
	// window the marker exists to close.
	if willMutate || pending {
		if err := e.clearResolvePending(ctx, worktree); err != nil {
			return err
		}
	}
	// Semantic tier (no-op unless an embedder is set): bring the vector index up
	// to date. Best-effort — a model error must not fail reconciliation, since
	// the lexical graph is already written and fully usable on its own.
	if e.emb != nil {
		_ = e.embedWorktree(ctx, worktree)
	}
	// Flush the WAL once per mutating reconcile. The resolve phase writes the
	// most bytes of the whole run (edge updates, candidates, index rebuilds) and
	// lands AFTER the last in-loop checkpoint; without this, every completed
	// index leaves all of that in the WAL. Best-effort: TRUNCATE yields to
	// concurrent readers, and the Stop hook retries at the next idle boundary.
	if mutated || pending {
		_ = e.st.Checkpoint(ctx)
	}
	o.progress.Done()
	return nil
}

// indexWorklist reads, parses, and writes every file in worklist, parsing in a
// worker pool and writing from a single goroutine in batched transactions. It
// reports one progress tick per file and returns whether any file was actually
// (re)written (a worklist entry whose content turned out identical writes
// nothing). prevHash looks up a path's last-indexed content hash for the cheap
// "mtime moved but bytes unchanged" skip.
func (e *Engine) indexWorklist(ctx context.Context, worktree string, src Source,
	worklist []FileEntry, prevHash func(path string) (string, bool), prog Progress) (bool, error) {

	// outcome is a worker's result for one file: a parsed graph to write, a skip
	// (content unchanged / unparseable), or an error.
	type outcome struct {
		pf   parsedFile
		skip bool
		err  error
	}
	readParse := func(fe FileEntry) outcome {
		content, err := src.Read(fe.Path)
		if err != nil {
			return outcome{err: err}
		}
		hash := sha256hex(content)
		if prev, known := prevHash(fe.Path); known && prev == hash {
			return outcome{skip: true} // mtime moved but content identical
		}
		lang, ok := parse.LangForExt(fe.Path)
		if !ok {
			return outcome{skip: true} // not parseable (List normally filters these out)
		}
		res, err := e.reg.ParseFile(lang, fe.Path, content)
		if err != nil {
			return outcome{err: err}
		}
		// Build the full graph here, in the parse worker, so the serial writer only
		// binds and inserts (graph construction is otherwise the writer's CPU floor).
		return outcome{pf: parsedFile{
			path: fe.Path, lang: lang, graph: buildFileGraph(content, res),
			hash: hash, mtime: fe.Mtime, size: fe.Size,
		}}
	}

	nworkers := runtime.NumCPU()
	if nworkers > len(worklist) {
		nworkers = len(worklist)
	}
	if nworkers < 1 {
		nworkers = 1
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan FileEntry)
	// The writer drains results into a batch, then stops draining for the ~0.4s it
	// holds the write lock flushing that batch. With only nworkers of buffer the
	// pool fills it and stalls for the rest of every flush — on a large first index
	// that idled the parse cores ~50% of wall. Buffer several batches so the workers
	// keep parsing the next batches ahead while the writer flushes the current one;
	// the pool then runs continuously and the serial writer becomes the sole floor.
	resultsBuf := 4 * indexBatchSize
	if resultsBuf > len(worklist) {
		resultsBuf = len(worklist)
	}
	results := make(chan outcome, resultsBuf)
	var wg sync.WaitGroup
	for range nworkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fe := range jobs {
				select {
				case results <- readParse(fe):
				case <-wctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, fe := range worklist {
			select {
			case jobs <- fe:
			case <-wctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	// Single writer: batch parsed graphs into one transaction each (prepared
	// statements reused across the batch) and truncate the WAL periodically.
	batch := make([]parsedFile, 0, indexBatchSize)
	sinceCheckpoint := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		now := e.clk.Now().UnixMilli()
		if err := e.st.WriteTx(ctx, func(c store.Conn) error {
			st, err := prepareWriteStmts(ctx, c)
			if err != nil {
				return err
			}
			defer st.Close()
			for _, pf := range batch {
				if err := e.writeFileGraph(ctx, c, st, worktree, pf.path, pf.lang,
					pf.graph, pf.hash, pf.mtime, pf.size, now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		sinceCheckpoint += len(batch)
		batch = batch[:0]
		if sinceCheckpoint >= checkpointEvery {
			sinceCheckpoint = 0
			_ = e.st.Checkpoint(ctx) // best-effort: bound WAL growth
		}
		return nil
	}

	wrote := false
	var indexErr error
	for out := range results {
		if out.err != nil {
			if indexErr == nil {
				indexErr = out.err
				cancel() // stop the pool; drain the rest below
			}
			continue
		}
		prog.Inc()
		if out.skip {
			continue
		}
		batch = append(batch, out.pf)
		wrote = true
		if len(batch) >= indexBatchSize {
			if err := flush(); err != nil {
				indexErr = err
				cancel()
				break
			}
		}
	}
	if indexErr != nil {
		for range results { // unblock any in-flight workers so they can exit
		}
		return false, indexErr
	}
	if err := flush(); err != nil {
		return false, err
	}
	return wrote, nil
}

// ReconcileIfStale reconciles only when the worktree's git token differs from the
// token stored at the last reconcile. An empty token (non-git worktree) always
// reconciles. On a successful reconcile it records the new token. This makes
// query-time freshness cheap: an unchanged worktree skips the file walk entirely.
//
// The git token (see WorktreeToken) folds in the mtime+size of every dirty file,
// making it exactly as precise as Reconcile's own mtime/size pre-check: even
// re-editing an ALREADY-dirty file (whose porcelain line stays "M <path>") bumps
// the token and triggers a reconcile. The only residual gap is a re-edit that
// leaves both mtime and size byte-for-byte identical, which a real write does not
// produce.
func (e *Engine) ReconcileIfStale(ctx context.Context, worktree string, src Source, token string, opts ...ReconcileOption) error {
	var o reconcileOptions
	for _, opt := range opts {
		opt(&o)
	}
	// A matching token alone is not proof of health: a reconcile that died
	// mid-recovery leaves the pending marker (or physically missing indexes)
	// behind a token an earlier run stamped, so graphFresh checks all three.
	// Each is a microsecond point lookup, keeping the per-query check cheap.
	if !o.force && e.graphFresh(ctx, worktree, token) {
		return nil
	}
	if err := e.Reconcile(ctx, worktree, src, opts...); err != nil {
		return err
	}
	if token != "" {
		if err := e.st.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				graphTokenKey+worktree, token)
			return err
		}); err != nil {
			return err
		}
		// The stamp itself appended a WAL frame after Reconcile's flush; clear it
		// so a completed reconcile leaves the WAL empty (best-effort, like all
		// checkpoints: it yields to concurrent readers).
		_ = e.st.Checkpoint(ctx)
	}
	return nil
}

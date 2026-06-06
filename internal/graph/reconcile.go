package graph

import (
	"context"
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

// parsedFile is a file whose graph has been extracted and is awaiting its batched
// write. Content is retained (src) so the node-signature column can be populated
// at write time, alongside the extracted result and the files-row metadata.
type parsedFile struct {
	path        string
	lang        string
	src         []byte
	res         parse.Result
	hash        string
	mtime, size int64
}

// Reconcile makes the worktree's graph match src: it (re)indexes new and changed
// files (mtime/size pre-check, then content-hash confirm), deletes removed files,
// and resolves name references. Unchanged files are skipped cheaply.
func (e *Engine) Reconcile(ctx context.Context, worktree string, src Source, opts ...ReconcileOption) error {
	var o reconcileOptions
	o.resolve(opts)

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

	// Pass 2: read + parse worklist files concurrently (tree-sitter parsing is
	// CPU-bound and embarrassingly parallel), then write the extracted graph in
	// serialized batches. A worker pool feeds a single writer, since SQLite has one
	// writer; this keeps every core busy parsing while the lock is held only for
	// inserts. mutated records whether the graph actually changed (so an unchanged
	// worktree can skip the expensive Resolve pass).
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
	// resolution is still correct — skip the full-worktree re-resolve.
	if mutated {
		o.progress.Phase("resolving references")
		if err := e.Resolve(ctx, worktree); err != nil {
			return err
		}
	}
	// Semantic tier (no-op unless an embedder is set): bring the vector index up
	// to date. Best-effort — a model error must not fail reconciliation, since
	// the lexical graph is already written and fully usable on its own.
	if e.emb != nil {
		_ = e.embedWorktree(ctx, worktree)
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
		return outcome{pf: parsedFile{
			path: fe.Path, lang: lang, src: content, res: res, hash: hash, mtime: fe.Mtime, size: fe.Size,
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
	results := make(chan outcome, nworkers)
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
					pf.src, pf.res, pf.hash, pf.mtime, pf.size, now); err != nil {
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
	const keyPrefix = "graph_token:"
	if token != "" {
		var stored string
		_ = e.st.DB.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key=?`, keyPrefix+worktree).Scan(&stored)
		if stored == token {
			return nil
		}
	}
	if err := e.Reconcile(ctx, worktree, src, opts...); err != nil {
		return err
	}
	if token != "" {
		return e.st.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				keyPrefix+worktree, token)
			return err
		})
	}
	return nil
}

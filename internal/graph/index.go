package graph

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/parse"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// IndexFile parses src and atomically (re)writes the file's nodes/edges and its
// files row. Unsupported extensions are skipped (no error). It runs in one short
// WriteTx so a file's graph is replaced atomically and the lock is held briefly.
func (e *Engine) IndexFile(ctx context.Context, worktree, path string, src []byte, mtime, size int64) error {
	lang, ok := parse.LangForExt(path)
	if !ok {
		return nil
	}
	res, err := e.reg.ParseFile(lang, path, src)
	if err != nil {
		return err
	}
	g := buildFileGraph(src, res)
	hash := sha256hex(src)
	now := e.clk.Now().UnixMilli()
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		st, err := prepareWriteStmts(ctx, c)
		if err != nil {
			return err
		}
		defer st.Close()
		return e.writeFileGraph(ctx, c, st, worktree, path, lang, g, hash, mtime, size, now)
	})
}

// ReindexFileIncremental rewrites one file's graph and re-resolves only what
// that edit can affect, with the same guards Reconcile applies: the names whose
// nodes in the file changed scope the affected edge set, and a change to its
// `extends` edges re-scopes the methods of the class that carries it, since
// interface dispatch in OTHER files reads them (structureChanged). Only a
// Python base change resolves fully. This is the post-edit hook path — a full
// Resolve per keystroke cost ~14s on a large repo for a one-file change.
func (e *Engine) ReindexFileIncremental(ctx context.Context, worktree, path string, src []byte, mtime, size int64) error {
	dirty := []string{path}
	old, err := e.captureDirty(ctx, worktree, dirty)
	if err != nil {
		return err
	}
	oldExtends, err := e.extendsSigForPaths(ctx, worktree, dirty)
	if err != nil {
		return err
	}
	if err := e.IndexFile(ctx, worktree, path, src, mtime, size); err != nil {
		return err
	}
	newExtends, err := e.extendsSigForPaths(ctx, worktree, dirty)
	if err != nil {
		return err
	}
	full, derived, err := e.structureChanged(ctx, worktree, oldExtends, newExtends)
	if err != nil {
		return err
	}
	if !full {
		return e.ResolveIncremental(ctx, worktree, dirty, old, derived)
	}
	return e.Resolve(ctx, worktree, noopProgress{})
}

// writeStmts holds the per-row INSERT statements of the file-graph write path,
// prepared once per transaction so a batch of files reuses them (compiling each
// statement once instead of once per row — a large saving with the pure-Go
// SQLite driver). All statements are bound to the transaction's connection.
//
// Per-row prepared statements are the measured optimum here: a 200-row
// multi-VALUES variant made the writer ~40% SLOWER on modernc (big bind arrays
// + VALUES coroutine, with no per-call round trip to amortize in an embedded
// driver) — don't reintroduce it.
type writeStmts struct {
	node     *sql.Stmt
	nodeUpd  *sql.Stmt
	ftsRow   *sql.Stmt
	fts      *sql.Stmt
	ftsStem  *sql.Stmt
	ftsTri   *sql.Stmt
	edge     *sql.Stmt
	binding  *sql.Stmt
	contains *sql.Stmt
}

// prepareWriteStmts compiles the hot insert statements on c. The caller must
// Close the result before the transaction commits.
func prepareWriteStmts(ctx context.Context, c store.Conn) (*writeStmts, error) {
	var s writeStmts
	var err error
	prep := func(q string) *sql.Stmt {
		if err != nil {
			return nil
		}
		var st *sql.Stmt
		st, err = c.PrepareContext(ctx, q)
		return st
	}
	s.node = prep(`INSERT INTO nodes(id,file_id,kind,name,qualified_name,parent_name,signature,
		   start_byte,end_byte,start_line,end_line)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	s.nodeUpd = prep(`UPDATE nodes SET parent_name=?,signature=?,start_byte=?,end_byte=?,start_line=?,end_line=?
		 WHERE id=?`)
	s.ftsRow = prep(`INSERT INTO node_fts_rows(node_id) VALUES (?)`)
	// OR REPLACE: a mapping rowid may land on an orphaned FTS row (its node deleted
	// before node_fts_rows existed); overwrite it instead of failing.
	s.fts = prep(`INSERT OR REPLACE INTO nodes_fts(rowid,node_id,name,qualified_name) VALUES (?,?,?,?)`)
	s.ftsStem = prep(`INSERT OR REPLACE INTO nodes_fts_stem(rowid,node_id,name,qualified_name) VALUES (?,?,?,?)`)
	s.ftsTri = prep(`INSERT OR REPLACE INTO nodes_fts_tri(rowid,node_id,content) VALUES (?,?,?)`)
	s.edge = prep(`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,dst_recv,kind,file_id,line)
		 VALUES (?,?,?,?,?,?,?,?)`)
	s.binding = prep(`INSERT INTO type_bindings(id,file_id,name,type_name,scope_start_byte,scope_end_byte)
		 VALUES (?,?,?,?,?,?)`)
	s.contains = prep(`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,kind,file_id,line)
		 VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &s, nil
}

// Close releases the prepared statements (safe on a partially-prepared set).
func (s *writeStmts) Close() {
	for _, st := range []*sql.Stmt{s.node, s.nodeUpd, s.ftsRow, s.fts, s.ftsStem, s.ftsTri, s.edge, s.binding, s.contains} {
		if st != nil {
			_ = st.Close()
		}
	}
}

// lineStart returns the byte offset of the start of the line containing pos.
func lineStart(src []byte, pos uint) uint {
	if int(pos) > len(src) {
		pos = uint(len(src))
	}
	if i := bytes.LastIndexByte(src[:pos], '\n'); i >= 0 {
		return uint(i + 1)
	}
	return 0
}

// signatureOf returns the symbol's first source line (trimmed, capped), used as a
// cheap one-line signature surfaced in search results.
func signatureOf(src []byte, start uint, max int) string {
	if int(start) >= len(src) {
		return ""
	}
	line := src[start:]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	s := strings.TrimSpace(string(line))
	if len(s) > max {
		// Cap is by bytes; dropping an incomplete trailing rune (a multibyte rune
		// split at the boundary) keeps the result valid UTF-8.
		s = strings.ToValidUTF8(s[:max], "")
	}
	return s
}

// fileGraph is a file's fully-built graph rows, ready for insertion. Every value
// EXCEPT each row's file_id (assigned at write time from the files-row upsert) is
// computed by buildFileGraph, so the expensive construction — qualified names,
// FTS text, signatures, and the O(n) byte-span containment lookups — runs in the
// parallel parse workers instead of on the single serial writer. Node ids and the
// internal references between rows (edge src, contains parent/child) are
// worker-generated and independent of file_id.
type fileGraph struct {
	nodes    []nodeRow
	edges    []edgeRow
	bindings []bindingRow
	contains []containsRow
}

type nodeRow struct {
	id, kind, name, qn, parentName, signature string
	startByte, endByte, startLine, endLine    int64
	ftsName, ftsQN, ftsTri                    string
}

type edgeRow struct {
	id                  string
	src                 any // node id, or nil for a file-scope reference
	dstName, recv, kind string
	line                int64
}

type bindingRow struct {
	id, name, typeName   string
	scopeStart, scopeEnd int64
}

type containsRow struct {
	id, parentID, childID string
	line                  int64
}

// buildFileGraph turns a parsed file into its insertable graph rows. It is pure
// (no DB, no shared state beyond ids.New) so it can run concurrently across the
// parse worker pool; the serial writer (writeFileGraph) then only binds and
// inserts. The computation mirrors the former in-writer logic exactly, so the
// stored graph is byte-identical modulo the (always-random) UUIDv7 ids.
func buildFileGraph(src []byte, res parse.Result) fileGraph {
	// Pass 1: assign ids and byte spans (in lockstep with res.Symbols) so a
	// symbol's qualified name can be derived from the full structural context of
	// the file before any row is built.
	spans := make([]span, len(res.Symbols))
	for i, s := range res.Symbols {
		spans[i] = span{id: ids.New(), start: s.StartByte, end: s.EndByte}
	}

	// Precompute, once for the file, each node's innermost structural parent and
	// the namespace nodes. qualifiedName and the contains edges both need these;
	// deriving them per symbol meant rescanning every span per symbol — O(n^2) that
	// pinned one worker for seconds on huge generated files (20k+ symbols).
	parentIdx := make([]int, len(spans))
	var nsIdx []int
	for i := range spans {
		parentIdx[i] = innermostContainerIdx(spans, i)
		if res.Symbols[i].Kind == "namespace" {
			nsIdx = append(nsIdx, i)
		}
	}

	g := fileGraph{
		nodes:    make([]nodeRow, 0, len(res.Symbols)),
		edges:    make([]edgeRow, 0, len(res.Edges)),
		bindings: make([]bindingRow, 0, len(res.Bindings)),
		contains: make([]containsRow, 0, len(res.Symbols)),
	}

	// Pass 2: nodes with structural qualified names + FTS content.
	for i, s := range res.Symbols {
		qn := qualifiedName(spans, res.Symbols, parentIdx, nsIdx, i)
		// Go methods are siblings of their type (not nested), so structural
		// containment yields a bare name; prepend the captured receiver type.
		if s.RecvType != "" && !strings.Contains(qn, ".") {
			qn = s.RecvType + "." + s.Name
		}
		// Store identifier-split components alongside the raw name so a query for
		// a single component word ("Renderer") matches a PascalCase symbol
		// ("MessageRenderer"); the default unicode61 tokenizer cannot split
		// camelCase on its own. The dotted qualified name also feeds the FTS so a
		// namespace/class word ("Domain") surfaces its members.
		// Primary exact index (unicode61) + the porter-stemmed broadener share the
		// same identifier-split content; the trigram broadener stores the raw name +
		// qualified name for substring matching.
		ftsName, ftsQN := ftsText(s.Name), ftsText(qn)
		// The reported line and one-line signature anchor on the line declaring
		// the symbol's NAME, not the span start: a C# attribute (or TS decorator)
		// is part of the declaration node, so the span starts at the attribute and
		// would misreport both the line and the signature ("[Obsolete(...)]"
		// instead of the method header). The byte span stays attribute-inclusive —
		// snippets should show the attribute.
		declLine, sigFrom := s.StartLine, s.StartByte
		if s.NameLine > 0 {
			declLine = s.NameLine
			sigFrom = lineStart(src, s.NameByte)
		}
		g.nodes = append(g.nodes, nodeRow{
			id: spans[i].id, kind: s.Kind, name: s.Name, qn: qn, parentName: parentSegment(qn),
			signature: signatureOf(src, sigFrom, 200),
			startByte: int64(s.StartByte), endByte: int64(s.EndByte),
			startLine: int64(declLine), endLine: int64(s.EndLine),
			ftsName: ftsName, ftsQN: ftsQN, ftsTri: s.Name + " " + qn,
		})
	}

	// Edges, attributed to the innermost enclosing node.
	for _, ed := range res.Edges {
		g.edges = append(g.edges, edgeRow{
			id: ids.New(), src: nullStr(enclosing(spans, ed.StartByte)),
			dstName: ed.DstName, recv: ed.Recv, kind: ed.Kind, line: int64(ed.Line),
		})
	}

	// Type bindings: scope each to its innermost enclosing definition's byte span
	// (class body for fields, method body for params/locals/receiver vars). A
	// binding at file scope (no enclosing def) covers up to the last node end.
	var maxEnd uint
	for _, sp := range spans {
		if sp.end > maxEnd {
			maxEnd = sp.end
		}
	}
	for _, b := range res.Bindings {
		scStart, scEnd := uint(0), maxEnd
		if encID := enclosing(spans, b.StartByte); encID != "" {
			for _, sp := range spans {
				if sp.id == encID {
					scStart, scEnd = sp.start, sp.end
					break
				}
			}
		}
		g.bindings = append(g.bindings, bindingRow{
			id: ids.New(), name: b.Name, typeName: b.TypeName,
			scopeStart: int64(scStart), scopeEnd: int64(scEnd),
		})
	}

	// Structural containment edges: each symbol nested inside another emits a
	// `contains` edge (parent -> child). Both endpoints are nodes in this same
	// file, so they are linked directly by id (no name resolution). spans is in
	// lockstep with res.Symbols, so spans[i] corresponds to res.Symbols[i].
	for i, child := range spans {
		p := parentIdx[i]
		if p < 0 {
			continue
		}
		g.contains = append(g.contains, containsRow{
			id: ids.New(), parentID: spans[p].id, childID: child.id, line: int64(res.Symbols[i].StartLine),
		})
	}
	return g
}

// writeFileGraph upserts the files row and writes the pre-built graph g for it.
// It runs on the single serial writer and does only the upsert, the prepared
// INSERTs and one UPDATE per surviving node — all per-row computation already
// happened in buildFileGraph — so the writer is no longer the place the index
// spends CPU. A node whose identity and ordinal survived the edit keeps its id
// and gets its position and signature refreshed; only the rest are replaced.
func (e *Engine) writeFileGraph(ctx context.Context, c store.Conn, st *writeStmts, worktree, path, lang string,
	g fileGraph, hash string, mtime, size, now int64) error {
	var kept map[string]bool
	// Upsert the files row and obtain its id.
	var fileID string
	err := c.QueryRowContext(ctx,
		`SELECT id FROM files WHERE worktree=? AND path=?`, worktree, path).Scan(&fileID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fileID = ids.New()
		if _, err := c.ExecContext(ctx,
			`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			fileID, worktree, path, lang, hash, mtime, size, now); err != nil {
			return err
		}
		// A first index normally journals nothing (fresh-clone flood guard), but
		// a path with earlier journal rows is a delete-then-recreate (branch
		// switch, stash pop): journal its symbols as added so the prior
		// "removed" folds to a present disposition instead of standing as false.
		if err := journalRecreatedFileDeltas(ctx, c, worktree, path, g.nodes, now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// Journal symbol-level deltas before the old rows are replaced — this is
		// the only moment both node sets exist. Genuinely new files journal
		// nothing (see the ErrNoRows recreate carve-out above).
		if err := journalFileDeltas(ctx, c, worktree, path, fileID, g.nodes, now); err != nil {
			return err
		}
		// Keep the ids of nodes whose identity survived the edit, so edges and
		// candidates elsewhere that point at them stay valid (no ON DELETE SET
		// NULL / CASCADE) and incremental resolve can skip them (see nameSeqs).
		if kept, err = reuseNodeIDs(ctx, c, fileID, g); err != nil {
			return err
		}
		keepIDs := make([]string, 0, len(kept)) // never nil: JSON null would make NOT IN match nothing
		for id := range kept {
			keepIDs = append(keepIDs, id)
		}
		keepJSON, err := json.Marshal(keepIDs)
		if err != nil {
			return err
		}
		// Drop the replaced nodes' FTS rows before deleting them (FTS5 is not
		// linked); kept nodes keep theirs, as their name/qualified name match.
		for _, q := range []string{
			`DELETE FROM nodes_fts WHERE rowid IN (SELECT fts_rowid FROM node_fts_rows WHERE node_id IN (` + staleNodes + `))`,
			`DELETE FROM nodes_fts_stem WHERE rowid IN (SELECT fts_rowid FROM node_fts_rows WHERE node_id IN (` + staleNodes + `))`,
			`DELETE FROM nodes_fts_tri WHERE rowid IN (SELECT fts_rowid FROM node_fts_rows WHERE node_id IN (` + staleNodes + `))`,
			`DELETE FROM nodes WHERE id IN (` + staleNodes + `)`,
		} {
			if _, err := c.ExecContext(ctx, q, fileID, string(keepJSON)); err != nil {
				return err
			}
		}
		for _, q := range []string{
			`DELETE FROM edges WHERE file_id=?`,
			`DELETE FROM type_bindings WHERE file_id=?`,
		} {
			if _, err := c.ExecContext(ctx, q, fileID); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE files SET language=?,content_hash=?,mtime=?,size=?,last_indexed_at=? WHERE id=?`,
			lang, hash, mtime, size, now, fileID); err != nil {
			return err
		}
	}

	for i := range g.nodes {
		n := &g.nodes[i]
		if kept[n.id] {
			if _, err := st.nodeUpd.ExecContext(ctx, n.parentName, n.signature,
				n.startByte, n.endByte, n.startLine, n.endLine, n.id); err != nil {
				return err
			}
			continue
		}
		if _, err := st.node.ExecContext(ctx,
			n.id, fileID, n.kind, n.name, n.qn, n.parentName, n.signature,
			n.startByte, n.endByte, n.startLine, n.endLine); err != nil {
			return err
		}
		result, err := st.ftsRow.ExecContext(ctx, n.id)
		if err != nil {
			return err
		}
		ftsRowID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := st.fts.ExecContext(ctx, ftsRowID, n.id, n.ftsName, n.ftsQN); err != nil {
			return err
		}
		if _, err := st.ftsStem.ExecContext(ctx, ftsRowID, n.id, n.ftsName, n.ftsQN); err != nil {
			return err
		}
		if _, err := st.ftsTri.ExecContext(ctx, ftsRowID, n.id, n.ftsTri); err != nil {
			return err
		}
	}
	for i := range g.edges {
		ed := &g.edges[i]
		if _, err := st.edge.ExecContext(ctx,
			ed.id, ed.src, nil, ed.dstName, ed.recv, ed.kind, fileID, ed.line); err != nil {
			return err
		}
	}
	for i := range g.bindings {
		b := &g.bindings[i]
		if _, err := st.binding.ExecContext(ctx,
			b.id, fileID, b.name, b.typeName, b.scopeStart, b.scopeEnd); err != nil {
			return err
		}
	}
	for i := range g.contains {
		cn := &g.contains[i]
		if _, err := st.contains.ExecContext(ctx,
			cn.id, cn.parentID, cn.childID, "", "contains", fileID, cn.line); err != nil {
			return err
		}
	}
	return nil
}

// staleNodes selects a file's nodes (arg 1) except the kept ids (arg 2, a JSON array).
const staleNodes = `SELECT id FROM nodes WHERE file_id=? AND id NOT IN (SELECT value FROM json_each(?))`

// nodeIdentity is what a node's id is kept by across a reindex, together with
// its ordinal among the file's nodes of equal identity in nodeLess order. Name,
// kind and qualified name (hence parent_name) are everything a resolver matches
// a target node on; the embedding and FTS text derive from them too.
func nodeIdentity(kind, qn, name string) string { return kind + "\x00" + qn + "\x00" + name }

// nodeOrderSQL / nodeLess are the one canonical node order (span start, outer
// first, then kind and qualified name) both reuseNodeIDs and nameSeqs number
// ordinals in.
const nodeOrderSQL = `n.start_byte, n.end_byte DESC, n.kind, n.qualified_name, n.id`

func nodeLess(a, b *nodeRow) bool {
	if a.startByte != b.startByte {
		return a.startByte < b.startByte
	}
	if a.endByte != b.endByte {
		return a.endByte > b.endByte
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	return a.qn < b.qn
}

// reuseNodeIDs gives each node of g the id of the file's existing node with the
// same identity and ordinal, rewriting g's edge sources and contains endpoints to
// match, and returns the reused ids.
func reuseNodeIDs(ctx context.Context, c store.Conn, fileID string, g fileGraph) (map[string]bool, error) {
	rows, err := c.QueryContext(ctx, `SELECT n.id, n.kind, n.qualified_name, n.name FROM nodes n
		 WHERE n.file_id=? ORDER BY `+nodeOrderSQL, fileID)
	if err != nil {
		return nil, err
	}
	old := map[string][]string{}
	for rows.Next() {
		var id, kind, qn, name string
		if err := rows.Scan(&id, &kind, &qn, &name); err != nil {
			rows.Close()
			return nil, err
		}
		k := nodeIdentity(kind, qn, name)
		old[k] = append(old[k], id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	order := make([]int, len(g.nodes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return nodeLess(&g.nodes[order[a]], &g.nodes[order[b]]) })
	kept := map[string]bool{}
	remap := map[string]string{}
	seen := map[string]int{}
	for _, i := range order {
		n := &g.nodes[i]
		k := nodeIdentity(n.kind, n.qn, n.name)
		ord := seen[k]
		seen[k]++
		if ord < len(old[k]) {
			remap[n.id] = old[k][ord]
			n.id = old[k][ord]
			kept[n.id] = true
		}
	}
	for i := range g.edges {
		if s, ok := g.edges[i].src.(string); ok && remap[s] != "" {
			g.edges[i].src = remap[s]
		}
	}
	for i := range g.contains {
		cn := &g.contains[i]
		if o := remap[cn.parentID]; o != "" {
			cn.parentID = o
		}
		if o := remap[cn.childID]; o != "" {
			cn.childID = o
		}
	}
	return kept, nil
}

// qualifiedName builds a dotted structural name for the symbol at syms[i]. It
// joins, outermost-first: any namespace that governs the symbol but does not
// span-contain it (a C# file-scoped namespace `namespace N;` precedes its types
// without enclosing them), then the chain of enclosing definitions (block
// namespaces, classes) found by byte containment, then the symbol's own name.
// spans is in lockstep with syms (spans[i] is syms[i]).
// parentIdx[k] is spans[k]'s innermost container index (or -1) and nsIdx lists the
// namespace symbols' indices; both are precomputed once per file by buildFileGraph
// so this stays O(namespaces + depth) per symbol instead of rescanning all spans.
func qualifiedName(spans []span, syms []parse.Symbol, parentIdx, nsIdx []int, i int) string {
	self := spans[i]
	contains := func(outer, inner span) bool {
		return outer.start <= inner.start && inner.end <= outer.end
	}
	// Governing namespaces that precede the symbol but do not enclose it. Block
	// namespaces that DO enclose the symbol are excluded here — they are picked up
	// by the containment chain below, so neither path double-counts them.
	type pre struct {
		start uint
		name  string
	}
	var prefixes []pre
	for _, j := range nsIdx {
		if j == i {
			continue
		}
		if spans[j].start <= self.start && !contains(spans[j], self) {
			prefixes = append(prefixes, pre{spans[j].start, syms[j].Name})
		}
	}
	sort.Slice(prefixes, func(a, b int) bool { return prefixes[a].start < prefixes[b].start })
	// Enclosing definitions, innermost first via the precomputed containment tree.
	var anc []string
	cur := i
	seen := map[int]bool{i: true}
	for {
		p := parentIdx[cur]
		if p < 0 || seen[p] {
			break
		}
		seen[p] = true
		anc = append(anc, syms[p].Name)
		cur = p
	}
	parts := make([]string, 0, len(prefixes)+len(anc)+1)
	for _, p := range prefixes {
		parts = append(parts, p.name)
	}
	for k := len(anc) - 1; k >= 0; k-- { // reverse to outermost-first
		parts = append(parts, anc[k])
	}
	parts = append(parts, syms[i].Name)
	return strings.Join(parts, ".")
}

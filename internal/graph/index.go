package graph

import (
	"context"
	"database/sql"
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
	hash := sha256hex(src)
	now := e.clk.Now().UnixMilli()
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.writeFileGraph(ctx, c, worktree, path, lang, res, hash, mtime, size, now)
	})
}

func (e *Engine) writeFileGraph(ctx context.Context, c store.Conn, worktree, path, lang string,
	res parse.Result, hash string, mtime, size, now int64) error {
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
	case err != nil:
		return err
	default:
		// Drop the file's FTS rows before deleting nodes (FTS5 is not linked).
		if _, err := c.ExecContext(ctx,
			`DELETE FROM nodes_fts WHERE node_id IN (SELECT id FROM nodes WHERE file_id=?)`,
			fileID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM nodes WHERE file_id=?`, fileID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM edges WHERE file_id=?`, fileID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE files SET language=?,content_hash=?,mtime=?,size=?,last_indexed_at=? WHERE id=?`,
			lang, hash, mtime, size, now, fileID); err != nil {
			return err
		}
	}

	// Pass 1: assign ids and byte spans (in lockstep with res.Symbols) so a
	// symbol's qualified name can be derived from the full structural context of
	// the file before any row is written.
	spans := make([]span, len(res.Symbols))
	for i, s := range res.Symbols {
		spans[i] = span{id: ids.New(), start: s.StartByte, end: s.EndByte}
	}
	// Pass 2: insert nodes with structural qualified names + FTS content.
	for i, s := range res.Symbols {
		qn := qualifiedName(spans, res.Symbols, i)
		if _, err := c.ExecContext(ctx,
			`INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,
			   start_byte,end_byte,start_line,end_line)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			spans[i].id, fileID, s.Kind, s.Name, qn, "", // signature: reserved for a future version
			int64(s.StartByte), int64(s.EndByte), int64(s.StartLine), int64(s.EndLine)); err != nil {
			return err
		}
		// Store identifier-split components alongside the raw name so a query for
		// a single component word ("Renderer") matches a PascalCase symbol
		// ("MessageRenderer"); the default unicode61 tokenizer cannot split
		// camelCase on its own. The dotted qualified name also feeds the FTS so a
		// namespace/class word ("Domain") surfaces its members.
		if _, err := c.ExecContext(ctx,
			`INSERT INTO nodes_fts(node_id,name,qualified_name) VALUES (?,?,?)`,
			spans[i].id, ftsText(s.Name), ftsText(qn)); err != nil {
			return err
		}
	}

	// Insert edges, attributing each to the innermost enclosing node.
	for _, ed := range res.Edges {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,kind,file_id,line)
			 VALUES (?,?,?,?,?,?,?)`,
			ids.New(), nullStr(enclosing(spans, ed.StartByte)), nil, ed.DstName, ed.Kind,
			fileID, int64(ed.Line)); err != nil {
			return err
		}
	}

	// Structural containment edges: each symbol nested inside another emits a
	// `contains` edge (parent -> child). Both endpoints are nodes in this same
	// file, so they are linked directly by id (no name resolution). spans is in
	// lockstep with res.Symbols, so spans[i] corresponds to res.Symbols[i].
	for i, child := range spans {
		pid := containingParent(spans, child)
		if pid == "" {
			continue
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,kind,file_id,line)
			 VALUES (?,?,?,?,?,?,?)`,
			ids.New(), pid, child.id, "", "contains",
			fileID, int64(res.Symbols[i].StartLine)); err != nil {
			return err
		}
	}
	return nil
}

// qualifiedName builds a dotted structural name for the symbol at syms[i]. It
// joins, outermost-first: any namespace that governs the symbol but does not
// span-contain it (a C# file-scoped namespace `namespace N;` precedes its types
// without enclosing them), then the chain of enclosing definitions (block
// namespaces, classes) found by byte containment, then the symbol's own name.
// spans is in lockstep with syms (spans[i] is syms[i]).
func qualifiedName(spans []span, syms []parse.Symbol, i int) string {
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
	for j := range spans {
		if j == i || syms[j].Kind != "namespace" {
			continue
		}
		if spans[j].start <= self.start && !contains(spans[j], self) {
			prefixes = append(prefixes, pre{spans[j].start, syms[j].Name})
		}
	}
	sort.Slice(prefixes, func(a, b int) bool { return prefixes[a].start < prefixes[b].start })
	// Enclosing definitions, innermost first via the containment tree.
	var anc []string
	cur := self
	seen := map[string]bool{self.id: true}
	for {
		pid := containingParent(spans, cur)
		if pid == "" || seen[pid] {
			break
		}
		seen[pid] = true
		for k := range spans {
			if spans[k].id == pid {
				anc = append(anc, syms[k].Name)
				cur = spans[k]
				break
			}
		}
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

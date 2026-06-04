package graph

import (
	"context"
	"database/sql"
	"errors"

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

	// Insert nodes; remember each node's byte span for edge attribution.
	spans := make([]span, 0, len(res.Symbols))
	for _, s := range res.Symbols {
		nid := ids.New()
		qn := s.QualifiedName
		if qn == "" {
			qn = s.Name
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,
			   start_byte,end_byte,start_line,end_line)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			nid, fileID, s.Kind, s.Name, qn, "", // signature: reserved for a future version
			int64(s.StartByte), int64(s.EndByte), int64(s.StartLine), int64(s.EndLine)); err != nil {
			return err
		}
		// Store identifier-split components alongside the raw name so a query for
		// a single component word ("Renderer") matches a PascalCase symbol
		// ("MessageRenderer"); the default unicode61 tokenizer cannot split
		// camelCase on its own.
		if _, err := c.ExecContext(ctx,
			`INSERT INTO nodes_fts(node_id,name,qualified_name) VALUES (?,?,?)`,
			nid, ftsText(s.Name), ftsText(qn)); err != nil {
			return err
		}
		spans = append(spans, span{id: nid, start: s.StartByte, end: s.EndByte})
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

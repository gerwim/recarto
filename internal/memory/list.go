package memory

import "context"

// ListFilter selects stored memories for read-only listing (markdown export,
// status counts, and the MCP list_memory tool in Phase 1C-2).
type ListFilter struct {
	Kind   Kind   // "" → any kind
	Branch string // "" → all branches; else this branch OR repo-global (NULL)
	Status string // "" → "active"
	Limit  int    // 0 → no limit
}

// List returns stored records (with refs) matching f, ordered newest-updated
// first (then id) for stable, deterministic output.
func (e *Engine) List(ctx context.Context, f ListFilter) ([]*Record, error) {
	status := f.Status
	if status == "" {
		status = "active"
	}
	q := selectMemoriesSQL + ` WHERE status = ?`
	args := []any{status}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(f.Kind))
	}
	if f.Branch != "" {
		q += ` AND (branch IS NULL OR branch = ?)`
		args = append(args, f.Branch)
	}
	q += ` ORDER BY updated_at DESC, id ASC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := e.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := e.loadRefs(ctx, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

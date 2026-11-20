package store

import (
	"context"
	"fmt"
	"strings"
)

// FTSTables lists every FTS5 virtual table in the store, in name order. It reads
// sqlite_master rather than a hard-coded list so a table added by a later
// migration is covered automatically. fts5vocab tables (USING fts5vocab(...)) are
// read-only views over another FTS5 table and are excluded: they cannot be
// optimized and have no storage of their own.
func (s *Store) FTSTables(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND sql LIKE 'CREATE VIRTUAL TABLE%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, err
		}
		if isFTS5DDL(ddl) {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// isFTS5DDL reports whether a CREATE VIRTUAL TABLE statement uses the fts5 module
// itself (not fts5vocab or another module whose name merely starts with "fts5").
func isFTS5DDL(ddl string) bool {
	lower := strings.ToLower(ddl)
	i := strings.Index(lower, " using ")
	if i < 0 {
		return false
	}
	module := strings.TrimSpace(lower[i+len(" using "):])
	if j := strings.IndexAny(module, "( \t\r\n"); j >= 0 {
		module = module[:j]
	}
	return module == "fts5"
}

// OptimizeFTS runs FTS5's 'optimize' command on every FTS5 table, merging each
// index's accumulated b-tree segments into one. Incremental inserts and deletes
// leave many small segments plus delete markers behind, so a long-lived store's
// FTS indexes grow well beyond their live content; optimize rewrites them
// compactly. The pages it frees stay in the file until a following VACUUM returns
// them to the OS. Each table is optimized in its own write transaction so the WAL
// is bounded by the largest index rather than the sum of all of them. It returns
// the tables optimized.
func (s *Store) OptimizeFTS(ctx context.Context) ([]string, error) {
	tables, err := s.FTSTables(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		// The table name comes from sqlite_master, not user input; quote it as an
		// identifier anyway. The command column shares the table's name.
		q := quoteIdent(t)
		stmt := fmt.Sprintf(`INSERT INTO %s(%s) VALUES ('optimize')`, q, q)
		if err := s.WriteTx(ctx, func(c Conn) error {
			_, err := c.ExecContext(ctx, stmt)
			return err
		}); err != nil {
			return nil, fmt.Errorf("optimize %s: %w", t, err)
		}
		_ = s.Checkpoint(ctx)
	}
	return tables, nil
}

// quoteIdent quotes an SQLite identifier with double quotes, doubling any
// embedded double quote.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

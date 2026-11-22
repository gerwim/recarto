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

// isFTS5DDL reads the module from the leading DDL tokens because SQLite keeps
// line breaks, comments and quoted names in stored schema statements.
func isFTS5DDL(ddl string) bool {
	for _, want := range []string{"CREATE", "VIRTUAL", "TABLE"} {
		word, rest, ok := sqlWord(ddl)
		if !ok || !strings.EqualFold(word, want) {
			return false
		}
		ddl = rest
	}
	_, ddl, ok := sqlWord(ddl)
	if !ok {
		return false
	}
	word, ddl, ok := sqlWord(ddl)
	if !ok || !strings.EqualFold(word, "USING") {
		return false
	}
	module, _, ok := sqlWord(ddl)
	return ok && strings.EqualFold(module, "fts5")
}

func sqlWord(s string) (word, rest string, ok bool) {
	for {
		if len(s) == 0 {
			return "", s, false
		}
		switch {
		case isSQLSpace(s[0]):
			s = s[1:]
		case strings.HasPrefix(s, "\uFEFF"):
			s = s[len("\uFEFF"):]
		case strings.HasPrefix(s, "--"):
			if end := strings.IndexByte(s[2:], '\n'); end >= 0 {
				s = s[end+3:]
			} else {
				return "", "", false
			}
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s[2:], "*/")
			if end < 0 {
				return "", "", false
			}
			s = s[end+4:]
		default:
			goto readWord
		}
	}

readWord:
	if len(s) == 0 {
		return "", s, false
	}
	if s[0] == '"' || s[0] == '`' || s[0] == '\'' {
		quote := s[0]
		var unquoted strings.Builder
		for i := 1; i < len(s); {
			if s[i] == quote {
				if i+1 < len(s) && s[i+1] == quote {
					unquoted.WriteByte(quote)
					i += 2
					continue
				}
				return unquoted.String(), s[i+1:], true
			}
			unquoted.WriteByte(s[i])
			i++
		}
		return "", "", false
	}
	if s[0] == '[' {
		end := strings.IndexByte(s[1:], ']')
		if end < 0 {
			return "", "", false
		}
		return s[1 : end+1], s[end+2:], true
	}

	i := 0
	for i < len(s) && isSQLWordByte(s[i]) {
		i++
	}
	if i == 0 {
		return "", s, false
	}
	return s[:i], s[i:], true
}

func isSQLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
}

func isSQLWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_' || b == '$' || b >= 0x80
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
		// Optimization is committed; a later checkpoint can handle remaining WAL frames.
		_ = s.Checkpoint(ctx)
	}
	return tables, nil
}

// quoteIdent quotes an SQLite identifier with double quotes, doubling any
// embedded double quote.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

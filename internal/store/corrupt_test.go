package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sqlitedrv "modernc.org/sqlite"
)

func TestIsCorruptClassifiesMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// The reported failure, verbatim: modernc formats "<message> (<code>)".
		{"corrupt 11", errors.New("database disk image is malformed (11)"), true},
		// Extended corrupt codes: 267 = CORRUPT_VTAB (the FTS5 write path, which is
		// what record_memory hits), 523 = CORRUPT_SEQUENCE, 779 = CORRUPT_INDEX.
		{"corrupt vtab 267", errors.New("database disk image is malformed (267)"), true},
		{"corrupt sequence 523", errors.New("database disk image is malformed (523)"), true},
		{"corrupt index 779", errors.New("database disk image is malformed (779)"), true},
		{"notadb 26", errors.New("file is not a database (26)"), true},
		{"notadb driver text", errors.New("File opened that is not a database file (SQLITE_NOTADB)"), true},
		{"malformed schema", errors.New("malformed database schema (nodes_fts)"), true},
		{"wrapped", fmt.Errorf("migrate: %w", errors.New("database disk image is malformed (11)")), true},
		{"failed integrity check", &errCorruptCheck{detail: "Tree 30 page 201"}, true},
		{"busy", errors.New("database is locked (5)"), false},
		{"busy code name", errors.New("SQLITE_BUSY"), false},
		{"constraint", errors.New("constraint failed (19)"), false},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCorrupt(tc.err); got != tc.want {
				t.Fatalf("IsCorrupt(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsCorruptUsesDriverCode proves classification goes through the driver's
// structured result code (errors.As on *sqlite.Error), not only its message: a
// real SQLITE_NOTADB from opening a non-database file must classify as corrupt,
// and a real non-corrupt driver error must not.
func TestIsCorruptUsesDriverCode(t *testing.T) {
	dir := t.TempDir()
	notDB := filepath.Join(dir, "not.db")
	if err := os.WriteFile(notDB, []byte("this is definitely not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+notDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Ping()
	if err == nil {
		t.Fatal("expected opening a non-database file to fail")
	}
	var se *sqlitedrv.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected a *sqlite.Error, got %T: %v", err, err)
	}
	if se.Code()&0xff != sqliteNotADB {
		t.Fatalf("code = %d, want SQLITE_NOTADB (%d)", se.Code(), sqliteNotADB)
	}
	if !IsCorrupt(err) {
		t.Fatalf("IsCorrupt(%v) = false, want true", err)
	}

	// A genuine constraint violation carries a driver code too and must not be
	// mistaken for corruption.
	st, err := Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	werr := st.WriteTx(ctx, func(c Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at)
			 VALUES ('x','not-a-valid-kind','t','b','tb',1,1)`)
		return err
	})
	if werr == nil {
		t.Fatal("expected the CHECK constraint to reject an invalid kind")
	}
	if IsCorrupt(werr) {
		t.Fatalf("IsCorrupt(%v) = true, want false", werr)
	}
}

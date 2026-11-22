package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"testing"
)

// MigrateWithGraphWipe applies the shipped migrations plus one more whose only
// step is wipeCodeGraph: the shape a graph-wiping migration takes.
func MigrateWithGraphWipe(db *sql.DB) error {
	return migrateOnce(context.Background(), db,
		append(slices.Clone(migrations), migration{run: wipeCodeGraph}))
}

// SchemaSnapshot returns the schema objects without their unstable root pages.
func SchemaSnapshot(t testing.TB, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close schema rows: %v", err)
		}
	}()
	var snapshot []string
	for rows.Next() {
		var typ, name, table, sqlText string
		if err := rows.Scan(&typ, &name, &table, &sqlText); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, fmt.Sprintf("%s|%s|%s|%s", typ, name, table, sqlText))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(snapshot)
	return snapshot
}

package syncserver

import "database/sql"

// EnsureSyncSchema creates the server-only bookkeeping table that indexes
// every accepted change by a monotonic per-repo sequence number, so a client
// can page "everything after seq N". It is deliberately NOT part of
// internal/store's client migration list — sync_index is a server-side
// concern a client store never needs — so it is created here, idempotently,
// the first time a per-repo-key store is opened by the server.
//
// The seq counter itself lives in the store's existing `counters` table
// (scope="sync", metric="seq"), reusing the client schema rather than adding
// another server-only table for a single integer.
func EnsureSyncSchema(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sync_index (
  memory_id  TEXT PRIMARY KEY,
  seq        INTEGER NOT NULL,
  deleted    INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sync_index_seq ON sync_index(seq);
`)
	return err
}
